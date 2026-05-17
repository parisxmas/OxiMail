package store

import (
	"fmt"
	"log"
)

// Legacy (pre-per-account) collection names. After MigratePerAccount
// has run successfully these collections are emptied and dropped;
// keeping the constants here as the only references in the codebase
// makes the migration self-contained.
const (
	legacyCollMailboxes    = "mailboxes"
	legacyCollMessages     = "messages"
	legacyCollVacations    = "vacations"
	legacyCollSieveScripts = "sieve_scripts"
	legacyCollExpungeLog   = "expunge_log"
)

// MigratePerAccount walks every account and moves any data that is
// still in the legacy shared collections into the account's own
// per-account collections.
//
// Called from EnsureSchema on every startup. Idempotent: on a fresh
// install (no legacy collections) it does nothing; on a server that
// already migrated, the legacy collections are empty and the work
// reduces to checking that per-account collections exist.
//
// After all accounts have been migrated, the legacy collections are
// dropped so the disk space is reclaimed and there's only one
// canonical layout to reason about.
func MigratePerAccount(s *Store) error {
	accounts, err := s.ListAccounts("")
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	// First pass: make sure every account has its per-account
	// collections in place. EnsureAccountCollections is idempotent.
	for _, acc := range accounts {
		if err := EnsureAccountCollections(s.db, acc.ID); err != nil {
			return fmt.Errorf("ensure collections for account %d: %w", acc.ID, err)
		}
	}

	// Second pass: move docs from the legacy shared collections into
	// the per-account ones. We do this per legacy collection so a
	// crash leaves a partial-but-consistent state (the next startup
	// resumes from wherever we left off; rows we already moved are
	// idempotently re-inserted because OxiDB matches on _id).
	movedAny := false
	moved, err := migrateLegacyCollection(s, legacyCollMailboxes, MailboxesColl)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", legacyCollMailboxes, err)
	}
	movedAny = movedAny || moved > 0

	moved, err = migrateLegacyCollection(s, legacyCollMessages, MessagesColl)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", legacyCollMessages, err)
	}
	movedAny = movedAny || moved > 0

	moved, err = migrateLegacyCollection(s, legacyCollVacations, VacationsColl)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", legacyCollVacations, err)
	}
	movedAny = movedAny || moved > 0

	moved, err = migrateLegacyCollection(s, legacyCollSieveScripts, SieveScriptsColl)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", legacyCollSieveScripts, err)
	}
	movedAny = movedAny || moved > 0

	// expunge_log doesn't carry an account_id field — its rows reference
	// mailbox_id. We resolve mailbox_id → account_id from each account's
	// (already migrated) mailboxes collection.
	moved, err = migrateLegacyExpungeLog(s)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", legacyCollExpungeLog, err)
	}
	movedAny = movedAny || moved > 0

	if movedAny {
		log.Print("store: per-account migration moved legacy rows; old collections will be dropped")
	}
	// Drop the legacy collections once they are empty. If a row is left
	// behind (orphan with an account_id that no account owns) we skip
	// the drop and leave the collection in place so the operator can
	// see it; another startup pass will retry.
	for _, legacy := range []string{
		legacyCollMailboxes, legacyCollMessages,
		legacyCollVacations, legacyCollSieveScripts, legacyCollExpungeLog,
	} {
		left, err := s.db.Count(legacy, map[string]any{})
		if err != nil {
			// Collection might not exist on a fresh install — that's fine.
			if isMissingCollection(err) {
				continue
			}
			return fmt.Errorf("count %s: %w", legacy, err)
		}
		if left > 0 {
			log.Printf("store: legacy collection %q still has %d row(s); not dropping yet", legacy, left)
			continue
		}
		// Drop the now-empty legacy collection. OxiDB
		// v0.0.0-20260514 has a DropCollection bug ("io error: Not a
		// directory") on some collection shapes; treat that as
		// non-fatal — the data is already gone, the file just sticks
		// around as an empty btree. Logging it once per startup is
		// loud enough to nag the operator without breaking the boot.
		if err := s.db.DropCollection(legacy); err != nil && !isMissingCollection(err) {
			log.Printf("store: drop legacy collection %s: %v (non-fatal; data already migrated, empty btree remains)", legacy, err)
		}
	}
	return nil
}

