package store

import (
	"fmt"

	"github.com/parisxmas/OxiDB/go/oxidb"
)

// Per-account collection naming. Each account owns its own set of
// collections — the per-account data (mailboxes / messages / vacation
// rule / sieve script / expunge log) lives in collections suffixed
// with the account id, so on disk you get e.g.
// `messages_acct_2.btree`, `mailboxes_acct_2.btree`, ... — one file
// per account per type. Side benefits:
//   - DeleteAccount can DropCollection to reclaim space and audit
//     completion (no orphan rows in a shared table).
//   - oximailctl backup / per-account snapshots map onto filesystem
//     boundaries instead of needing collection scans.
//   - Bigger blast-radius failures (e.g. a corrupt btree) only take
//     out the affected account, not every mailbox on the server.
//
// Collections that are inherently cross-account (accounts, domains,
// aliases, outbound_queue, blob_refs) stay shared — splitting them
// would just move bookkeeping into the caller without gaining
// isolation.

const (
	collMessagesPrefix     = "messages_acct_"
	collMailboxesPrefix    = "mailboxes_acct_"
	collVacationsPrefix    = "vacations_acct_"
	collSieveScriptsPrefix = "sieve_scripts_acct_"
	collExpungeLogPrefix   = "expunge_log_acct_"
)

// MessagesColl returns the messages-collection name for accountID.
func MessagesColl(accountID uint64) string { return fmt.Sprintf("%s%d", collMessagesPrefix, accountID) }

// MailboxesColl returns the mailboxes-collection name for accountID.
func MailboxesColl(accountID uint64) string {
	return fmt.Sprintf("%s%d", collMailboxesPrefix, accountID)
}

// VacationsColl returns the vacations-collection name for accountID.
// (Holds exactly one document per account — kept as a collection for
// schema symmetry and to share the OxiDB index/WAL machinery.)
func VacationsColl(accountID uint64) string {
	return fmt.Sprintf("%s%d", collVacationsPrefix, accountID)
}

// SieveScriptsColl returns the sieve-scripts-collection name for
// accountID. (Same one-doc-per-account shape as VacationsColl.)
func SieveScriptsColl(accountID uint64) string {
	return fmt.Sprintf("%s%d", collSieveScriptsPrefix, accountID)
}

// ExpungeLogColl returns the expunge-log-collection name for
// accountID. The log is keyed by mailbox_id at the row level; the
// per-account scope means QRESYNC reads only scan this account's
// mailboxes' history.
func ExpungeLogColl(accountID uint64) string {
	return fmt.Sprintf("%s%d", collExpungeLogPrefix, accountID)
}

// EnsureAccountCollections creates the per-account collections + their
// indexes for accountID. Idempotent — safe to call at every startup
// (in case the schema check ran but account-collection setup didn't).
// Called from CreateAccount and from the startup migration walker.
func EnsureAccountCollections(db *oxidb.Client, accountID uint64) error {
	msg := MessagesColl(accountID)
	mb := MailboxesColl(accountID)
	vac := VacationsColl(accountID)
	sieve := SieveScriptsColl(accountID)
	exp := ExpungeLogColl(accountID)

	for _, coll := range []string{msg, mb, vac, sieve, exp} {
		if err := ensureCollection(db, coll); err != nil {
			return err
		}
	}

	// Mailboxes: lookup by account is implicit (we're already in the
	// account's collection), but name lookup + (account_id, name) is
	// still hit by GetMailboxByName — keep account_id field on the
	// row so the index can carry across the per-account split.
	if err := ensureIndex(db, mb, "account_id"); err != nil {
		return err
	}
	if err := ensureComposite(db, mb, []string{"account_id", "name"}); err != nil {
		return err
	}

	// Messages: list by mailbox, fetch by (mailbox, uid), per-account
	// cascades by account_id.
	if err := ensureIndex(db, msg, "mailbox_id"); err != nil {
		return err
	}
	if err := ensureIndex(db, msg, "account_id"); err != nil {
		return err
	}
	if err := ensureComposite(db, msg, []string{"mailbox_id", "uid"}); err != nil {
		return err
	}

	// Vacation / Sieve: one row per account, indexed for the lookup.
	if err := ensureUnique(db, vac, "account_id"); err != nil {
		return err
	}
	if err := ensureUnique(db, sieve, "account_id"); err != nil {
		return err
	}

	// Expunge log: scanned by mailbox_id for QRESYNC reconciliation.
	if err := ensureIndex(db, exp, "mailbox_id"); err != nil {
		return err
	}
	return nil
}

// DropAccountCollections removes every per-account collection for
// accountID. Called from DeleteAccount after the account doc is gone.
//
// Implementation: prefer DropCollection (reclaims the on-disk btree
// files entirely) but fall back to clearing all docs if OxiDB rejects
// the drop. OxiDB v0.0.0-20260514 currently fails DropCollection on
// some collection shapes with an "io error: Not a directory" — until
// that upstream bug is fixed, falling back keeps the cascade correct
// (the data is gone, files just stick around as empty btrees, and
// per-account collection ids are monotonic so the empty files are
// never reused).
func DropAccountCollections(db *oxidb.Client, accountID uint64) error {
	for _, coll := range []string{
		MessagesColl(accountID),
		MailboxesColl(accountID),
		VacationsColl(accountID),
		SieveScriptsColl(accountID),
		ExpungeLogColl(accountID),
	} {
		// Try DropCollection first — the ideal path.
		if err := db.DropCollection(coll); err == nil {
			continue
		} else if isMissingCollection(err) {
			continue
		}
		// Fall back: clear every row. Treats a missing collection as
		// success since either way the rows are gone.
		if _, err := db.Delete(coll, map[string]any{}); err != nil && !isMissingCollection(err) {
			return fmt.Errorf("store: clear collection %q: %w", coll, err)
		}
	}
	return nil
}

// isMissingCollection identifies OxiDB's "no such collection" response
// so DropAccountCollections can be idempotent.
func isMissingCollection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// OxiDB's wire-level error text — keep loose-match to survive
	// minor wording tweaks.
	return contains(msg, "no such collection") ||
		contains(msg, "does not exist") ||
		contains(msg, "not found")
}

// contains is a tiny case-insensitive substring helper kept private so
// we don't tangle this file with strings.ToLower imports for one use.
func contains(haystack, needle string) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return len(needle) == 0
	}
	// Case-insensitive walk; len(needle) tends to be small.
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a := haystack[i+j]
			b := needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
