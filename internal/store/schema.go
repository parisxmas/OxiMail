package store

import (
	"fmt"
	"strings"

	"github.com/parisxmas/OxiDB/go/oxidb"
)

// EnsureSchema creates the indexes and the blob bucket the mail server
// relies on. OxiDB auto-creates collections on first insert, but indexes
// must be declared. It is safe to call on every startup: an index that
// already exists is treated as success.
//
// Per-account collections (messages_acct_<id>, …) are NOT created here
// — they're created when the account is created (CreateAccount calls
// EnsureAccountCollections). A startup migration in MigratePerAccount
// catches any pre-existing accounts whose data still lives in the old
// shared `messages` / `mailboxes` / … collections.
func EnsureSchema(s *Store) error {
	// Create the SHARED collections up front so index creation has
	// something to attach to. The legacy per-account collection names
	// ("messages", "mailboxes", "vacations", "sieve_scripts",
	// "expunge_log") are NOT pre-created here — if a fresh install
	// only sees the new layout, those names never get materialised.
	// They are created on demand by MigratePerAccount when migrating
	// an existing install that has data under the old names.
	for _, coll := range []string{
		CollDomains, CollAccounts, CollAliases,
		CollOutboundQueue, CollBlobRefs,
	} {
		if err := ensureCollection(s.db, coll); err != nil {
			return err
		}
	}

	// Unique identity indexes — one row per address / domain / account.
	if err := ensureUnique(s.db, CollDomains, "domain"); err != nil {
		return err
	}
	if err := ensureUnique(s.db, CollAccounts, "address"); err != nil {
		return err
	}
	if err := ensureUnique(s.db, CollAliases, "address"); err != nil {
		return err
	}
	if err := ensureUnique(s.db, CollBlobRefs, "blob_key"); err != nil {
		return err
	}

	// Lookup indexes for the hot query paths on the SHARED collections.
	for _, ix := range []struct {
		coll, field string
	}{
		{CollAccounts, "domain"},
		{CollAliases, "domain"},
		{CollOutboundQueue, "status"},
		{CollOutboundQueue, "next_retry_at"},
	} {
		if err := ensureIndex(s.db, ix.coll, ix.field); err != nil {
			return err
		}
	}

	// The blob bucket for message bodies (idempotent).
	if err := s.db.CreateBucket(BlobBucket); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("store: create blob bucket %q: %w", BlobBucket, err)
	}

	// Make sure every existing account has its per-account collections
	// + indexes in place, and that any data left in the old shared
	// `messages` / `mailboxes` / … collections is moved over.
	if err := MigratePerAccount(s); err != nil {
		return fmt.Errorf("store: per-account migration: %w", err)
	}
	return nil
}

func ensureCollection(db *oxidb.Client, coll string) error {
	if err := db.CreateCollection(coll); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("store: create collection %q: %w", coll, err)
	}
	return nil
}

func ensureIndex(db *oxidb.Client, coll, field string) error {
	if err := db.CreateIndex(coll, field); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("store: index %s.%s: %w", coll, field, err)
	}
	return nil
}

func ensureUnique(db *oxidb.Client, coll, field string) error {
	if err := db.CreateUniqueIndex(coll, field); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("store: unique index %s.%s: %w", coll, field, err)
	}
	return nil
}

func ensureComposite(db *oxidb.Client, coll string, fields []string) error {
	if err := db.CreateCompositeIndex(coll, fields); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("store: composite index %s%v: %w", coll, fields, err)
	}
	return nil
}

// isAlreadyExists reports whether `err` is OxiDB complaining that the
// index already exists — which makes EnsureSchema safe to re-run.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "exist") || strings.Contains(msg, "already")
}