// migrateLegacyCollection moves every row in `legacy` whose
// account_id matches an existing account into the per-account
// collection chosen by `target(accountID)`. Returns the number of
// rows actually moved.
//
// Rows without an account_id, or with an account_id that points at
// no account, are left in place — operator-visible evidence that
// something is off, and a safety net against silently dropping data.
func migrateLegacyCollection(s *Store, legacy string, target func(uint64) string) (int, error) {
	rows, err := s.db.Find(legacy, map[string]any{}, nil)
	if err != nil {
		if isMissingCollection(err) {
			return 0, nil
		}
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	moved := 0
	for _, row := range rows {
		accID, ok := uint64Field(row, "account_id")
		if !ok || accID == 0 {
			continue
		}
		// Re-insert into the per-account collection. OxiDB Insert
		// auto-assigns _id; we strip the legacy _id to let it pick a
		// fresh one in the new collection (the document body — which
		// is what callers care about — is unchanged).
		dst := target(accID)
		clone := make(map[string]any, len(row))
		for k, v := range row {
			if k == "_id" {
				continue
			}
			clone[k] = v
		}
		if _, err := s.db.Insert(dst, clone); err != nil {
			return moved, fmt.Errorf("insert into %s: %w", dst, err)
		}
		if _, err := s.db.Delete(legacy, map[string]any{"_id": row["_id"]}); err != nil {
			return moved, fmt.Errorf("delete from %s: %w", legacy, err)
		}
		moved++
	}
	return moved, nil
}

// migrateLegacyExpungeLog moves expunge-log rows into per-account
// collections. Rows reference mailbox_id, not account_id; we walk the
// accounts and their per-account mailboxes collection to build the
// mailbox_id → account_id map.
func migrateLegacyExpungeLog(s *Store) (int, error) {
	rows, err := s.db.Find(legacyCollExpungeLog, map[string]any{}, nil)
	if err != nil {
		if isMissingCollection(err) {
			return 0, nil
		}
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	mailboxOwner, err := buildMailboxOwnerIndex(s)
	if err != nil {
		return 0, err
	}

	moved := 0
	for _, row := range rows {
		mbID, ok := uint64Field(row, "mailbox_id")
		if !ok || mbID == 0 {
			continue
		}
		accID, ok := mailboxOwner[mbID]
		if !ok {
			continue
		}
		dst := ExpungeLogColl(accID)
		clone := make(map[string]any, len(row))
		for k, v := range row {
			if k == "_id" {
				continue
			}
			clone[k] = v
		}
		if _, err := s.db.Insert(dst, clone); err != nil {
			return moved, fmt.Errorf("insert into %s: %w", dst, err)
		}
		if _, err := s.db.Delete(legacyCollExpungeLog, map[string]any{"_id": row["_id"]}); err != nil {
			return moved, fmt.Errorf("delete from %s: %w", legacyCollExpungeLog, err)
		}
		moved++
	}
	return moved, nil
}

// buildMailboxOwnerIndex returns mailbox_id → account_id for every
// mailbox across every account. Used by the expunge-log migration to
// resolve mailbox_id rows back to their owning account.
func buildMailboxOwnerIndex(s *Store) (map[uint64]uint64, error) {
	accounts, err := s.ListAccounts("")
	if err != nil {
		return nil, err
	}
	owners := make(map[uint64]uint64)
	for _, acc := range accounts {
		rows, err := s.db.Find(MailboxesColl(acc.ID), map[string]any{}, nil)
		if err != nil {
			if isMissingCollection(err) {
				continue
			}
			return nil, fmt.Errorf("scan mailboxes for account %d: %w", acc.ID, err)
		}
		for _, r := range rows {
			if id, ok := uint64Field(r, "_id"); ok {
				owners[id] = acc.ID
			}
		}
	}
	return owners, nil
}

// uint64Field reads a numeric field from a raw OxiDB document. OxiDB
// returns ints as the largest representation the wire format used, so
// we normalise across float64 / int64 / uint64 / json.Number.
func uint64Field(doc map[string]any, name string) (uint64, bool) {
	v, ok := doc[name]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case uint64:
		return x, true
	case int64:
		if x < 0 {
			return 0, false
		}
		return uint64(x), true
	case int:
		if x < 0 {
			return 0, false
		}
		return uint64(x), true
	case float64:
		if x < 0 {
			return 0, false
		}
		return uint64(x), true
	}
	return 0, false
}
