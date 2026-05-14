//go:build integration

// Integration test for the IMAP server. It boots a live oxidb-server,
// starts the IMAP server against it, and drives it with a real IMAP
// client. Gated behind the `integration` build tag:
//
//	go test -tags=integration ./internal/imap/...
package imap_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/parisxmas/OxiMail/internal/imap"
	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/store"
)

const (
	testAddr     = "user@oximail.test"
	testPassword = "s3cret"
)

func TestIMAP(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// One account with the default folders, and one message already in
	// its INBOX so SELECT/FETCH have something to work with.
	hash, err := store.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	acc, err := st.CreateAccount(testAddr, hash, 0)
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
	rawMsg := []byte("From: Sender <sender@elsewhere.test>\r\n" +
		"To: " + testAddr + "\r\n" +
		"Subject: Hello IMAP\r\n" +
		"Message-Id: <imap-1@elsewhere.test>\r\n" +
		"\r\n" +
		"the message body\r\n")
	if _, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
		Raw: rawMsg, Subject: "Hello IMAP", MessageID: "imap-1@elsewhere.test",
		FromAddr: "sender@elsewhere.test",
	}); err != nil {
		t.Fatalf("seed INBOX message: %v", err)
	}

	addr := startIMAP(t, st, nil, false)

	t.Run("rejects a bad password", func(t *testing.T) {
		c := dial(t, addr)
		defer c.Close()
		if err := c.Login(testAddr, "wrong-password").Wait(); err == nil {
			t.Fatal("login with a bad password succeeded, want rejection")
		}
	})

	// The remaining subtests share one authenticated connection and run
	// in order — SELECT before FETCH, STORE before EXPUNGE.
	c := dial(t, addr)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login(testAddr, testPassword).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	t.Run("LIST returns the default mailboxes", func(t *testing.T) {
		boxes, err := c.List("", "*", nil).Collect()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(boxes) != 5 {
			names := make([]string, len(boxes))
			for i, b := range boxes {
				names[i] = b.Mailbox
			}
			t.Fatalf("LIST returned %d mailboxes, want 5: %v", len(boxes), names)
		}
	})

	t.Run("SELECT INBOX", func(t *testing.T) {
		data, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if data.NumMessages != 1 {
			t.Fatalf("INBOX NumMessages = %d, want 1", data.NumMessages)
		}
		if data.UIDValidity == 0 {
			t.Fatal("INBOX UIDVALIDITY is 0")
		}
	})

	t.Run("FETCH a message", func(t *testing.T) {
		// Peek so the fetch does not implicitly set \Seen — the STORE
		// subtest below owns that.
		msgs, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{
			UID: true, Flags: true, Envelope: true, RFC822Size: true,
			BodySection: []*goimap.FetchItemBodySection{{Peek: true}},
		}).Collect()
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("FETCH returned %d messages, want 1", len(msgs))
		}
		m := msgs[0]
		if m.UID != 1 {
			t.Errorf("UID = %d, want 1", m.UID)
		}
		if m.Envelope == nil || m.Envelope.Subject != "Hello IMAP" {
			t.Errorf("envelope subject = %+v, want \"Hello IMAP\"", m.Envelope)
		}
		if m.RFC822Size != int64(len(rawMsg)) {
			t.Errorf("RFC822Size = %d, want %d", m.RFC822Size, len(rawMsg))
		}
		if len(m.BodySection) != 1 || !bytes.Equal(m.BodySection[0].Bytes, rawMsg) {
			t.Errorf("BODY[] did not round-trip the raw message")
		}
	})

	t.Run("STORE +FLAGS \\Seen", func(t *testing.T) {
		msgs, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{
			Op:    goimap.StoreFlagsAdd,
			Flags: []goimap.Flag{goimap.FlagSeen},
		}, nil).Collect()
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		if len(msgs) != 1 || !hasIMAPFlag(msgs[0].Flags, goimap.FlagSeen) {
			t.Fatalf("STORE response flags = %v, want \\Seen present", msgs)
		}
		// Verify it was persisted, not just echoed.
		stored, err := st.ListMessages(inbox.ID)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		if len(stored) != 1 || !hasStringFlag(stored[0].Flags, `\Seen`) {
			t.Fatalf("\\Seen not persisted to the store: %v", stored)
		}
	})

	t.Run("APPEND to Drafts", func(t *testing.T) {
		draft := []byte("From: " + testAddr + "\r\nSubject: A draft\r\n\r\nwork in progress\r\n")
		ac := c.Append("Drafts", int64(len(draft)), nil)
		if _, err := ac.Write(draft); err != nil {
			t.Fatalf("append write: %v", err)
		}
		if err := ac.Close(); err != nil {
			t.Fatalf("append close: %v", err)
		}
		if _, err := ac.Wait(); err != nil {
			t.Fatalf("append wait: %v", err)
		}

		drafts, err := st.GetMailboxByName(acc.ID, "Drafts")
		if err != nil {
			t.Fatalf("get Drafts: %v", err)
		}
		msgs, err := st.ListMessages(drafts.ID)
		if err != nil {
			t.Fatalf("list Drafts: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Subject != "A draft" {
			t.Fatalf("Drafts has %d messages: %+v", len(msgs), msgs)
		}
	})

	t.Run("EXPUNGE a \\Deleted message", func(t *testing.T) {
		// INBOX is still the selected mailbox. Mark its one message
		// \Deleted, then expunge it.
		if _, err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{
			Op:     goimap.StoreFlagsAdd,
			Flags:  []goimap.Flag{goimap.FlagDeleted},
			Silent: true,
		}, nil).Collect(); err != nil {
			t.Fatalf("store \\Deleted: %v", err)
		}
		seqNums, err := c.Expunge().Collect()
		if err != nil {
			t.Fatalf("expunge: %v", err)
		}
		if len(seqNums) != 1 || seqNums[0] != 1 {
			t.Fatalf("EXPUNGE reported %v, want [1]", seqNums)
		}
		if msgs, err := st.ListMessages(inbox.ID); err != nil || len(msgs) != 0 {
			t.Fatalf("message not removed from the store: %d (%v)", len(msgs), err)
		}
		// A fresh SELECT should now see an empty INBOX.
		data, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("re-select: %v", err)
		}
		if data.NumMessages != 0 {
			t.Fatalf("INBOX NumMessages = %d after expunge, want 0", data.NumMessages)
		}
	})
}

