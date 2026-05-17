package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/parisxmas/OxiMail/internal/notifier"
)

// Message is one stored message. The RFC 5322 body lives in the blob
// store under BodyBlob; the document holds only metadata and the IMAP
// flags (a small document, so a flag change is a cheap update).
type Message struct {
	ID        uint64 `json:"_id,omitempty"`
	MailboxID uint64 `json:"mailbox_id"`
	AccountID uint64 `json:"account_id"` // denormalized for account-wide ops (e.g. cascade delete)
	UID       uint32 `json:"uid"`        // IMAP UID, unique and increasing within the mailbox
	BodyBlob  string `json:"body_blob"`  // blob-store key for the raw RFC 5322 body
	SizeBytes int64  `json:"size_bytes"`

	Flags        []string `json:"flags"`         // IMAP flags: \Seen, \Flagged, \Answered, \Deleted, \Draft
	InternalDate string   `json:"internal_date"` // IMAP INTERNALDATE
	MessageID    string   `json:"message_id"`    // RFC 5322 Message-ID header
	Subject      string   `json:"subject"`
	FromAddr     string   `json:"from_addr"`
	ReceivedAt   string   `json:"received_at"`
	// ModSeq is the per-mailbox mod-sequence value (RFC 7162). Bumped
	// to a fresh NextModSeq on every flag change and on append.
	ModSeq uint64 `json:"modseq,omitempty"`
}

// IncomingMessage is the input to AppendMessage: the raw bytes plus the
// header fields the SMTP/IMAP layer has already parsed out of them.
type IncomingMessage struct {
	Raw          []byte    // the complete RFC 5322 message
	MessageID    string    // Message-ID header
	Subject      string    // Subject header
	FromAddr     string    // From header (address only)
	InternalDate time.Time // IMAP INTERNALDATE; the zero value means "now"
	Flags        []string  // initial flags (usually empty)
}

// newBlobKey returns a fresh, unique key for a body blob. Bodies are not
// content-addressed: two identical messages get two blobs, so deleting
// one never affects the other (no reference counting needed).
func newBlobKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generate blob key: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// AppendMessage stores a message into a mailbox: it writes the body to
// the blob store, allocates an IMAP UID, inserts the metadata document,
// and updates the account's used-bytes counter.
//
// Ordering is chosen to minimize residue on a partial failure: a UID is
// allocated first (a burned UID is harmless — IMAP tolerates gaps), then
// the body blob, then the document; if the document insert fails the
// blob is removed, so no orphan is left.
func (s *Store) AppendMessage(mailboxID uint64, in IncomingMessage) (*Message, error) {
	mb, err := s.GetMailbox(mailboxID)
	if err != nil {
		return nil, fmt.Errorf("store: append to mailbox %d: %w", mailboxID, err)
	}

	uid, err := s.NextUID(mailboxID)
	if err != nil {
		return nil, err
	}
	modSeq, err := s.NextModSeq(mailboxID)
	if err != nil {
		return nil, err
	}

	key, err := newBlobKey()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.PutObject(BlobBucket, key, in.Raw, "message/rfc822", nil); err != nil {
		return nil, fmt.Errorf("store: append to mailbox %d: store body: %w", mailboxID, err)
	}
	if err := s.blobAddRef(key); err != nil {
		_ = s.db.DeleteObject(BlobBucket, key)
		return nil, err
	}

	internal := in.InternalDate
	if internal.IsZero() {
		internal = time.Now()
	}
	msg := &Message{
		MailboxID:    mailboxID,
		AccountID:    mb.AccountID,
		UID:          uid,
		BodyBlob:     key,
		SizeBytes:    int64(len(in.Raw)),
		Flags:        in.Flags,
		InternalDate: internal.UTC().Format(time.RFC3339),
		MessageID:    in.MessageID,
		Subject:      in.Subject,
		FromAddr:     in.FromAddr,
		ReceivedAt:   nowRFC3339(),
		ModSeq:       modSeq,
	}
	if msg.Flags == nil {
		msg.Flags = []string{}
	}
	doc, err := encodeDoc(msg)
	if err != nil {
		_ = s.db.DeleteObject(BlobBucket, key)
		return nil, err
	}
	resp, err := s.db.Insert(CollMessages, doc)
	if err != nil {
		_ = s.db.DeleteObject(BlobBucket, key) // no orphan blob
		return nil, fmt.Errorf("store: append to mailbox %d: insert message: %w", mailboxID, err)
	}
	if msg.ID, err = insertedID(resp); err != nil {
		_ = s.db.DeleteObject(BlobBucket, key)
		return nil, err
	}

	// Account usage accounting — best-effort: the message is already
	// delivered, so a counter that drifts is not worth failing the
	// append over. find_and_modify keeps the increment atomic.
	_, _ = s.db.FindAndModify(
		CollAccounts,
		map[string]any{"_id": mb.AccountID},
		map[string]any{"$inc": map[string]any{"used_bytes": msg.SizeBytes}},
	)
	// Wake any IMAP sessions IDLE'ing on this mailbox so they emit an
	// unsolicited EXISTS for the new arrival.
	notifier.Default.Notify(mailboxID)
	return msg, nil
}

