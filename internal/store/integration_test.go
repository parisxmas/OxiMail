//go:build integration

// Integration tests for the store layer, run against a live
// oxidb-server. They are gated behind the `integration` build tag and
// excluded from a plain `go test ./...`:
//
//	go test -tags=integration ./internal/store/...
//
// The oxidb-server harness lives in internal/itest; if the server
// binary cannot be found the test is skipped, not failed.
package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/store"
)

func TestStore(t *testing.T) {
	// These tests cover logical correctness, atomicity, and concurrency
	// — not durability across a restart — so lazy sync is safe here and
	// keeps the 800-way NextUID contention test off the per-write fsync
	// path.
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// Subtests are self-contained — each uses its own domain / addresses
	// so they can share the one OxiDB instance without colliding.

	t.Run("domains", func(t *testing.T) {
		d, err := st.CreateDomain("domains.test")
		if err != nil {
			t.Fatalf("create domain: %v", err)
		}
		if d.ID == 0 || d.Domain != "domains.test" || !d.Active {
			t.Fatalf("unexpected domain: %+v", d)
		}
		got, err := st.GetDomain("domains.test")
		if err != nil {
			t.Fatalf("get domain: %v", err)
		}
		if got.ID != d.ID {
			t.Fatalf("get domain id = %d, want %d", got.ID, d.ID)
		}
		if _, err := st.GetDomain("missing.test"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get missing domain: err = %v, want ErrNotFound", err)
		}
		if _, err := st.CreateDomain("domains.test"); err == nil {
			t.Fatal("duplicate domain was allowed — unique index not enforced")
		}
	})

	t.Run("accounts", func(t *testing.T) {
		a, err := st.CreateAccount("alice@accounts.test", "hash-1", 1000)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		if a.ID == 0 || a.Domain != "accounts.test" || a.QuotaBytes != 1000 || a.UsedBytes != 0 {
			t.Fatalf("unexpected account: %+v", a)
		}
		byAddr, err := st.GetAccount("alice@accounts.test")
		if err != nil {
			t.Fatalf("get account: %v", err)
		}
		byID, err := st.GetAccountByID(a.ID)
		if err != nil {
			t.Fatalf("get account by id: %v", err)
		}
		if byAddr.ID != a.ID || byID.Address != a.Address {
			t.Fatalf("account lookups disagree: byAddr=%+v byID=%+v", byAddr, byID)
		}
		if _, err := st.GetAccount("nobody@accounts.test"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get missing account: err = %v, want ErrNotFound", err)
		}
		if _, err := st.CreateAccount("not-an-email", "h", 0); err == nil {
			t.Fatal("malformed address was accepted")
		}
	})

	t.Run("aliases and ResolveRecipient", func(t *testing.T) {
		bob, err := st.CreateAccount("bob@resolve.test", "h", 0)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		if _, err := st.CreateAlias("team@resolve.test",
			[]string{"bob@resolve.test", "remote@elsewhere.test"}); err != nil {
			t.Fatalf("create alias: %v", err)
		}

		direct, err := st.ResolveRecipient("bob@resolve.test")
		if err != nil {
			t.Fatalf("resolve direct: %v", err)
		}
		if len(direct) != 1 || direct[0] != bob.ID {
			t.Fatalf("resolve direct = %v, want [%d]", direct, bob.ID)
		}

		viaAlias, err := st.ResolveRecipient("team@resolve.test")
		if err != nil {
			t.Fatalf("resolve alias: %v", err)
		}
		if len(viaAlias) != 1 || viaAlias[0] != bob.ID {
			t.Fatalf("resolve alias = %v, want [%d] (remote dest skipped)", viaAlias, bob.ID)
		}

		none, err := st.ResolveRecipient("ghost@resolve.test")
		if err != nil {
			t.Fatalf("resolve missing: %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("resolve missing = %v, want empty", none)
		}
	})

	t.Run("mailboxes", func(t *testing.T) {
		acc, err := st.CreateAccount("m@mailbox.test", "h", 0)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
			t.Fatalf("ensure default mailboxes: %v", err)
		}
		boxes, err := st.ListMailboxes(acc.ID)
		if err != nil {
			t.Fatalf("list mailboxes: %v", err)
		}
		if len(boxes) != 6 {
			t.Fatalf("default mailboxes = %d, want 6 (%v)", len(boxes), mailboxNames(boxes))
		}
		inbox, err := st.GetMailboxByName(acc.ID, "INBOX")
		if err != nil {
			t.Fatalf("get INBOX: %v", err)
		}
		if inbox.UIDNext != 1 || inbox.UIDValidity == 0 {
			t.Fatalf("fresh INBOX: uidnext=%d uidvalidity=%d", inbox.UIDNext, inbox.UIDValidity)
		}
		// Idempotent.
		if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
			t.Fatalf("ensure default mailboxes (2nd): %v", err)
		}
		if boxes, _ := st.ListMailboxes(acc.ID); len(boxes) != 6 {
			t.Fatalf("EnsureDefaultMailboxes not idempotent: %d mailboxes", len(boxes))
		}
		if _, err := st.CreateMailbox(acc.ID, "Work"); err != nil {
			t.Fatalf("create mailbox: %v", err)
		}
		if boxes, _ := st.ListMailboxes(acc.ID); len(boxes) != 7 {
			t.Fatalf("after CreateMailbox: %d mailboxes, want 7", len(boxes))
		}
	})

	t.Run("NextUID is monotonic and concurrent-safe", func(t *testing.T) {
		acc, err := st.CreateAccount("u@uid.test", "h", 0)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		mb, err := st.CreateMailbox(acc.ID, "INBOX")
		if err != nil {
			t.Fatalf("create mailbox: %v", err)
		}

		const (
			writers   = 8
			perWriter = 100
		)
		var (
			mu  sync.Mutex
			all []uint32
			wg  sync.WaitGroup
		)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got := make([]uint32, 0, perWriter)
				for j := 0; j < perWriter; j++ {
					uid, err := st.NextUID(mb.ID)
					if err != nil {
						t.Errorf("NextUID: %v", err)
						return
					}
					got = append(got, uid)
				}
				mu.Lock()
				all = append(all, got...)
				mu.Unlock()
			}()
		}
		wg.Wait()

		// Every allocation must be unique, and the set must be exactly
		// 1..writers*perWriter — no UID lost, none handed out twice.
		seen := make(map[uint32]bool, len(all))
		for _, uid := range all {
			if seen[uid] {
				t.Fatalf("UID %d handed out more than once", uid)
			}
			seen[uid] = true
		}
		total := writers * perWriter
		if len(seen) != total {
			t.Fatalf("allocated %d distinct UIDs, want %d", len(seen), total)
		}
		for uid := uint32(1); uid <= uint32(total); uid++ {
			if !seen[uid] {
				t.Fatalf("UID %d was never allocated — an increment was lost", uid)
			}
		}
	})

	t.Run("messages: append, fetch, list", func(t *testing.T) {
		acc, err := st.CreateAccount("msg@message.test", "h", 0)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
			t.Fatalf("ensure mailboxes: %v", err)
		}
		inbox, err := st.GetMailboxByName(acc.ID, "INBOX")
		if err != nil {
			t.Fatalf("get INBOX: %v", err)
		}

		raw1 := []byte("From: a@b.test\r\nSubject: First\r\n\r\nhello one")
		m1, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
			Raw: raw1, MessageID: "<1@b.test>", Subject: "First", FromAddr: "a@b.test",
		})
		if err != nil {
			t.Fatalf("append message 1: %v", err)
		}
		if m1.UID != 1 || m1.SizeBytes != int64(len(raw1)) || m1.AccountID != acc.ID {
			t.Fatalf("unexpected message 1: %+v", m1)
		}
		m2, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
			Raw: []byte("From: c@d.test\r\nSubject: Second\r\n\r\nhello two"), Subject: "Second",
		})
		if err != nil {
			t.Fatalf("append message 2: %v", err)
		}
		if m2.UID != 2 {
			t.Fatalf("message 2 UID = %d, want 2", m2.UID)
		}

		got, err := st.GetMessage(m1.ID)
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		if got.Subject != "First" || got.UID != 1 {
			t.Fatalf("get message returned %+v", got)
		}
		body, err := st.FetchBody(m1)
		if err != nil {
			t.Fatalf("fetch body: %v", err)
		}
		if !bytes.Equal(body, raw1) {
			t.Fatalf("fetched body does not round-trip: got %q want %q", body, raw1)
		}

		list, err := st.ListMessages(inbox.ID)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		if len(list) != 2 || list[0].UID != 1 || list[1].UID != 2 {
			t.Fatalf("list messages = %d entries, not UID-ordered: %+v", len(list), list)
		}
	})

	t.Run("flags", func(t *testing.T) {
		acc, _ := st.CreateAccount("flags@flag.test", "h", 0)
		mb, _ := st.CreateMailbox(acc.ID, "INBOX")
		m, err := st.AppendMessage(mb.ID, store.IncomingMessage{Raw: []byte("x")})
		if err != nil {
			t.Fatalf("append: %v", err)
		}

		if err := st.AddFlags(m.ID, `\Seen`); err != nil {
			t.Fatalf("add flag: %v", err)
		}
		if err := st.AddFlags(m.ID, `\Flagged`, `\Answered`); err != nil {
			t.Fatalf("add flags: %v", err)
		}
		if got := flagsOf(t, st, m.ID); !hasAll(got, `\Seen`, `\Flagged`, `\Answered`) {
			t.Fatalf("after AddFlags: %v", got)
		}
		if err := st.RemoveFlags(m.ID, `\Seen`); err != nil {
			t.Fatalf("remove flag: %v", err)
		}
		if got := flagsOf(t, st, m.ID); hasAll(got, `\Seen`) {
			t.Fatalf("after RemoveFlags \\Seen still present: %v", got)
		}
		if err := st.SetFlags(m.ID, []string{`\Draft`}); err != nil {
			t.Fatalf("set flags: %v", err)
		}
		if got := flagsOf(t, st, m.ID); len(got) != 1 || got[0] != `\Draft` {
			t.Fatalf("after SetFlags = %v, want [\\Draft]", got)
		}
	})

	t.Run("delete message removes the body blob", func(t *testing.T) {
		acc, _ := st.CreateAccount("del@delete.test", "h", 0)
		mb, _ := st.CreateMailbox(acc.ID, "INBOX")
		m, err := st.AppendMessage(mb.ID, store.IncomingMessage{Raw: []byte("to be deleted")})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := st.DeleteMessage(m.ID); err != nil {
			t.Fatalf("delete message: %v", err)
		}
		if _, err := st.GetMessage(m.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get deleted message: err = %v, want ErrNotFound", err)
		}
		if _, err := st.FetchBody(m); err == nil {
			t.Fatal("body blob still readable after DeleteMessage")
		}
	})

	t.Run("DeliverTo files into the named folder (Junk for quarantine)", func(t *testing.T) {
		acc, err := st.CreateAccount("quarantine@deliver.test", "h", 0)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		// No EnsureDefaultMailboxes — DeliverTo must auto-create them.
		raw := []byte("From: s@x.test\r\nSubject: spammy\r\n\r\n")
		m, err := st.DeliverTo(acc.ID, "Junk", store.IncomingMessage{
			Raw: raw, Subject: "spammy", FromAddr: "s@x.test",
		})
		if err != nil {
			t.Fatalf("DeliverTo Junk: %v", err)
		}
		junk, err := st.GetMailboxByName(acc.ID, "Junk")
		if err != nil {
			t.Fatalf("get Junk: %v", err)
		}
		if m.MailboxID != junk.ID {
			t.Errorf("message landed in mailbox %d, want Junk (%d)", m.MailboxID, junk.ID)
		}
		// INBOX is empty.
		inbox, _ := st.GetMailboxByName(acc.ID, "INBOX")
		if msgs, _ := st.ListMessages(inbox.ID); len(msgs) != 0 {
			t.Errorf("INBOX has %d messages, want 0 (the Junk delivery should not have leaked here)", len(msgs))
		}
	})

	t.Run("DeleteAccount cascades to mailboxes, messages, and blobs", func(t *testing.T) {
		acc, err := st.CreateAccount("cascade@cascade.test", "h", 0)
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
			t.Fatalf("ensure mailboxes: %v", err)
		}
		inbox, _ := st.GetMailboxByName(acc.ID, "INBOX")
		var saved *store.Message
		for i := 0; i < 3; i++ {
			m, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
				Raw: []byte(fmt.Sprintf("message %d", i)),
			})
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			saved = m
		}

		if err := st.DeleteAccount(acc.ID); err != nil {
			t.Fatalf("delete account: %v", err)
		}
		if _, err := st.GetAccountByID(acc.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("account still present: %v", err)
		}
		if boxes, err := st.ListMailboxes(acc.ID); err != nil || len(boxes) != 0 {
			t.Fatalf("mailboxes not cascaded: %d (%v)", len(boxes), err)
		}
		if msgs, err := st.ListMessages(inbox.ID); err != nil || len(msgs) != 0 {
			t.Fatalf("messages not cascaded: %d (%v)", len(msgs), err)
		}
		if _, err := st.FetchBody(saved); err == nil {
			t.Fatal("a body blob survived DeleteAccount")
		}
	})

	t.Run("admin list and delete operations", func(t *testing.T) {
		if _, err := st.CreateDomain("adminlist.test"); err != nil {
			t.Fatalf("create domain: %v", err)
		}
		domains, err := st.ListDomains()
		if err != nil {
			t.Fatalf("list domains: %v", err)
		}
		if !domainListed(domains, "adminlist.test") {
			t.Fatal("ListDomains is missing the created domain")
		}

		// DKIM key storage.
		if err := st.SetDKIMKey("adminlist.test", "sel", "PEM-DATA"); err != nil {
			t.Fatalf("set DKIM key: %v", err)
		}
		d, err := st.GetDomain("adminlist.test")
		if err != nil {
			t.Fatalf("get domain: %v", err)
		}
		if d.DKIMSelector != "sel" || d.DKIMPrivateKey != "PEM-DATA" {
			t.Fatalf("DKIM key not stored: selector=%q key=%q", d.DKIMSelector, d.DKIMPrivateKey)
		}
		if err := st.SetDKIMKey("no-such-domain.test", "s", "k"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("SetDKIMKey on a missing domain: err = %v, want ErrNotFound", err)
		}

		// Two accounts in a domain unique to this subtest, so the
		// per-domain filter has an exact expected count.
		for _, addr := range []string{"u1@adminaccts.test", "u2@adminaccts.test"} {
			if _, err := st.CreateAccount(addr, "h", 0); err != nil {
				t.Fatalf("create account %s: %v", addr, err)
			}
		}
		inDomain, err := st.ListAccounts("adminaccts.test")
		if err != nil {
			t.Fatalf("list accounts by domain: %v", err)
		}
		if len(inDomain) != 2 {
			t.Fatalf("ListAccounts(adminaccts.test) = %d, want 2", len(inDomain))
		}
		all, err := st.ListAccounts("")
		if err != nil {
			t.Fatalf("list all accounts: %v", err)
		}
		if len(all) < len(inDomain) {
			t.Fatalf("ListAccounts(\"\") = %d, want at least %d", len(all), len(inDomain))
		}

		if _, err := st.CreateAlias("team@adminaccts.test", []string{"u1@adminaccts.test"}); err != nil {
			t.Fatalf("create alias: %v", err)
		}
		aliases, err := st.ListAliases()
		if err != nil {
			t.Fatalf("list aliases: %v", err)
		}
		if !aliasListed(aliases, "team@adminaccts.test") {
			t.Fatal("ListAliases is missing the created alias")
		}
		if err := st.DeleteAlias("team@adminaccts.test"); err != nil {
			t.Fatalf("delete alias: %v", err)
		}
		if _, err := st.GetAlias("team@adminaccts.test"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("alias still present after delete: err = %v", err)
		}
	})
}

// flagsOf re-reads a message and returns its flags.
func flagsOf(t *testing.T, st *store.Store, id uint64) []string {
	t.Helper()
	m, err := st.GetMessage(id)
	if err != nil {
		t.Fatalf("get message %d: %v", id, err)
	}
	return m.Flags
}

func hasAll(flags []string, want ...string) bool {
	set := make(map[string]bool, len(flags))
	for _, f := range flags {
		set[f] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func mailboxNames(boxes []store.Mailbox) []string {
	names := make([]string, len(boxes))
	for i, b := range boxes {
		names[i] = b.Name
	}
	return names
}

func domainListed(domains []store.Domain, name string) bool {
	for _, d := range domains {
		if d.Domain == name {
			return true
		}
	}
	return false
}

func aliasListed(aliases []store.Alias, address string) bool {
	for _, a := range aliases {
		if a.Address == address {
			return true
		}
	}
	return false
}