// TestIMAPTLS covers the IMAP server with TLS configured: STARTTLS on
// the plaintext listener, the implicit-TLS listener, and that cleartext
// LOGIN is refused once TLS is available.
func TestIMAPTLS(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	acc, err := st.CreateAccount("tls-user@oximail.test", hash, 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("ensure mailboxes: %v", err)
	}

	certFile, keyFile := itest.WriteSelfSignedCert(t)
	serverTLS := itest.ServerTLS(t, certFile, keyFile)
	clientOpts := &imapclient.Options{TLSConfig: &tls.Config{InsecureSkipVerify: true}}

	t.Run("STARTTLS login and select", func(t *testing.T) {
		addr := startIMAP(t, st, serverTLS, false)
		c, err := imapclient.DialStartTLS(addr, clientOpts)
		if err != nil {
			t.Fatalf("dial STARTTLS: %v", err)
		}
		defer c.Close()
		if err := c.Login("tls-user@oximail.test", "s3cret").Wait(); err != nil {
			t.Fatalf("login over STARTTLS: %v", err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("select over STARTTLS: %v", err)
		}
	})

	t.Run("implicit TLS login", func(t *testing.T) {
		addr := startIMAP(t, st, serverTLS, true)
		c, err := imapclient.DialTLS(addr, clientOpts)
		if err != nil {
			t.Fatalf("dial implicit TLS: %v", err)
		}
		defer c.Close()
		if err := c.Login("tls-user@oximail.test", "s3cret").Wait(); err != nil {
			t.Fatalf("login over implicit TLS: %v", err)
		}
	})

	t.Run("cleartext login refused when TLS is available", func(t *testing.T) {
		addr := startIMAP(t, st, serverTLS, false)
		c, err := imapclient.DialInsecure(addr, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if err := c.Login("tls-user@oximail.test", "s3cret").Wait(); err == nil {
			t.Error("cleartext LOGIN succeeded, want rejection while TLS is available")
		}
	})
}

// TestIMAPSearch covers the SEARCH command: metadata criteria (flags,
// size, sequence) over the snapshot, and header / body criteria that
// read the message from the blob store.
func TestIMAPSearch(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	hash, err := store.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	acc, err := st.CreateAccount(testAddr, hash, 0)
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

	// Seed INBOX with three messages of varied sender, subject, body,
	// size, and flags — they become sequence numbers 1, 2, 3.
	seed := func(from, subject, body string, flags []string) *store.Message {
		t.Helper()
		raw := []byte("From: " + from + "\r\nTo: " + testAddr + "\r\n" +
			"Subject: " + subject + "\r\nDate: Wed, 14 May 2025 10:00:00 +0000\r\n" +
			"\r\n" + body + "\r\n")
		m, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
			Raw: raw, Subject: subject, FromAddr: from, Flags: flags,
		})
		if err != nil {
			t.Fatalf("seed message: %v", err)
		}
		return m
	}
	seed("alice@partners.test", "Quarterly report", "the revenue numbers look strong", nil)
	m2 := seed("bob@partners.test", "Lunch tomorrow?", "want to grab lunch", []string{`\Seen`})
	seed("alice@partners.test", "Re: Quarterly report", "thanks — "+strings.Repeat("padding ", 400), nil)

	addr := startIMAP(t, st, nil, false)
	c := dial(t, addr)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login(testAddr, testPassword).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select INBOX: %v", err)
	}

	// searchSeq runs a SEARCH and returns the matched sequence numbers.
	searchSeq := func(t *testing.T, criteria *goimap.SearchCriteria) []uint32 {
		t.Helper()
		data, err := c.Search(criteria, nil).Wait()
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		return data.AllSeqNums()
	}

	t.Run("ALL", func(t *testing.T) {
		if got := searchSeq(t, &goimap.SearchCriteria{}); len(got) != 3 {
			t.Fatalf("SEARCH ALL = %v, want 3 messages", got)
		}
	})

	t.Run("FROM", func(t *testing.T) {
		got := searchSeq(t, &goimap.SearchCriteria{
			Header: []goimap.SearchCriteriaHeaderField{{Key: "From", Value: "alice"}},
		})
		if !sameSet(got, []uint32{1, 3}) {
			t.Fatalf("SEARCH FROM alice = %v, want [1 3]", got)
		}
	})

	t.Run("SUBJECT", func(t *testing.T) {
		got := searchSeq(t, &goimap.SearchCriteria{
			Header: []goimap.SearchCriteriaHeaderField{{Key: "Subject", Value: "report"}},
		})
		if !sameSet(got, []uint32{1, 3}) {
			t.Fatalf("SEARCH SUBJECT report = %v, want [1 3]", got)
		}
	})

	t.Run("SEEN and UNSEEN", func(t *testing.T) {
		if got := searchSeq(t, &goimap.SearchCriteria{Flag: []goimap.Flag{goimap.FlagSeen}}); !sameSet(got, []uint32{2}) {
			t.Fatalf("SEARCH SEEN = %v, want [2]", got)
		}
		if got := searchSeq(t, &goimap.SearchCriteria{NotFlag: []goimap.Flag{goimap.FlagSeen}}); !sameSet(got, []uint32{1, 3}) {
			t.Fatalf("SEARCH UNSEEN = %v, want [1 3]", got)
		}
	})

	t.Run("BODY", func(t *testing.T) {
		if got := searchSeq(t, &goimap.SearchCriteria{Body: []string{"lunch"}}); !sameSet(got, []uint32{2}) {
			t.Fatalf("SEARCH BODY lunch = %v, want [2]", got)
		}
	})

	t.Run("LARGER", func(t *testing.T) {
		// m3's padded body makes it far larger than the other two.
		if got := searchSeq(t, &goimap.SearchCriteria{Larger: 1000}); !sameSet(got, []uint32{3}) {
			t.Fatalf("SEARCH LARGER 1000 = %v, want [3]", got)
		}
	})

	t.Run("NOT", func(t *testing.T) {
		got := searchSeq(t, &goimap.SearchCriteria{
			Not: []goimap.SearchCriteria{{
				Header: []goimap.SearchCriteriaHeaderField{{Key: "From", Value: "alice"}},
			}},
		})
		if !sameSet(got, []uint32{2}) {
			t.Fatalf("SEARCH NOT FROM alice = %v, want [2]", got)
		}
	})

	t.Run("OR", func(t *testing.T) {
		got := searchSeq(t, &goimap.SearchCriteria{
			Or: [][2]goimap.SearchCriteria{{
				{Body: []string{"lunch"}},
				{Body: []string{"revenue"}},
			}},
		})
		if !sameSet(got, []uint32{1, 2}) {
			t.Fatalf("SEARCH OR BODY lunch BODY revenue = %v, want [1 2]", got)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		if got := searchSeq(t, &goimap.SearchCriteria{
			Header: []goimap.SearchCriteriaHeaderField{{Key: "From", Value: "nobody"}},
		}); len(got) != 0 {
			t.Fatalf("SEARCH FROM nobody = %v, want no matches", got)
		}
	})

	t.Run("UID SEARCH", func(t *testing.T) {
		data, err := c.UIDSearch(&goimap.SearchCriteria{
			Header: []goimap.SearchCriteriaHeaderField{{Key: "From", Value: "bob"}},
		}, nil).Wait()
		if err != nil {
			t.Fatalf("uid search: %v", err)
		}
		uids := data.AllUIDs()
		if len(uids) != 1 || uids[0] != goimap.UID(m2.UID) {
			t.Fatalf("UID SEARCH FROM bob = %v, want [%d]", uids, m2.UID)
		}
	})
}

// startIMAP launches the IMAP server on a free port, wired to st, and
// returns its address. A non-nil tlsConfig enables STARTTLS; implicit
// additionally serves implicit TLS (IMAPS). It is shut down when the
// test finishes.
func startIMAP(t *testing.T, st *store.Store, tlsConfig *tls.Config, implicit bool) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	var srv *imap.Server
	if implicit {
		srv = imap.NewTLS(addr, st, tlsConfig)
	} else {
		srv = imap.New(addr, st, tlsConfig)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("imap server exited with: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("imap server did not stop within 5s")
		}
	})

	itest.WaitTCP(t, addr)
	return addr
}

func dial(t *testing.T, addr string) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial imap: %v", err)
	}
	return c
}

func hasIMAPFlag(flags []goimap.Flag, want goimap.Flag) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func hasStringFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// sameSet reports whether got and want hold the same uint32 values,
// regardless of order.
func sameSet(got, want []uint32) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[uint32]int, len(got))
	for _, v := range got {
		counts[v]++
	}
	for _, v := range want {
		counts[v]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