// Deliver is the common "deliver to INBOX" path.
func (s *Store) Deliver(accountID uint64, in IncomingMessage) (*Message, error) {
	return s.DeliverTo(accountID, "INBOX", in)
}

// DeliverTo files an inbound message into the named folder of an
// account — INBOX in the common case, "Junk" for quarantined mail,
// any other default folder for special-case routing. The default
// mailboxes are created on first delivery if they do not exist yet,
// so an account is reachable the moment it is created without a
// separate provisioning step.
func (s *Store) DeliverTo(accountID uint64, folder string, in IncomingMessage) (*Message, error) {
	mb, err := s.GetMailboxByName(accountID, folder)
	if errors.Is(err, ErrNotFound) {
		if err := s.EnsureDefaultMailboxes(accountID); err != nil {
			return nil, fmt.Errorf("store: deliver to account %d (%s): %w", accountID, folder, err)
		}
		mb, err = s.GetMailboxByName(accountID, folder)
	}
	if err != nil {
		return nil, fmt.Errorf("store: deliver to account %d (%s): %w", accountID, folder, err)
	}
	return s.AppendMessage(mb.ID, in)
}

// GetMessage looks a message up by its OxiDB id.
func (s *Store) GetMessage(id uint64) (*Message, error) {
	m, err := s.db.FindOne(CollMessages, map[string]any{"_id": id})
	if err != nil {
		return nil, fmt.Errorf("store: get message %d: %w", id, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var msg Message
	if err := decodeDoc(m, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ListMessages returns a mailbox's messages, ordered by ascending UID.
func (s *Store) ListMessages(mailboxID uint64) ([]Message, error) {
	rows, err := s.db.Find(CollMessages, map[string]any{"mailbox_id": mailboxID}, nil)
	if err != nil {
		return nil, fmt.Errorf("store: list messages in mailbox %d: %w", mailboxID, err)
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		var msg Message
		if err := decodeDoc(r, &msg); err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// FetchBody reads the raw RFC 5322 body of a message from the blob store.
func (s *Store) FetchBody(m *Message) ([]byte, error) {
	body, _, err := s.db.GetObject(BlobBucket, m.BodyBlob)
	if err != nil {
		return nil, fmt.Errorf("store: fetch body of message %d (blob %q): %w", m.ID, m.BodyBlob, err)
	}
	return body, nil
}

// SetFlags replaces a message's IMAP flags wholesale (IMAP STORE FLAGS).
func (s *Store) SetFlags(messageID uint64, flags []string) error {
	if flags == nil {
		flags = []string{}
	}
	return s.modifyFlags(messageID, func(set map[string]any) {
		set["flags"] = flags
	})
}

// AddFlags adds flags to a message (IMAP STORE +FLAGS).
func (s *Store) AddFlags(messageID uint64, flags ...string) error {
	msg, err := s.GetMessage(messageID)
	if err != nil {
		return err
	}
	merged := unionStrings(msg.Flags, flags)
	return s.modifyFlags(messageID, func(set map[string]any) {
		set["flags"] = merged
	})
}

// RemoveFlags removes flags from a message (IMAP STORE -FLAGS).
func (s *Store) RemoveFlags(messageID uint64, flags ...string) error {
	msg, err := s.GetMessage(messageID)
	if err != nil {
		return err
	}
	pruned := minusStrings(msg.Flags, flags)
	return s.modifyFlags(messageID, func(set map[string]any) {
		set["flags"] = pruned
	})
}

// modifyFlags applies a flag update atomically via find_and_modify
// AND stamps a fresh mod-sequence (RFC 7162) so concurrent STORE
// commands on the same message cannot lose a change and CONDSTORE
// clients see every transition reflected in MODSEQ.
//
// We use $set with the post-mutation flag list (computed by the
// caller) rather than $addToSet / $pull so that the new modseq and
// the new flag value land in one find_and_modify call. OxiDB does
// not promise atomicity across multiple operators in a single
// document update.
func (s *Store) modifyFlags(messageID uint64, build func(set map[string]any)) error {
	msg, err := s.GetMessage(messageID)
	if err != nil {
		return err
	}
	modSeq, err := s.NextModSeq(msg.MailboxID)
	if err != nil {
		return err
	}
	set := map[string]any{"modseq": modSeq}
	build(set)
	doc, err := s.db.FindAndModify(
		CollMessages,
		map[string]any{"_id": messageID},
		map[string]any{"$set": set},
	)
	if err != nil {
		return fmt.Errorf("store: modify flags of message %d: %w", messageID, err)
	}
	// Wake IMAP sessions IDLE'ing on this mailbox so they can
	// surface the flag change (cross-connection STORE → FETCH
	// FLAGS unilateral update).
	notifier.Default.Notify(msg.MailboxID)
	if doc == nil {
		return ErrNotFound
	}
	return nil
}

// unionStrings returns a + b with duplicates removed (a's order
// preserved, b's new entries appended).
func unionStrings(a, b []string) []string {
	out := append([]string(nil), a...)
	seen := make(map[string]bool, len(out))
	for _, v := range out {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// minusStrings returns a with every element of b removed.
func minusStrings(a, b []string) []string {
	drop := make(map[string]bool, len(b))
	for _, v := range b {
		drop[v] = true
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if !drop[v] {
			out = append(out, v)
		}
	}
	return out
}

// MoveMessage moves a message into another of the same account's
// mailboxes, allocating a fresh UID in the destination (IMAP UIDs are
// per-mailbox). The body blob is untouched — only the document's
// mailbox and UID change. It backs the webmail "move" action and, in
// time, IMAP MOVE.
func (s *Store) MoveMessage(messageID, destMailboxID uint64) (*Message, error) {
	msg, err := s.GetMessage(messageID)
	if err != nil {
		return nil, err
	}
	if msg.MailboxID == destMailboxID {
		return msg, nil // already there
	}
	dest, err := s.GetMailbox(destMailboxID)
	if err != nil {
		return nil, err
	}
	if dest.AccountID != msg.AccountID {
		return nil, fmt.Errorf("store: move message %d: destination mailbox belongs to another account", messageID)
	}

	uid, err := s.NextUID(destMailboxID)
	if err != nil {
		return nil, err
	}
	modSeq, err := s.NextModSeq(destMailboxID)
	if err != nil {
		return nil, err
	}
	doc, err := s.db.FindAndModify(
		CollMessages,
		map[string]any{"_id": messageID},
		map[string]any{"$set": map[string]any{
			"mailbox_id": destMailboxID,
			"uid":        uid,
			"modseq":     modSeq,
		}},
	)
	if err != nil {
		return nil, fmt.Errorf("store: move message %d: %w", messageID, err)
	}
	if doc == nil {
		return nil, ErrNotFound
	}
	// Notify both ends: the destination gained a message, the source
	// lost one.
	notifier.Default.Notify(destMailboxID)
	notifier.Default.Notify(msg.MailboxID)
	msg.MailboxID = destMailboxID
	msg.UID = uid
	msg.ModSeq = modSeq
	return msg, nil
}

// CopyMessage copies a message into another of the same account's
// mailboxes. The body is NOT duplicated: the new metadata document
// points at the source's blob key and the blob's refcount is bumped
// via blobBumpRef. IMAP flags, internal date, and header metadata
// carry over; the destination gets a fresh UID and mod-sequence.
//
// The blob is only physically removed when the LAST referrer is
// deleted (see DeleteMessage → blobDropRef).
func (s *Store) CopyMessage(messageID, destMailboxID uint64) (*Message, error) {
	src, err := s.GetMessage(messageID)
	if err != nil {
		return nil, err
	}
	dest, err := s.GetMailbox(destMailboxID)
	if err != nil {
		return nil, err
	}
	if dest.AccountID != src.AccountID {
		return nil, fmt.Errorf("store: copy message %d: destination mailbox belongs to another account", messageID)
	}

	// Bump the source blob's refcount before we insert the new
	// metadata doc. On any later failure we drop it back down.
	if _, err := s.blobBumpRef(src.BodyBlob); err != nil {
		return nil, fmt.Errorf("store: copy message %d: %w", messageID, err)
	}
	uid, err := s.NextUID(destMailboxID)
	if err != nil {
		_, _ = s.blobDropRef(src.BodyBlob)
		return nil, err
	}
	modSeq, err := s.NextModSeq(destMailboxID)
	if err != nil {
		_, _ = s.blobDropRef(src.BodyBlob)
		return nil, err
	}

	dst := &Message{
		MailboxID:    destMailboxID,
		AccountID:    src.AccountID,
		UID:          uid,
		BodyBlob:     src.BodyBlob,
		SizeBytes:    src.SizeBytes,
		Flags:        append([]string(nil), src.Flags...),
		InternalDate: src.InternalDate,
		MessageID:    src.MessageID,
		Subject:      src.Subject,
		FromAddr:     src.FromAddr,
		ReceivedAt:   nowRFC3339(),
		ModSeq:       modSeq,
	}
	if dst.Flags == nil {
		dst.Flags = []string{}
	}
	doc, err := encodeDoc(dst)
	if err != nil {
		_, _ = s.blobDropRef(src.BodyBlob)
		return nil, err
	}
	resp, err := s.db.Insert(CollMessages, doc)
	if err != nil {
		_, _ = s.blobDropRef(src.BodyBlob)
		return nil, fmt.Errorf("store: copy message %d: insert: %w", messageID, err)
	}
	if dst.ID, err = insertedID(resp); err != nil {
		_, _ = s.blobDropRef(src.BodyBlob)
		return nil, err
	}
	notifier.Default.Notify(destMailboxID)
	return dst, nil
}

// RestoreMessage inserts a message at its original UID + ModSeq +
// Flags and writes the body to the blob bucket (when newBlob is
// true; otherwise the blob is assumed to already be present from a
// previous RestoreMessage call sharing the same key). Used by
// `oximailctl restore` so a backed-up message lands at exactly the
// state the backup captured, not at fresh allocator values.
//
// The _id field on msg is ignored; OxiDB assigns a fresh one. The
// account's used_bytes counter is NOT updated — callers should run
// a fresh quota accounting pass after a restore.
func (s *Store) RestoreMessage(msg *Message, body []byte, newBlob bool) (*Message, error) {
	clone := *msg
	clone.ID = 0
	if clone.Flags == nil {
		clone.Flags = []string{}
	}
	if clone.ReceivedAt == "" {
		clone.ReceivedAt = nowRFC3339()
	}
	if newBlob {
		if _, err := s.db.PutObject(BlobBucket, clone.BodyBlob, body, "message/rfc822", nil); err != nil {
			return nil, fmt.Errorf("store: restore message: write blob %q: %w", clone.BodyBlob, err)
		}
		if err := s.blobAddRef(clone.BodyBlob); err != nil {
			_ = s.db.DeleteObject(BlobBucket, clone.BodyBlob)
			return nil, err
		}
	} else {
		// Blob already exists from a prior RestoreMessage (the
		// backup carried two messages pointing at the same key);
		// just bump the refcount.
		if _, err := s.blobBumpRef(clone.BodyBlob); err != nil {
			return nil, err
		}
	}
	doc, err := encodeDoc(&clone)
	if err != nil {
		_, _ = s.blobDropRef(clone.BodyBlob)
		return nil, err
	}
	resp, err := s.db.Insert(CollMessages, doc)
	if err != nil {
		_, _ = s.blobDropRef(clone.BodyBlob)
		return nil, fmt.Errorf("store: restore message: insert: %w", err)
	}
	if clone.ID, err = insertedID(resp); err != nil {
		_, _ = s.blobDropRef(clone.BodyBlob)
		return nil, err
	}
	return &clone, nil
}

// DeleteMessage removes a message: the metadata document first, then its
// body blob (an orphaned blob is just wasted disk; a document pointing
// at a missing blob would be a broken read). The account's used-bytes
// counter is decremented best-effort, and the (UID, mod-seq) pair is
// appended to the expunge log so QRESYNC clients can later resync.
func (s *Store) DeleteMessage(id uint64) error {
	msg, err := s.GetMessage(id)
	if err != nil {
		return err
	}
	// RFC 7162 §2.1.2: EXPUNGE bumps the mailbox mod-seq. The bumped
	// value is what we record on the expunge log so QRESYNC's
	// "since modseq M" comparison is exact.
	modSeq, err := s.NextModSeq(msg.MailboxID)
	if err != nil {
		return err
	}
	if _, err := s.db.Delete(CollMessages, map[string]any{"_id": id}); err != nil {
		return fmt.Errorf("store: delete message %d: %w", id, err)
	}
	// Drop our refcount on the body; the blob is physically removed
	// only when this was the last referrer (the common case — copies
	// are rare). A failure here is logged but not fatal: the metadata
	// doc is already gone and the blob is, at worst, leaked disk.
	if _, err := s.blobDropRef(msg.BodyBlob); err != nil {
		return fmt.Errorf("store: delete message %d: %w", id, err)
	}
	if err := s.recordExpunge(msg.MailboxID, msg.UID, modSeq); err != nil {
		// A log gap means QRESYNC clients won't be told this UID
		// went away — they'll discover it on their own. Log and
		// move on; do not fail the delete.
		_ = err
	}
	_, _ = s.db.FindAndModify(
		CollAccounts,
		map[string]any{"_id": msg.AccountID},
		map[string]any{"$inc": map[string]any{"used_bytes": -msg.SizeBytes}},
	)
	notifier.Default.Notify(msg.MailboxID)
	return nil
}
