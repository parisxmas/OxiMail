package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
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
// content-addressed: two identical deliveries get two blobs. A blob is
// only shared through CopyMessage, which is what the refcounts in
// blobrefs.go track.
func newBlobKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generate blob key: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// CheckQuota returns ErrQuotaExceeded if adding size bytes would take
// the account past its quota (0 means unlimited).
//
// This is a read followed later by the caller's write, not a
// reservation, so concurrent deliveries can each pass the check and
// overshoot the quota by up to their combined size. That is accepted:
// the next delivery is refused, and a precise check would put a
// cross-account write on the path of every message.
func (s *Store) CheckQuota(accountID uint64, size int64) error {
	acc, err := s.GetAccountByID(accountID)
	if err != nil {
		return err
	}
	if acc.QuotaBytes > 0 && acc.UsedBytes+size > acc.QuotaBytes {
		return ErrQuotaExceeded
	}
	return nil
}

// addUsedBytes adjusts the account's used-bytes counter. Best-effort:
// the message change has already happened, so a counter that drifts is
// not worth failing it over. find_and_modify keeps the increment atomic.
func (s *Store) addUsedBytes(accountID uint64, delta int64) {
	if delta == 0 {
		return
	}
	_, _ = s.db.FindAndModify(
		CollAccounts,
		map[string]any{"_id": accountID},
		map[string]any{"$inc": map[string]any{"used_bytes": delta}},
	)
}

// assignUID allocates a UID and mod-sequence in a mailbox and runs write,
// which must make the message visible under them, all while holding the
// mailbox's UID lock.
//
// IMAP clients track "everything below UIDNEXT has been seen", so a UID
// must not become visible before a lower one. Holding the lock across
// allocation and write gives that order for everything in this process;
// the slow body upload stays outside it. Writers in another process
// (oximailctl) are not covered, which is why the IMAP refresh still
// accepts a late lower UID.
func (s *Store) assignUID(accountID, mailboxID uint64, write func(uid uint32, modSeq uint64) error) error {
	mu := &s.uidLocks[(accountID*31+mailboxID)%uidLockStripes]
	mu.Lock()
	defer mu.Unlock()
	uid, err := s.NextUID(accountID, mailboxID)
	if err != nil {
		return err
	}
	modSeq, err := s.NextModSeq(accountID, mailboxID)
	if err != nil {
		return err
	}
	return write(uid, modSeq)
}

// AppendMessage stores a message into a mailbox: it checks the quota,
// writes the body to the blob store, allocates an IMAP UID, inserts the
// metadata document, and updates the account's used-bytes counter. It
// returns ErrQuotaExceeded (test with errors.Is) if the message does
// not fit.
//
// The body goes first so that the UID is allocated as late as possible
// (see assignUID). A failure after that point discards the blob and its
// refcount row, so no orphan is left; at worst a UID is burned, and IMAP
// tolerates gaps.
func (s *Store) AppendMessage(accountID, mailboxID uint64, in IncomingMessage) (*Message, error) {
	mb, err := s.GetMailbox(accountID, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("store: append to mailbox %d: %w", mailboxID, err)
	}
	if err := s.CheckQuota(mb.AccountID, int64(len(in.Raw))); err != nil {
		return nil, fmt.Errorf("store: append to mailbox %d: %w", mailboxID, err)
	}

	key, err := newBlobKey()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.PutObject(BlobBucket, key, in.Raw, "message/rfc822", nil); err != nil {
		return nil, fmt.Errorf("store: append to mailbox %d: store body: %w", mailboxID, err)
	}
	if err := s.blobAddRef(key); err != nil {
		s.discardBlob(key)
		return nil, err
	}

	internal := in.InternalDate
	if internal.IsZero() {
		internal = time.Now()
	}
	msg := &Message{
		MailboxID:    mailboxID,
		AccountID:    mb.AccountID,
		BodyBlob:     key,
		SizeBytes:    int64(len(in.Raw)),
		Flags:        in.Flags,
		InternalDate: internal.UTC().Format(time.RFC3339),
		MessageID:    in.MessageID,
		Subject:      in.Subject,
		FromAddr:     in.FromAddr,
		ReceivedAt:   nowRFC3339(),
	}
	if msg.Flags == nil {
		msg.Flags = []string{}
	}
	err = s.assignUID(accountID, mailboxID, func(uid uint32, modSeq uint64) error {
		msg.UID, msg.ModSeq = uid, modSeq
		return s.insertMessage(msg)
	})
	if err != nil {
		s.discardBlob(key) // no orphan blob or ref row
		return nil, fmt.Errorf("store: append to mailbox %d: %w", mailboxID, err)
	}

	s.addUsedBytes(mb.AccountID, msg.SizeBytes)
	// Wake any IMAP sessions IDLE'ing on this mailbox so they emit an
	// unsolicited EXISTS for the new arrival.
	notifier.Default.Notify(mailboxID)
	return msg, nil
}

