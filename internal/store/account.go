package store

import (
	"errors"
	"fmt"
	"strings"
)

// Domain is a hosted mail domain.
type Domain struct {
	ID        uint64 `json:"_id,omitempty"`
	Domain    string `json:"domain"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
	// DKIM signing material, set via SetDKIMKey. When DKIMPrivateKey is
	// empty, outbound mail from the domain is sent unsigned.
	DKIMSelector   string `json:"dkim_selector,omitempty"`
	DKIMPrivateKey string `json:"dkim_private_key,omitempty"` // PEM-encoded PKCS#1 RSA key
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

// DeleteDomain removes a hosted domain. It refuses to delete a domain
// that still has accounts in it — purge those first via DeleteAccount.
func (s *Store) DeleteDomain(domain string) error {
	domain = strings.ToLower(domain)
	accounts, err := s.ListAccounts(domain)
	if err != nil {
		return fmt.Errorf("store: delete domain %q: %w", domain, err)
	}
	if len(accounts) > 0 {
		return fmt.Errorf("store: delete domain %q: %d account(s) still in it", domain, len(accounts))
	}
	if _, err := s.db.Delete(CollDomains, map[string]any{"domain": domain}); err != nil {
		return fmt.Errorf("store: delete domain %q: %w", domain, err)
	}
	return nil
}

// SetAccountPassword updates an account's stored password hash. The
// caller is responsible for hashing the plaintext (see HashPassword).
func (s *Store) SetAccountPassword(accountID uint64, passwordHash string) error {
	doc, err := s.db.FindAndModify(
		CollAccounts,
		map[string]any{"_id": accountID},
		map[string]any{"$set": map[string]any{"password_hash": passwordHash}},
	)
	if err != nil {
		return fmt.Errorf("store: set password for account %d: %w", accountID, err)
	}
	if doc == nil {
		return ErrNotFound
	}
	return nil
}

// ListDomains returns every hosted domain.
func (s *Store) ListDomains() ([]Domain, error) {
	rows, err := s.db.Find(CollDomains, map[string]any{}, nil)
	if err != nil {
		return nil, fmt.Errorf("store: list domains: %w", err)
	}
	out := make([]Domain, 0, len(rows))
	for _, r := range rows {
		var d Domain
		if err := decodeDoc(r, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// SetDKIMKey stores a domain's DKIM signing key and selector. The
// private key is PEM-encoded; outbound mail from the domain is then
// signed with it. The domain must already exist.
func (s *Store) SetDKIMKey(domain, selector, privateKeyPEM string) error {
	doc, err := s.db.FindAndModify(
		CollDomains,
		map[string]any{"domain": strings.ToLower(domain)},
		map[string]any{"$set": map[string]any{
			"dkim_selector":    selector,
			"dkim_private_key": privateKeyPEM,
		}},
	)
	if err != nil {
		return fmt.Errorf("store: set DKIM key for %q: %w", domain, err)
	}
	if doc == nil {
		return ErrNotFound
	}
	return nil
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

// Authenticate looks an account up by address and checks the password
// against its stored bcrypt hash. It returns the account on success and
// ErrAuthFailed for any failure — unknown address, inactive account, or
// wrong password — so the caller cannot tell those cases apart. A
// non-auth infrastructure error (e.g. OxiDB unreachable) is returned as
// itself.
func (s *Store) Authenticate(address, password string) (*Account, error) {
	acc, err := s.GetAccount(address)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrAuthFailed
	}
	if err != nil {
		return nil, err
	}
	if !acc.Active || !VerifyPassword(acc.PasswordHash, password) {
		return nil, ErrAuthFailed
	}
	return acc, nil
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

// ListAccounts returns every account, or — when `domain` is non-empty —
// just the accounts in that domain.
func (s *Store) ListAccounts(domain string) ([]Account, error) {
	query := map[string]any{}
	if domain != "" {
		query["domain"] = strings.ToLower(domain)
	}
	rows, err := s.db.Find(CollAccounts, query, nil)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	out := make([]Account, 0, len(rows))
	for _, r := range rows {
		var a Account
		if err := decodeDoc(r, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
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
	// Drop one refcount per message doc; the blob disappears when
	// the count hits zero. A blob that is also referenced by another
	// account (which OxiMail never produces today, but the refcount
	// model permits) survives until that other account drops it
	// too.
	for _, m := range msgs {
		if key, ok := m["body_blob"].(string); ok && key != "" {
			if _, err := s.blobDropRef(key); err != nil {
				return fmt.Errorf("store: delete account %d: drop blob %q: %w", id, key, err)
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

// ListAliases returns every forwarding alias.
func (s *Store) ListAliases() ([]Alias, error) {
	rows, err := s.db.Find(CollAliases, map[string]any{}, nil)
	if err != nil {
		return nil, fmt.Errorf("store: list aliases: %w", err)
	}
	out := make([]Alias, 0, len(rows))
	for _, r := range rows {
		var al Alias
		if err := decodeDoc(r, &al); err != nil {
			return nil, err
		}
		out = append(out, al)
	}
	return out, nil
}

// DeleteAlias removes a forwarding alias by address. Deleting an alias
// that does not exist is not an error.
func (s *Store) DeleteAlias(address string) error {
	if _, err := s.db.Delete(CollAliases, map[string]any{"address": strings.ToLower(address)}); err != nil {
		return fmt.Errorf("store: delete alias %q: %w", address, err)
	}
	return nil
}

// maxAliasDepth caps alias-of-alias expansion. Five hops is far more
// than any sane configuration needs and keeps a cycle from looping
// forever even if the visited-set check missed it somehow.
const maxAliasDepth = 5

// Destinations is the result of resolving one inbound recipient: zero
// or more local account ids, plus zero or more remote addresses
// reached via alias chains. The caller is expected to deliver to the
// local accounts and forward (over the outbound queue) to the remote
// addresses.
type Destinations struct {
	LocalAccounts []uint64
	RemoteAddrs   []string
}

// Empty reports whether the address is not deliverable anywhere.
func (d Destinations) Empty() bool {
	return len(d.LocalAccounts) == 0 && len(d.RemoteAddrs) == 0
}

// ResolveDestinations maps an inbound RCPT address to its delivery
// destinations: local account ids (direct matches and the local
// accounts reached via alias chains) plus remote addresses (alias
// destinations that point outside this server). Recursion is depth-
// and cycle-guarded; an empty result means the address is not hosted
// here.
func (s *Store) ResolveDestinations(address string) (Destinations, error) {
	var d Destinations
	if err := s.resolveDestinations(strings.ToLower(address), map[string]bool{}, 0, &d); err != nil {
		return Destinations{}, err
	}
	return d, nil
}

// ResolveRecipient is the legacy wrapper that returns only the local
// account ids. New code should prefer ResolveDestinations so it can
// also forward to remote alias destinations.
func (s *Store) ResolveRecipient(address string) ([]uint64, error) {
	d, err := s.ResolveDestinations(address)
	if err != nil {
		return nil, err
	}
	return d.LocalAccounts, nil
}

func (s *Store) resolveDestinations(address string, visited map[string]bool, depth int, out *Destinations) error {
	if depth > maxAliasDepth || visited[address] {
		return nil // hop limit or alias cycle — stop
	}
	visited[address] = true

	if acc, err := s.GetAccount(address); err == nil {
		if !containsID(out.LocalAccounts, acc.ID) {
			out.LocalAccounts = append(out.LocalAccounts, acc.ID)
		}
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	al, err := s.GetAlias(address)
	if errors.Is(err, ErrNotFound) {
		// Not a local account and not a hosted alias — at depth 0 it
		// means "not deliverable here"; deeper, it means the alias
		// chain pointed at a remote address.
		if depth > 0 && !containsAddr(out.RemoteAddrs, address) {
			out.RemoteAddrs = append(out.RemoteAddrs, address)
		}
		return nil
	}
	if err != nil {
		return err
	}

	for _, dest := range al.Destinations {
		if err := s.resolveDestinations(strings.ToLower(dest), visited, depth+1, out); err != nil {
			return err
		}
	}
	return nil
}

// containsID reports whether ids already includes id.
func containsID(ids []uint64, id uint64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// containsAddr reports whether addrs already includes addr.
func containsAddr(addrs []string, addr string) bool {
	for _, x := range addrs {
		if x == addr {
			return true
		}
	}
	return false
}
