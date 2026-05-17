package store

import (
	"fmt"
	"time"
)

// Mailbox is an IMAP folder belonging to an account. It carries the
// per-mailbox UID state IMAP requires.
type Mailbox struct {
	ID        uint64 `json:"_id,omitempty"`
	AccountID uint64 `json:"account_id"`
	Name      string `json:"name"` // "INBOX", "Sent", "Trash", or a user folder
	// UIDNext is the next UID to hand out. Allocate strictly through
	// NextUID — never a plain update — so concurrent deliveries cannot
	// lose an increment.
	UIDNext uint32 `json:"uidnext"`
	// UIDValidity is fixed at creation. If a client ever sees it change,
	// it must discard its cached view of the mailbox.
	UIDValidity uint32 `json:"uidvalidity"`
	// HighestModSeq is the highest mod-sequence value seen in this
	// mailbox (RFC 7162 CONDSTORE). Bumped via NextModSeq on every
	// flag change and every message append; returned as
	// HIGHESTMODSEQ in SELECT / STATUS responses.
	HighestModSeq uint64 `json:"highest_modseq"`
	Subscribed    bool   `json:"subscribed"`
	CreatedAt     string `json:"created_at"`
}

// defaultMailboxes are created for every new account. Junk holds
// messages quarantined by the spam pipeline (DMARC p=quarantine, etc.).
var defaultMailboxes = []string{"INBOX", "Sent", "Drafts", "Trash", "Archive", "Junk"}

// CreateMailbox creates a folder for an account. UIDNext starts at 1 and
// UIDValidity is set once, to the creation time.
func (s *Store) CreateMailbox(accountID uint64, name string) (*Mailbox, error) {
	mb := &Mailbox{
		AccountID:     accountID,
		Name:          name,
		UIDNext:       1,
		UIDValidity:   uint32(time.Now().Unix()),
		HighestModSeq: 0,
		Subscribed:    true,
		CreatedAt:     nowRFC3339(),
	}
	doc, err := encodeDoc(mb)
	if err != nil {
		return nil, err
	}
	resp, err := s.db.Insert(CollMailboxes, doc)
	if err != nil {
		return nil, fmt.Errorf("store: create mailbox %q for account %d: %w", name, accountID, err)
	}
	if mb.ID, err = insertedID(resp); err != nil {
		return nil, err
	}
	return mb, nil
}

