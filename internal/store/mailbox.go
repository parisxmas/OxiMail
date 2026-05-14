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
	Subscribed  bool   `json:"subscribed"`
	CreatedAt   string `json:"created_at"`
}

// defaultMailboxes are created for every new account.
var defaultMailboxes = []string{"INBOX", "Sent", "Drafts", "Trash", "Archive"}

// CreateMailbox creates a folder for an account. UIDNext starts at 1 and
// UIDValidity is set once, to the creation time.
func (s *Store) CreateMailbox(accountID uint64, name string) (*Mailbox, error) {
	mb := &Mailbox{
		AccountID:   accountID,
		Name:        name,
		UIDNext:     1,
		UIDValidity: uint32(time.Now().Unix()),
		Subscribed:  true,
		CreatedAt:   nowRFC3339(),
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