// insertMessage inserts msg's document and records the id OxiDB gave it.
func (s *Store) insertMessage(msg *Message) error {
	doc, err := encodeDoc(msg)
	if err != nil {
		return err
	}
	resp, err := s.db.Insert(MessagesColl(msg.AccountID), doc)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	msg.ID, err = insertedID(resp)
	return err
}

// Deliver is the common "deliver to INBOX" path.
func (s *Store) Deliver(accountID uint64, in IncomingMessage) (*Message, error) {
	return s.DeliverTo(accountID, "INBOX", in)
}

// DeliverTo files an inbound message into the named folder of an
// account — INBOX in the common case, "Junk" for quarantined mail,
// or any folder named by a Sieve `fileinto` rule. Missing folders are
// auto-created on first delivery (the Sieve `mailbox` extension /
// RFC 5490 semantics), so a `fileinto "DMARC reports"` on a fresh
// account works without a separate provisioning step.
func (s *Store) DeliverTo(accountID uint64, folder string, in IncomingMessage) (*Message, error) {
	mb, err := s.GetMailboxByName(accountID, folder)
	if errors.Is(err, ErrNotFound) {
		// Two reasons a folder might be missing on a fresh delivery:
		// (a) the account just got created and the standard mailboxes
		// haven't been materialised yet, or (b) Sieve asked us to file
		// into a user-named folder we've never seen. EnsureDefaultMailboxes
		// covers (a); the explicit CreateMailbox fallback covers (b).
		if err := s.EnsureDefaultMailboxes(accountID); err != nil {
			return nil, fmt.Errorf("store: deliver to account %d (%s): %w", accountID, folder, err)
		}
		mb, err = s.GetMailboxByName(accountID, folder)
		if errors.Is(err, ErrNotFound) {
			if _, cerr := s.CreateMailbox(accountID, folder); cerr != nil {
				return nil, fmt.Errorf("store: deliver to account %d: create folder %q: %w", accountID, folder, cerr)
			}
			mb, err = s.GetMailboxByName(accountID, folder)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("store: deliver to account %d (%s): %w", accountID, folder, err)
	}
	return s.AppendMessage(accountID, mb.ID, in)
}

// GetMessage looks a message up by its OxiDB id within an account's
// collection. The account scope is mandatory: per-account collections
// each have their own _id sequence, so the same numeric id can exist
// in two different accounts' messages collections.
func (s *Store) GetMessage(accountID, id uint64) (*Message, error) {
	msg, _, err := s.getMessageVersioned(accountID, id)
	return msg, err
}

// getMessageVersioned is GetMessage plus the document's `_version`, for
// a compare-and-swap update. The version is nil if the document has none.
func (s *Store) getMessageVersioned(accountID, id uint64) (*Message, any, error) {
	m, err := s.db.FindOne(MessagesColl(accountID), map[string]any{"_id": id})
	if err != nil {
		if isMissingCollection(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("store: get message %d (account %d): %w", id, accountID, err)
	}
	if m == nil {
		return nil, nil, ErrNotFound
	}
	var msg Message
	if err := decodeDoc(m, &msg); err != nil {
		return nil, nil, err
	}
	return &msg, m["_version"], nil
}

// ListMessages returns a mailbox's messages, ordered by ascending UID.
func (s *Store) ListMessages(accountID, mailboxID uint64) ([]Message, error) {
	rows, err := s.db.Find(MessagesColl(accountID), map[string]any{"mailbox_id": mailboxID}, nil)
	if err != nil {
		if isMissingCollection(err) {
			return nil, nil
		}
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
func (s *Store) SetFlags(accountID, messageID uint64, flags []string) error {
	_, err := s.UpdateFlags(accountID, messageID, 0, func([]string) []string { return flags })
	return err
}

// AddFlags adds flags to a message (IMAP STORE +FLAGS).
func (s *Store) AddFlags(accountID, messageID uint64, flags ...string) error {
	_, err := s.UpdateFlags(accountID, messageID, 0, func(cur []string) []string {
		return unionStrings(cur, flags)
	})
	return err
}

// RemoveFlags removes flags from a message (IMAP STORE -FLAGS).
func (s *Store) RemoveFlags(accountID, messageID uint64, flags ...string) error {
	_, err := s.UpdateFlags(accountID, messageID, 0, func(cur []string) []string {
		return minusStrings(cur, flags)
	})
	return err
}

// maxFlagAttempts bounds UpdateFlags' retry loop. Each lost round means
// some other writer's update landed, so this only runs out when that
// many writers hit one message at once.
const maxFlagAttempts = 64

// UpdateFlags applies mutate to a message's stored flags and stamps a
// fresh mod-sequence (RFC 7162), returning the message as stored.
//
// The read and the write are tied together by a compare-and-swap on the
// document's `_version`: find_and_modify only matches if nobody wrote
// the document since the read, otherwise the whole step is redone on
// the newer flags. So concurrent STORE commands on one message cannot
// lose a change, and a message's mod-sequence never moves backwards.
// mutate may run more than once. A lost round burns a mod-sequence,
// which is harmless.
//
// A non-zero unchangedSince makes the update conditional (UNCHANGEDSINCE):
// if the stored mod-sequence is past it, ErrModified is returned and
// nothing changes.
func (s *Store) UpdateFlags(accountID, messageID, unchangedSince uint64, mutate func(flags []string) []string) (*Message, error) {
	for attempt := 0; attempt < maxFlagAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(min(attempt, 10)) * time.Millisecond)
		}
		msg, version, err := s.getMessageVersioned(accountID, messageID)
		if err != nil {
			return nil, err
		}
		if unchangedSince != 0 && msg.ModSeq > unchangedSince {
			return nil, ErrModified
		}
		flags := mutate(msg.Flags)
		if flags == nil {
			flags = []string{}
		}
		modSeq, err := s.NextModSeq(accountID, msg.MailboxID)
		if err != nil {
			return nil, err
		}
		query := map[string]any{"_id": messageID}
		if version != nil {
			query["_version"] = version
		}
		doc, err := s.db.FindAndModify(
			MessagesColl(accountID),
			query,
			map[string]any{"$set": map[string]any{"flags": flags, "modseq": modSeq}},
		)
		if err != nil {
			return nil, fmt.Errorf("store: modify flags of message %d: %w", messageID, err)
		}
		if len(doc) == 0 {
			continue // lost the race, or deleted: the next read tells which
		}
		msg.Flags, msg.ModSeq = flags, modSeq
		// Wake IMAP sessions IDLE'ing on this mailbox so they can
		// surface the flag change (cross-connection STORE → FETCH
		// FLAGS unilateral update).
		notifier.Default.Notify(msg.MailboxID)
		return msg, nil
	}
	return nil, fmt.Errorf("store: modify flags of message %d: too many concurrent updates", messageID)
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
// mailbox and UID change. It backs the webmail "move" action and IMAP
// MOVE. To the source mailbox a move is an expunge, so it is logged like
// one for QRESYNC clients that are offline when it happens.
func (s *Store) MoveMessage(accountID, messageID, destMailboxID uint64) (*Message, error) {
	msg, err := s.GetMessage(accountID, messageID)
	if err != nil {
		return nil, err
	}
	if msg.MailboxID == destMailboxID {
		return msg, nil // already there
	}
	dest, err := s.GetMailbox(accountID, destMailboxID)
	if err != nil {
		return nil, err
	}
	if dest.AccountID != msg.AccountID {
		return nil, fmt.Errorf("store: move message %d: destination mailbox belongs to another account", messageID)
	}

	srcMailbox, srcUID := msg.MailboxID, msg.UID
	srcModSeq, err := s.NextModSeq(accountID, srcMailbox)
	if err != nil {
		return nil, err
	}
	// Log first, as in removeMessage.
	logged := s.logExpunge(accountID, srcMailbox, srcUID, srcModSeq)

	err = s.assignUID(accountID, destMailboxID, func(uid uint32, modSeq uint64) error {
		// Matching on the source mailbox makes a concurrent move of the
		// same message lose cleanly instead of moving it twice.
		doc, err := s.db.FindAndModify(
			MessagesColl(accountID),
			map[string]any{"_id": messageID, "mailbox_id": srcMailbox},
			map[string]any{"$set": map[string]any{
				"mailbox_id": destMailboxID,
				"uid":        uid,
				"modseq":     modSeq,
			}},
		)
		if err != nil {
			return fmt.Errorf("store: move message %d: %w", messageID, err)
		}
		if len(doc) == 0 {
			return ErrNotFound
		}
		msg.UID, msg.ModSeq = uid, modSeq
		return nil
	})
	if err != nil {
		if logged {
			s.unrecordExpunge(accountID, srcMailbox, srcUID, srcModSeq)
		}
		return nil, err
	}
	msg.MailboxID = destMailboxID
	// Notify both ends: the destination gained a message, the source
	// lost one.
	notifier.Default.Notify(destMailboxID)
	notifier.Default.Notify(srcMailbox)
	return msg, nil
}

// CopyMessage copies a message into another of the same account's
// mailboxes. The body is NOT duplicated: the new metadata document
// points at the source's blob key and the blob's refcount is bumped
// via blobBumpRef. IMAP flags, internal date, and header metadata
// carry over; the destination gets a fresh UID and mod-sequence.
//
// Quota is per message document, so a copy counts its full size even
// though the bytes are shared; DeleteMessage gives the same amount back
// for each document. Returns ErrQuotaExceeded if the copy does not fit.
//
// The blob is only physically removed when the LAST referrer is
// deleted (see removeMessage → blobDropRef).
func (s *Store) CopyMessage(accountID, messageID, destMailboxID uint64) (*Message, error) {
	src, err := s.GetMessage(accountID, messageID)
	if err != nil {
		return nil, err
	}
	dest, err := s.GetMailbox(accountID, destMailboxID)
	if err != nil {
		return nil, err
	}
	if dest.AccountID != src.AccountID {
		return nil, fmt.Errorf("store: copy message %d: destination mailbox belongs to another account", messageID)
	}
	if err := s.CheckQuota(src.AccountID, src.SizeBytes); err != nil {
		return nil, fmt.Errorf("store: copy message %d: %w", messageID, err)
	}

	// Bump the source blob's refcount before we insert the new
	// metadata doc. On any later failure we drop it back down.
	if _, err := s.blobBumpRef(src.BodyBlob); err != nil {
		return nil, fmt.Errorf("store: copy message %d: %w", messageID, err)
	}

	dst := &Message{
		MailboxID:    destMailboxID,
		AccountID:    src.AccountID,
		BodyBlob:     src.BodyBlob,
		SizeBytes:    src.SizeBytes,
		Flags:        append([]string(nil), src.Flags...),
		InternalDate: src.InternalDate,
		MessageID:    src.MessageID,
		Subject:      src.Subject,
		FromAddr:     src.FromAddr,
		ReceivedAt:   nowRFC3339(),
	}
	if dst.Flags == nil {
		dst.Flags = []string{}
	}
	err = s.assignUID(accountID, destMailboxID, func(uid uint32, modSeq uint64) error {
		dst.UID, dst.ModSeq = uid, modSeq
		return s.insertMessage(dst)
	})
	if err != nil {
		_, _ = s.blobDropRef(src.BodyBlob)
		return nil, fmt.Errorf("store: copy message %d: %w", messageID, err)
	}
	s.addUsedBytes(src.AccountID, dst.SizeBytes)
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
// RecountUsedBytes after a restore.
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
			s.discardBlob(clone.BodyBlob)
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
	resp, err := s.db.Insert(MessagesColl(clone.AccountID), doc)
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

// DeleteMessage removes a message: see removeMessage for the order. The
// account's used-bytes counter is decremented best-effort.
func (s *Store) DeleteMessage(accountID, id uint64) error {
	msg, err := s.GetMessage(accountID, id)
	if err != nil {
		return err
	}
	// RFC 7162 §2.1.2: EXPUNGE bumps the mailbox mod-seq. The bumped
	// value is what we record on the expunge log so QRESYNC's
	// "since modseq M" comparison is exact.
	modSeq, err := s.NextModSeq(accountID, msg.MailboxID)
	if err != nil {
		return err
	}
	removed, err := s.removeMessage(accountID, msg, modSeq)
	if err != nil {
		return err
	}
	if removed {
		s.addUsedBytes(msg.AccountID, -msg.SizeBytes)
	}
	notifier.Default.Notify(msg.MailboxID)
	return nil
}

// removeMessage is the one way a message document goes away: expunge
// log row, then the document, then the body reference.
//
// The log row comes first so a crash cannot lose it; a crash right after
// it reports a UID as vanished while the message survives, which costs a
// client one re-sync of a message that was on its way out anyway. An
// orderly failure to remove the document withdraws the row. The document
// goes before the blob because an orphaned blob is just wasted disk,
// while a document pointing at a missing blob is a broken read.
//
// The bool reports whether this call removed the document. When a
// concurrent delete got there first it is false and the reference is
// left alone: dropping it again would take the count below the number
// of surviving documents and destroy a body a copy still needs.
func (s *Store) removeMessage(accountID uint64, msg *Message, modSeq uint64) (bool, error) {
	logged := s.logExpunge(accountID, msg.MailboxID, msg.UID, modSeq)
	resp, err := s.db.Delete(MessagesColl(accountID), map[string]any{"_id": msg.ID})
	if err != nil {
		if logged {
			s.unrecordExpunge(accountID, msg.MailboxID, msg.UID, modSeq)
		}
		return false, fmt.Errorf("store: delete message %d: %w", msg.ID, err)
	}
	if n, ok := resp["deleted"].(float64); ok && n == 0 {
		return false, nil
	}
	// Drop our refcount on the body; the blob is physically removed
	// only when this was the last referrer (the common case — copies
	// are rare). A failure here is logged but not fatal: the metadata
	// doc is already gone and the blob is, at worst, leaked disk.
	if _, err := s.blobDropRef(msg.BodyBlob); err != nil {
		log.Printf("store: delete message %d: %v", msg.ID, err)
	}
	return true, nil
}

// logExpunge writes an expunge-log row and reports whether it landed.
// A log gap means QRESYNC clients won't be told this UID went away —
// they'll discover it on their own — so a failure is logged and does
// not stop the removal.
func (s *Store) logExpunge(accountID, mailboxID uint64, uid uint32, modSeq uint64) bool {
	if err := s.recordExpunge(accountID, mailboxID, uid, modSeq); err != nil {
		log.Printf("%v", err)
		return false
	}
	return true
}
