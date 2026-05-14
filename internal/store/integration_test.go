//go:build integration

// Integration tests for the store layer, run against a live
// oxidb-server. They are gated behind the `integration` build tag and
// excluded from a plain `go test ./...`:
//
//	go test -tags=integration ./internal/store/...
//
// The test boots its own oxidb-server in a temp directory. It looks for
// the binary at $OXIDB_BIN, then at ../../../docdb/target/{release,debug}
// /oxidb-server; if none is found the test is skipped (not failed).
package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/store"
)

func TestStore(t *testing.T) {
	host, port := startOxiDB(t)

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
		if len(boxes) != 5 {
			t.Fatalf("default mailboxes = %d, want 5 (%v)", len(boxes), mailboxNames(boxes))
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
		if boxes, _ := st.ListMailboxes(acc.ID); len(boxes) != 5 {
			t.Fatalf("EnsureDefaultMailboxes not idempotent: %d mailboxes", len(boxes))
		}
		if _, err := st.CreateMailbox(acc.ID, "Work"); err != nil {
			t.Fatalf("create mailbox: %v", err)
		}
		if boxes, _ := st.ListMailboxes(acc.ID); len(boxes) != 6 {
			t.Fatalf("after CreateMailbox: %d mailboxes, want 6", len(boxes))
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

// -----------------------------------------------------------------------
// oxidb-server harness
// -----------------------------------------------------------------------

// startOxiDB boots an oxidb-server in a fresh temp directory on a free
// port and registers its teardown with t. If the server binary cannot
// be found the test is skipped, not failed.
func startOxiDB(t *testing.T) (host string, port int) {
	t.Helper()
	bin := findOxiDBBinary(t)

	port = freePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("OXIDB_ADDR=127.0.0.1:%d", port),
		"OXIDB_DATA="+t.TempDir(),
		"OXIDB_S3_PORT=0",
		"OXIDB_IDLE_TIMEOUT=120",
	)
	cmd.Stdout = os.Stderr // surfaced by `go test` only on failure
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start oxidb-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	waitReady(t, port)
	return "127.0.0.1", port
}

// findOxiDBBinary locates the oxidb-server binary: $OXIDB_BIN, then the
// release and debug build outputs of the sibling OxiDB checkout.
func findOxiDBBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("OXIDB_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
		t.Fatalf("OXIDB_BIN=%s does not exist", bin)
	}
	for _, p := range []string{
		"../../../docdb/target/release/oxidb-server",
		"../../../docdb/target/debug/oxidb-server",
	} {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Skip("oxidb-server binary not found — set OXIDB_BIN or build it (cargo build -p oxidb-server)")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitReady(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("oxidb-server did not become ready on :%d within 10s", port)
}
