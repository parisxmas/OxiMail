package store

import (
	"fmt"
	"strings"
)

// Domain is a hosted mail domain.
type Domain struct {
	ID        uint64 `json:"_id,omitempty"`
	Domain    string `json:"domain"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
}

// Account is a mailbox account: a login (for SMTP AUTH and IMAP) plus a
// quota. The address is the full email and is unique.
type Account struct {
	ID           uint64 `json:"_id,omitempty"`
	Address      string `json:"address"`
	Domain       string `json:"domain"` // denormalized from Address for fast per-domain queries
	PasswordHash string `json:"password_hash"`
	QuotaBytes   int64  `json:"quota_bytes"` // 0 = unlimited
	UsedBytes    int64  `json:"used_bytes"`
	Active       bool   `json:"active"`
	CreatedAt    string `json:"created_at"`
}

// Alias forwards an address to one or more destination addresses, which
// may be local accounts or remote addresses.
type Alias struct {
	ID           uint64   `json:"_id,omitempty"`
	Address      string   `json:"address"`
	Domain       string   `json:"domain"`
	Destinations []string `json:"destinations"`
	Active       bool     `json:"active"`
	CreatedAt    string   `json:"created_at"`
}

// domainOf extracts the domain part of an email address.
func domainOf(address string) (string, error) {
	at := strings.LastIndexByte(address, '@')
	if at <= 0 || at == len(address)-1 {
		return "", fmt.Errorf("store: %q is not a valid address", address)
	}
	return strings.ToLower(address[at+1:]), nil
}

// CreateDomain registers a hosted domain.
func (s *Store) CreateDomain(domain string) (*Domain, error) {
	d := &Domain{Domain: strings.ToLower(domain), Active: true, CreatedAt: nowRFC3339()}
	doc, err := encodeDoc(d)
	if err != nil {
		return nil, err
	}
	resp, err := s.db.Insert(CollDomains, doc)
	if err != nil {
		return nil, fmt.Errorf("store: create domain %q: %w", domain, err)
	}
	if d.ID, err = insertedID(resp); err != nil {
		return nil, err
	}
	return d, nil
}

// GetDomain looks a domain up by name. Returns ErrNotFound if absent.
func (s *Store) GetDomain(domain string) (*Domain, error) {
	m, err := s.db.FindOne(CollDomains, map[string]any{"domain": strings.ToLower(domain)})
	if err != nil {
		return nil, fmt.Errorf("store: get domain %q: %w", domain, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var d Domain
	if err := decodeDoc(m, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateAccount registers a mailbox account. The domain is derived from
// the address; the caller is responsible for hashing the password.
func (s *Store) CreateAccount(address, passwordHash string, quotaBytes int64) (*Account, error) {
	address = strings.ToLower(address)
	domain, err := domainOf(address)
	if err != nil {
		return nil, err
	}
	a := &Account{
		Address:      address,
		Domain:       domain,
		PasswordHash: passwordHash,
		QuotaBytes:   quotaBytes,
		UsedBytes:    0,
		Active:       true,
		CreatedAt:    nowRFC3339(),
	}
	doc, err := encodeDoc(a)
	if err != nil {
		return nil, err
	}
	resp, err := s.db.Insert(CollAccounts, doc)
	if err != nil {
		return nil, fmt.Errorf("store: create account %q: %w", address, err)
	}
	if a.ID, err = insertedID(resp); err != nil {
		return nil, err
	}
	return a, nil
}

// GetAccount looks an account up by address. Returns ErrNotFound if absent.
func (s *Store) GetAccount(address string) (*Account, error) {
	m, err := s.db.FindOne(CollAccounts, map[string]any{"address": strings.ToLower(address)})
	if err != nil {
		return nil, fmt.Errorf("store: get account %q: %w", address, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var a Account
	if err := decodeDoc(m, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAccountByID looks an account up by its OxiDB id.
func (s *Store) GetAccountByID(id uint64) (*Account, error) {
	m, err := s.db.FindOne(CollAccounts, map[string]any{"_id": id})
	if err != nil {
		return nil, fmt.Errorf("store: get account %d: %w", id, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var a Account
	if err := decodeDoc(m, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteAccount removes an account and everything that hangs off it.
//
// OxiDB has no foreign keys or cascading deletes, so this layer performs
// the cascade by hand: every message's body blob is removed, then the
// message documents, then the mailboxes, then the account itself. The
// body blobs go first — an orphaned blob is mere wasted disk, whereas a
// message document pointing at a missing blob is a broken read.
func (s *Store) DeleteAccount(id uint64) error {
	msgs, err := s.db.Find(CollMessages, map[string]any{"account_id": id}, nil)
	if err != nil {
		return fmt.Errorf("store: delete account %d: list messages: %w", id, err)
	}
	for _, m := range msgs {
		if key, ok := m["body_blob"].(string); ok && key != "" {
			if err := s.db.DeleteObject(BlobBucket, key); err != nil {
				return fmt.Errorf("store: delete account %d: remove body %q: %w", id, key, err)
			}
		}
	}
	if _, err := s.db.Delete(CollMessages, map[string]any{"account_id": id}); err != nil {
		return fmt.Errorf("store: delete account %d: remove messages: %w", id, err)
	}
	if _, err := s.db.Delete(CollMailboxes, map[string]any{"account_id": id}); err != nil {
		return fmt.Errorf("store: delete account %d: remove mailboxes: %w", id, err)
	}
	if _, err := s.db.Delete(CollAccounts, map[string]any{"_id": id}); err != nil {
		return fmt.Errorf("store: delete account %d: remove account: %w", id, err)
	}
	return nil
}

// CreateAlias registers a forwarding alias.
func (s *Store) CreateAlias(address string, destinations []string) (*Alias, error) {
	address = strings.ToLower(address)
	domain, err := domainOf(address)
	if err != nil {
		return nil, err
	}
	al := &Alias{
		Address:      address,
		Domain:       domain,
		Destinations: destinations,
		Active:       true,
		CreatedAt:    nowRFC3339(),
	}
	doc, err := encodeDoc(al)
	if err != nil {
		return nil, err
	}
	resp, err := s.db.Insert(CollAliases, doc)
	if err != nil {
		return nil, fmt.Errorf("store: create alias %q: %w", address, err)
	}
	if al.ID, err = insertedID(resp); err != nil {
		return nil, err
	}
	return al, nil
}

// GetAlias looks an alias up by address. Returns ErrNotFound if absent.
func (s *Store) GetAlias(address string) (*Alias, error) {
	m, err := s.db.FindOne(CollAliases, map[string]any{"address": strings.ToLower(address)})
	if err != nil {
		return nil, fmt.Errorf("store: get alias %q: %w", address, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var al Alias
	if err := decodeDoc(m, &al); err != nil {
		return nil, err
	}
	return &al, nil
}

// ResolveRecipient maps an inbound RCPT address to the local account IDs
// that should receive the mail: a direct account match yields that one
// account; an alias yields the local accounts among its destinations.
// An empty result means the address is not deliverable to a local
// mailbox here.
//
// TODO: alias destinations that are *remote* addresses should be handed
// to the outbound queue for forwarding; for now only local destinations
// are resolved, and alias-of-alias chains are not followed.
func (s *Store) ResolveRecipient(address string) ([]uint64, error) {
	address = strings.ToLower(address)

	if acc, err := s.GetAccount(address); err == nil {
		return []uint64{acc.ID}, nil
	} else if err != ErrNotFound {
		return nil, err
	}

	al, err := s.GetAlias(address)
	if err == ErrNotFound {
		return nil, nil // not deliverable here
	}
	if err != nil {
		return nil, err
	}

	var local []uint64
	for _, dest := range al.Destinations {
		acc, err := s.GetAccount(dest)
		if err == ErrNotFound {
			continue // remote destination — outbound forwarding, not handled yet
		}
		if err != nil {
			return nil, err
		}
		local = append(local, acc.ID)
	}
	return local, nil
}
