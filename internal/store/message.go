package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
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

	key, err := newBlobKey()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.PutObject(BlobBucket, key, in.Raw, "message/rfc822", nil); err != nil {
		return nil, fmt.Errorf("store: append to mailbox %d: store body: %w", mailboxID, err)
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
	return msg, nil
}

// Deliver files an inbound message into an account's INBOX. It is the
// delivery entry point for the SMTP server (and, later, local alias
// forwarding): callers resolve a recipient address to account IDs with
// ResolveRecipient, then Deliver to each.
//
// The account's default mailboxes are created on first delivery if they
// do not exist yet, so an account is reachable the moment it is created
// without a separate provisioning step.
func (s *Store) Deliver(accountID uint64, in IncomingMessage) (*Message, error) {
	mb, err := s.GetMailboxByName(accountID, "INBOX")
	if errors.Is(err, ErrNotFound) {
		if err := s.EnsureDefaultMailboxes(accountID); err != nil {
			return nil, fmt.Errorf("store: deliver to account %d: %w", accountID, err)
		}
		mb, err = s.GetMailboxByName(accountID, "INBOX")
	}
	if err != nil {
		return nil, fmt.Errorf("store: deliver to account %d: %w", accountID, err)
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
	return s.modifyFlags(messageID, map[string]any{"$set": map[string]any{"flags": flags}})
}

// AddFlags adds flags to a message (IMAP STORE +FLAGS).
func (s *Store) AddFlags(messageID uint64, flags ...string) error {
	for _, f := range flags {
		if err := s.modifyFlags(messageID, map[string]any{"$addToSet": map[string]any{"flags": f}}); err != nil {
			return err
		}
	}
	return nil
}

// RemoveFlags removes flags from a message (IMAP STORE -FLAGS).
func (s *Store) RemoveFlags(messageID uint64, flags ...string) error {
	for _, f := range flags {
		if err := s.modifyFlags(messageID, map[string]any{"$pull": map[string]any{"flags": f}}); err != nil {
			return err
		}
	}
	return nil
}

// modifyFlags applies a flag update atomically via find_and_modify, so
// concurrent STORE commands on the same message cannot lose a change.
func (s *Store) modifyFlags(messageID uint64, update map[string]any) error {
	doc, err := s.db.FindAndModify(CollMessages, map[string]any{"_id": messageID}, update)
	if err != nil {
		return fmt.Errorf("store: modify flags of message %d: %w", messageID, err)
	}
	if doc == nil {
		return ErrNotFound
	}
	return nil
}

// DeleteMessage removes a message: the metadata document first, then its
// body blob (an orphaned blob is just wasted disk; a document pointing
// at a missing blob would be a broken read). The account's used-bytes
// counter is decremented best-effort.
func (s *Store) DeleteMessage(id uint64) error {
	msg, err := s.GetMessage(id)
	if err != nil {
		return err
	}
	if _, err := s.db.Delete(CollMessages, map[string]any{"_id": id}); err != nil {
		return fmt.Errorf("store: delete message %d: %w", id, err)
	}
	if err := s.db.DeleteObject(BlobBucket, msg.BodyBlob); err != nil {
		return fmt.Errorf("store: delete message %d: remove body %q: %w", id, msg.BodyBlob, err)
	}
	_, _ = s.db.FindAndModify(
		CollAccounts,
		map[string]any{"_id": msg.AccountID},
		map[string]any{"$inc": map[string]any{"used_bytes": -msg.SizeBytes}},
	)
	return nil
}