// GetMailbox looks a mailbox up by its OxiDB id.
func (s *Store) GetMailbox(id uint64) (*Mailbox, error) {
	m, err := s.db.FindOne(CollMailboxes, map[string]any{"_id": id})
	if err != nil {
		return nil, fmt.Errorf("store: get mailbox %d: %w", id, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var mb Mailbox
	if err := decodeDoc(m, &mb); err != nil {
		return nil, err
	}
	return &mb, nil
}

// GetMailboxByName looks up one of an account's folders by name.
func (s *Store) GetMailboxByName(accountID uint64, name string) (*Mailbox, error) {
	m, err := s.db.FindOne(CollMailboxes, map[string]any{"account_id": accountID, "name": name})
	if err != nil {
		return nil, fmt.Errorf("store: get mailbox %q for account %d: %w", name, accountID, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var mb Mailbox
	if err := decodeDoc(m, &mb); err != nil {
		return nil, err
	}
	return &mb, nil
}

// ListMailboxes returns all of an account's folders.
func (s *Store) ListMailboxes(accountID uint64) ([]Mailbox, error) {
	rows, err := s.db.Find(CollMailboxes, map[string]any{"account_id": accountID}, nil)
	if err != nil {
		return nil, fmt.Errorf("store: list mailboxes for account %d: %w", accountID, err)
	}
	out := make([]Mailbox, 0, len(rows))
	for _, r := range rows {
		var mb Mailbox
		if err := decodeDoc(r, &mb); err != nil {
			return nil, err
		}
		out = append(out, mb)
	}
	return out, nil
}

// EnsureDefaultMailboxes creates the standard folders (INBOX, Sent, ...)
// for an account, skipping any that already exist. Safe to re-run.
func (s *Store) EnsureDefaultMailboxes(accountID uint64) error {
	for _, name := range defaultMailboxes {
		_, err := s.GetMailboxByName(accountID, name)
		if err == nil {
			continue // already there
		}
		if err != ErrNotFound {
			return err
		}
		if _, err := s.CreateMailbox(accountID, name); err != nil {
			return err
		}
	}
	return nil
}

// DeleteMailbox removes a mailbox and every message in it. The body
// blobs go first — orphaned blobs are wasted disk, but a message
// document pointing at a missing blob is a broken read.
func (s *Store) DeleteMailbox(mailboxID uint64) error {
	msgs, err := s.db.Find(CollMessages, map[string]any{"mailbox_id": mailboxID}, nil)
	if err != nil {
		return fmt.Errorf("store: delete mailbox %d: list messages: %w", mailboxID, err)
	}
	for _, m := range msgs {
		if key, ok := m["body_blob"].(string); ok && key != "" {
			if err := s.db.DeleteObject(BlobBucket, key); err != nil {
				return fmt.Errorf("store: delete mailbox %d: remove body %q: %w", mailboxID, key, err)
			}
		}
	}
	if _, err := s.db.Delete(CollMessages, map[string]any{"mailbox_id": mailboxID}); err != nil {
		return fmt.Errorf("store: delete mailbox %d: remove messages: %w", mailboxID, err)
	}
	if _, err := s.db.Delete(CollMailboxes, map[string]any{"_id": mailboxID}); err != nil {
		return fmt.Errorf("store: delete mailbox %d: remove mailbox: %w", mailboxID, err)
	}
	return nil
}

// RenameMailbox updates a mailbox's name. The IMAP layer is responsible
// for checking that the new name does not already exist for the account.
func (s *Store) RenameMailbox(mailboxID uint64, newName string) error {
	doc, err := s.db.FindAndModify(
		CollMailboxes,
		map[string]any{"_id": mailboxID},
		map[string]any{"$set": map[string]any{"name": newName}},
	)
	if err != nil {
		return fmt.Errorf("store: rename mailbox %d: %w", mailboxID, err)
	}
	if doc == nil {
		return ErrNotFound
	}
	return nil
}

// SetMailboxSubscribed updates a mailbox's IMAP subscription state
// (the SUBSCRIBE / UNSUBSCRIBE commands).
func (s *Store) SetMailboxSubscribed(mailboxID uint64, subscribed bool) error {
	doc, err := s.db.FindAndModify(
		CollMailboxes,
		map[string]any{"_id": mailboxID},
		map[string]any{"$set": map[string]any{"subscribed": subscribed}},
	)
	if err != nil {
		return fmt.Errorf("store: set subscribed on mailbox %d: %w", mailboxID, err)
	}
	if doc == nil {
		return ErrNotFound
	}
	return nil
}

// NextUID atomically allocates the next IMAP UID for a mailbox and
// returns it.
//
// IMAP requires a mailbox's UIDs to be strictly increasing and never
// reused. OxiDB's `update` + `$inc` is unsafe here — its read and write
// phases are not contiguous, so concurrent deliveries to the same
// mailbox would lose increments. `find_and_modify` performs the
// increment and the read-back as one atomic step, which is exactly what
// UID allocation needs.
//
// A crash between allocating a UID and inserting the message simply
// burns that UID; IMAP explicitly tolerates gaps in the UID sequence, so
// that is harmless.
func (s *Store) NextUID(mailboxID uint64) (uint32, error) {
	doc, err := s.db.FindAndModify(
		CollMailboxes,
		map[string]any{"_id": mailboxID},
		map[string]any{"$inc": map[string]any{"uidnext": 1}},
	)
	if err != nil {
		return 0, fmt.Errorf("store: allocate UID for mailbox %d: %w", mailboxID, err)
	}
	if doc == nil {
		return 0, fmt.Errorf("store: mailbox %d not found", mailboxID)
	}
	// `$inc` returns the post-increment value; the UID we just allocated
	// is the value immediately before it. JSON numbers decode as float64.
	next, ok := doc["uidnext"].(float64)
	if !ok || next < 1 {
		return 0, fmt.Errorf("store: mailbox %d has invalid uidnext %v", mailboxID, doc["uidnext"])
	}
	return uint32(next) - 1, nil
}

// MailboxStats is the per-mailbox counts the webmail mailbox-list
// endpoint uses: total messages plus unseen (messages without
// \Seen). Returned as a struct so the API stays stable when we
// later add e.g. recent or quota-used numbers.
type MailboxStats struct {
	Total  int
	Unseen int
}

// CountMessages returns the number of messages in a mailbox via the
// OxiDB Count aggregate — cheaper than a full ListMessages when the
// caller does not need each document.
func (s *Store) CountMessages(mailboxID uint64) (int, error) {
	n, err := s.db.Count(CollMessages, map[string]any{"mailbox_id": mailboxID})
	if err != nil {
		return 0, fmt.Errorf("store: count messages in mailbox %d: %w", mailboxID, err)
	}
	return n, nil
}

// Stats returns the total + unseen counts for a mailbox.
//
// OxiDB does not currently expose a "flags does NOT contain X" query
// operator, so unseen is still derived by walking the message
// metadata. Total uses the Count aggregate so callers that only need
// the total (e.g. STATUS MESSAGES) get the fast path.
func (s *Store) Stats(mailboxID uint64) (MailboxStats, error) {
	msgs, err := s.ListMessages(mailboxID)
	if err != nil {
		return MailboxStats{}, err
	}
	stats := MailboxStats{Total: len(msgs)}
	for i := range msgs {
		seen := false
		for _, f := range msgs[i].Flags {
			if f == `\Seen` {
				seen = true
				break
			}
		}
		if !seen {
			stats.Unseen++
		}
	}
	return stats, nil
}

// NextModSeq atomically bumps a mailbox's HighestModSeq and returns
// the new value. Like NextUID, every increment lands as one
// find_and_modify so concurrent flag changes cannot lose a bump. The
// returned value is the one to write on the affected message — and
// the new HighestModSeq the mailbox is at.
//
// RFC 7162 §2.1.2 requires the mod-sequence to be a positive 63-bit
// integer that never decreases. Burning one on a crash is harmless,
// same as for UID.
func (s *Store) NextModSeq(mailboxID uint64) (uint64, error) {
	doc, err := s.db.FindAndModify(
		CollMailboxes,
		map[string]any{"_id": mailboxID},
		map[string]any{"$inc": map[string]any{"highest_modseq": 1}},
	)
	if err != nil {
		return 0, fmt.Errorf("store: allocate mod-seq for mailbox %d: %w", mailboxID, err)
	}
	if doc == nil {
		return 0, fmt.Errorf("store: mailbox %d not found", mailboxID)
	}
	next, ok := doc["highest_modseq"].(float64)
	if !ok || next < 1 {
		return 0, fmt.Errorf("store: mailbox %d has invalid highest_modseq %v", mailboxID, doc["highest_modseq"])
	}
	return uint64(next), nil
}
