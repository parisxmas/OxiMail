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
func EnsureSchema(s *Store) error {
	// Create the collections up front so index creation has something to
	// attach to (OxiDB otherwise only materializes a collection on its
	// first insert).
	for _, coll := range []string{
		CollDomains, CollAccounts, CollAliases,
		CollMailboxes, CollMessages, CollOutboundQueue,
		CollVacations, CollSieveScripts,
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
	if err := ensureUnique(s.db, CollVacations, "account_id"); err != nil {
		return err
	}
	if err := ensureUnique(s.db, CollSieveScripts, "account_id"); err != nil {
		return err
	}

	// Lookup indexes for the hot query paths.
	for _, ix := range []struct {
		coll, field string
	}{
		{CollAccounts, "domain"},
		{CollAliases, "domain"},
		{CollMailboxes, "account_id"},
		{CollMessages, "mailbox_id"},
		{CollMessages, "account_id"},
		{CollOutboundQueue, "status"},
		{CollOutboundQueue, "next_retry_at"},
	} {
		if err := ensureIndex(s.db, ix.coll, ix.field); err != nil {
			return err
		}
	}

	// Composite indexes for the multi-field hot paths: IMAP fetch by
	// (mailbox, UID), and mailbox lookup by (account, folder name).
	if err := ensureComposite(s.db, CollMessages, []string{"mailbox_id", "uid"}); err != nil {
		return err
	}
	if err := ensureComposite(s.db, CollMailboxes, []string{"account_id", "name"}); err != nil {
		return err
	}

	// The blob bucket for message bodies (idempotent).
	if err := s.db.CreateBucket(BlobBucket); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("store: create blob bucket %q: %w", BlobBucket, err)
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
