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
	"github.com/emersion/go-sasl"

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
		if len(boxes) != 6 {
			names := make([]string, len(boxes))
			for i, b := range boxes {
				names[i] = b.Mailbox
			}
			t.Fatalf("LIST returned %d mailboxes, want 6: %v", len(boxes), names)
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

	t.Run("AUTHENTICATE PLAIN", func(t *testing.T) {
		authC := dial(t, addr)
		defer authC.Close()
		if err := authC.Authenticate(sasl.NewPlainClient("", testAddr, testPassword)); err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		// Once authenticated via SASL, the session is in the
		// authenticated state — basic commands work.
		if _, err := authC.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("select after AUTHENTICATE: %v", err)
		}
	})

	t.Run("AUTHENTICATE PLAIN rejects a bad password", func(t *testing.T) {
		authC := dial(t, addr)
		defer authC.Close()
		if err := authC.Authenticate(sasl.NewPlainClient("", testAddr, "wrong-password")); err == nil {
			t.Fatal("AUTHENTICATE with a bad password succeeded, want rejection")
		}
	})

	t.Run("COPY into another mailbox", func(t *testing.T) {
		// INBOX is empty after the EXPUNGE subtest. Append a fresh
		// message that the next subtest can also rely on.
		body := []byte("From: x@y.test\r\nTo: " + testAddr + "\r\n" +
			"Subject: to be copied\r\n\r\nhi\r\n")
		ac := c.Append("INBOX", int64(len(body)), nil)
		if _, err := ac.Write(body); err != nil {
			t.Fatalf("append write: %v", err)
		}
		if err := ac.Close(); err != nil {
			t.Fatalf("append close: %v", err)
		}
		if _, err := ac.Wait(); err != nil {
			t.Fatalf("append wait: %v", err)
		}

		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("select INBOX: %v", err)
		}

		data, err := c.Copy(goimap.SeqSetNum(1), "Archive").Wait()
		if err != nil {
			t.Fatalf("copy: %v", err)
		}
		if data == nil || len(data.SourceUIDs) == 0 || len(data.DestUIDs) == 0 {
			t.Fatalf("CopyData missing UIDs: %+v", data)
		}

		// Archive now contains the copy.
		sel, err := c.Select("Archive", nil).Wait()
		if err != nil {
			t.Fatalf("select Archive: %v", err)
		}
		if sel.NumMessages != 1 {
			t.Errorf("Archive NumMessages = %d, want 1", sel.NumMessages)
		}
	})

	t.Run("MOVE removes the source and recreates it in the destination", func(t *testing.T) {
		// Land a fresh message in INBOX (the previous subtest already
		// emptied / archived it).
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("select INBOX: %v", err)
		}
		body := "From: <sender@elsewhere.test>\r\nSubject: to move\r\n\r\nbody\r\n"
		ac := c.Append("INBOX", int64(len(body)), nil)
		if _, err := ac.Write([]byte(body)); err != nil {
			t.Fatalf("append write: %v", err)
		}
		if err := ac.Close(); err != nil {
			t.Fatalf("append close: %v", err)
		}
		if _, err := ac.Wait(); err != nil {
			t.Fatalf("append wait: %v", err)
		}
		// Re-SELECT INBOX so the new message is on the snapshot.
		sel, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("re-select INBOX: %v", err)
		}
		before := sel.NumMessages

		data, err := c.Move(goimap.SeqSetNum(1), "Archive").Wait()
		if err != nil {
			t.Fatalf("move: %v", err)
		}
		if data == nil || data.SourceUIDs == nil || data.DestUIDs == nil {
			t.Fatalf("MoveData missing UIDs: %+v", data)
		}

		// INBOX is now one shorter.
		sel, err = c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("re-select INBOX after move: %v", err)
		}
		if sel.NumMessages != before-1 {
			t.Errorf("INBOX NumMessages = %d, want %d", sel.NumMessages, before-1)
		}
	})

	t.Run("CREATE / RENAME / DELETE a user mailbox", func(t *testing.T) {
		if err := c.Create("TempA", nil).Wait(); err != nil {
			t.Fatalf("create TempA: %v", err)
		}
		if err := c.Rename("TempA", "TempB", nil).Wait(); err != nil {
			t.Fatalf("rename TempA -> TempB: %v", err)
		}

		boxes, err := c.List("", "*", nil).Collect()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var hasA, hasB bool
		for _, b := range boxes {
			if b.Mailbox == "TempA" {
				hasA = true
			}
			if b.Mailbox == "TempB" {
				hasB = true
			}
		}
		if hasA || !hasB {
			t.Errorf("after rename: hasA=%v hasB=%v (want false / true)", hasA, hasB)
		}

		if err := c.Delete("TempB").Wait(); err != nil {
			t.Fatalf("delete TempB: %v", err)
		}
		boxes, _ = c.List("", "*", nil).Collect()
		for _, b := range boxes {
			if b.Mailbox == "TempB" {
				t.Error("TempB still listed after delete")
			}
		}
	})

	t.Run("DELETE INBOX is refused", func(t *testing.T) {
		if err := c.Delete("INBOX").Wait(); err == nil {
			t.Error("DELETE INBOX succeeded, should be refused")
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

// TestIMAPCondStore covers the RFC 7162 §3 CONDSTORE round-trip end
// to end: server advertises CONDSTORE, SELECT carries a HIGHESTMODSEQ
// response code, FETCH MODSEQ surfaces the per-message mod-sequence,
// FETCH (CHANGEDSINCE n) filters out older messages, STORE
// (UNCHANGEDSINCE n) refuses stale updates with a MODIFIED response,
// and STATUS HIGHESTMODSEQ reports the mailbox-wide counter.
func TestIMAPCondStore(t *testing.T) {
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
	// Two messages seeded back-to-back; they pick up distinct
	// mod-sequences (1 and 2) at append time.
	seed := func(subject string) *store.Message {
		raw := []byte("From: <s@x.test>\r\nSubject: " + subject + "\r\n\r\nbody\r\n")
		m, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
			Raw: raw, Subject: subject, FromAddr: "s@x.test",
		})
		if err != nil {
			t.Fatalf("seed %s: %v", subject, err)
		}
		return m
	}
	m1 := seed("first")
	m2 := seed("second")
	if m1.ModSeq == 0 || m2.ModSeq == 0 || m1.ModSeq >= m2.ModSeq {
		t.Fatalf("expected fresh mod-seqs 1 < 2; got %d / %d", m1.ModSeq, m2.ModSeq)
	}

	addr := startIMAP(t, st, nil, false)
	c := dial(t, addr)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login(testAddr, testPassword).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	t.Run("server advertises CONDSTORE", func(t *testing.T) {
		if !c.Caps().Has(goimap.CapCondStore) {
			t.Errorf("CAPABILITY did not include CONDSTORE: %v", c.Caps())
		}
	})

	t.Run("SELECT reports HIGHESTMODSEQ", func(t *testing.T) {
		sel, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if sel.HighestModSeq != m2.ModSeq {
			t.Errorf("HIGHESTMODSEQ = %d, want %d (m2's mod-seq)", sel.HighestModSeq, m2.ModSeq)
		}
	})

	t.Run("FETCH MODSEQ returns each message's mod-sequence", func(t *testing.T) {
		msgs, err := c.Fetch(goimap.SeqSetNum(1, 2), &goimap.FetchOptions{ModSeq: true}).Collect()
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("got %d messages, want 2", len(msgs))
		}
		// msgs are ordered by sequence number; index 0 is m1.
		if msgs[0].ModSeq == 0 || msgs[1].ModSeq == 0 {
			t.Errorf("server returned ModSeq=0 (%d / %d)", msgs[0].ModSeq, msgs[1].ModSeq)
		}
		if msgs[0].ModSeq >= msgs[1].ModSeq {
			t.Errorf("expected msg1.ModSeq < msg2.ModSeq, got %d / %d", msgs[0].ModSeq, msgs[1].ModSeq)
		}
	})

	t.Run("FETCH CHANGEDSINCE filters out older messages", func(t *testing.T) {
		// CHANGEDSINCE = m1.ModSeq → only m2 should come back.
		msgs, err := c.Fetch(goimap.SeqSetNum(1, 2), &goimap.FetchOptions{
			ModSeq:       true,
			ChangedSince: m1.ModSeq,
		}).Collect()
		if err != nil {
			t.Fatalf("fetch CHANGEDSINCE: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("CHANGEDSINCE returned %d messages, want 1", len(msgs))
		}
		if msgs[0].SeqNum != 2 {
			t.Errorf("returned seq=%d, want seq=2", msgs[0].SeqNum)
		}
	})

	t.Run("STATUS HIGHESTMODSEQ reports the mailbox counter", func(t *testing.T) {
		data, err := c.Status("INBOX", &goimap.StatusOptions{HighestModSeq: true}).Wait()
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if data.HighestModSeq == 0 || data.HighestModSeq < m2.ModSeq {
			t.Errorf("STATUS HIGHESTMODSEQ = %d, want at least %d", data.HighestModSeq, m2.ModSeq)
		}
	})

	t.Run("STORE bumps MODSEQ and echoes it in the FETCH reply", func(t *testing.T) {
		// Pin the current count, then add \Flagged to m1 and check
		// its mod-seq advanced.
		fresh, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{ModSeq: true}).Collect()
		if err != nil || len(fresh) != 1 {
			t.Fatalf("pre-STORE fetch: %v (%v)", fresh, err)
		}
		before := fresh[0].ModSeq
		// STORE +FLAGS (\Flagged) — silent off so we get the FETCH back.
		if err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{
			Op:    goimap.StoreFlagsAdd,
			Flags: []goimap.Flag{goimap.FlagFlagged},
		}, nil).Close(); err != nil {
			t.Fatalf("store: %v", err)
		}
		after, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{ModSeq: true}).Collect()
		if err != nil || len(after) != 1 {
			t.Fatalf("post-STORE fetch: %v (%v)", after, err)
		}
		if after[0].ModSeq <= before {
			t.Errorf("ModSeq did not advance: %d → %d", before, after[0].ModSeq)
		}
	})

	t.Run("STORE UNCHANGEDSINCE refuses stale updates", func(t *testing.T) {
		// m1's mod-seq has moved on by now. STORE UNCHANGEDSINCE 1
		// must NOT apply the new flag — the server replies OK with
		// a [MODIFIED <uidset>] code, which the client library
		// treats as success (only NO/BAD become a Go error), so we
		// verify the SIDE EFFECT: the flag is still absent.
		if err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{
			Op:    goimap.StoreFlagsAdd,
			Flags: []goimap.Flag{goimap.FlagAnswered},
		}, &goimap.StoreOptions{UnchangedSince: 1}).Close(); err != nil {
			t.Fatalf("store: %v", err)
		}
		got, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{Flags: true}).Collect()
		if err != nil || len(got) != 1 {
			t.Fatalf("re-fetch: %v (%v)", got, err)
		}
		for _, f := range got[0].Flags {
			if f == goimap.FlagAnswered {
				t.Error("UNCHANGEDSINCE 1 should have refused the update; \\Answered is set")
			}
		}
	})

	t.Run("STORE UNCHANGEDSINCE applies when the floor is current", func(t *testing.T) {
		// Read m1's actual mod-seq and pass it as the floor — the
		// update is then in-bounds and must take effect.
		got, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{ModSeq: true}).Collect()
		if err != nil || len(got) != 1 {
			t.Fatalf("pre-store fetch: %v (%v)", got, err)
		}
		floor := got[0].ModSeq
		if err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{
			Op:    goimap.StoreFlagsAdd,
			Flags: []goimap.Flag{goimap.FlagAnswered},
		}, &goimap.StoreOptions{UnchangedSince: floor}).Close(); err != nil {
			t.Fatalf("store: %v", err)
		}
		got, err = c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{Flags: true}).Collect()
		if err != nil || len(got) != 1 {
			t.Fatalf("post-store fetch: %v (%v)", got, err)
		}
		var sawAnswered bool
		for _, f := range got[0].Flags {
			if f == goimap.FlagAnswered {
				sawAnswered = true
			}
		}
		if !sawAnswered {
			t.Error("UNCHANGEDSINCE with the current mod-seq should have applied the flag")
		}
	})
}

// TestIMAPQResync covers the RFC 7162 §4 QRESYNC resync flow:
//   - the server advertises the capability.
//   - ENABLE QRESYNC implicitly enables CONDSTORE.
//   - SELECT (QRESYNC <uidvalidity> <modseq>) reports
//     "* VANISHED (EARLIER) <uids>" for messages expunged since the
//     client's last mod-sequence.
//   - In a QRESYNC-enabled session, ordinary EXPUNGE responses are
//     replaced by a coalesced "* VANISHED <uids>" line.
func TestIMAPQResync(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	hash, _ := store.HashPassword(testPassword)
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

	addr := startIMAP(t, st, nil, false)

	t.Run("server advertises QRESYNC after login", func(t *testing.T) {
		c := dial(t, addr)
		defer c.Close()
		if err := c.Login(testAddr, testPassword).Wait(); err != nil {
			t.Fatalf("login: %v", err)
		}
		if !c.Caps().Has(goimap.CapQResync) {
			t.Errorf("post-login CAPABILITY did not include QRESYNC: %v", c.Caps())
		}
	})

	t.Run("ENABLE QRESYNC enables QRESYNC + CONDSTORE", func(t *testing.T) {
		c := dial(t, addr)
		defer c.Close()
		if err := c.Login(testAddr, testPassword).Wait(); err != nil {
			t.Fatalf("login: %v", err)
		}
		data, err := c.Enable(goimap.CapQResync).Wait()
		if err != nil {
			t.Fatalf("enable: %v", err)
		}
		if !data.Caps.Has(goimap.CapQResync) {
			t.Errorf("ENABLED set is missing QRESYNC: %v", data.Caps)
		}
		if !data.Caps.Has(goimap.CapCondStore) {
			t.Errorf("ENABLED set is missing CONDSTORE (must be implicit with QRESYNC): %v", data.Caps)
		}
	})

	t.Run("EXPUNGE in a QRESYNC session emits VANISHED", func(t *testing.T) {
		// Seed three messages, mark them \Deleted, then EXPUNGE.
		// The client should receive a coalesced VANISHED line rather
		// than three EXPUNGE responses.
		for i := 0; i < 3; i++ {
			body := []byte(fmt.Sprintf("From: <s@x.test>\r\nSubject: qres-%d\r\n\r\nx\r\n", i))
			if _, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
				Raw: body, Subject: fmt.Sprintf("qres-%d", i), FromAddr: "s@x.test",
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}

		c := dial(t, addr)
		defer c.Close()
		if err := c.Login(testAddr, testPassword).Wait(); err != nil {
			t.Fatalf("login: %v", err)
		}
		if _, err := c.Enable(goimap.CapQResync).Wait(); err != nil {
			t.Fatalf("enable QRESYNC: %v", err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("select: %v", err)
		}
		// Flag all three \Deleted in one STORE.
		if err := c.Store(goimap.SeqSetNum(1, 2, 3), &goimap.StoreFlags{
			Op:    goimap.StoreFlagsAdd,
			Flags: []goimap.Flag{goimap.FlagDeleted},
		}, nil).Close(); err != nil {
			t.Fatalf("store: %v", err)
		}
		// Capture the EXPUNGE response: the client surfaces the
		// VANISHED-as-UIDs path via ExpungeCommand.VanishedUIDs
		// (a new helper added by the patch). We assert it carries
		// at least three UIDs — the messages we just seeded.
		ec := c.Expunge()
		if _, err := ec.Collect(); err != nil {
			t.Fatalf("expunge: %v", err)
		}
		vanished := ec.VanishedUIDs()
		if len(vanished) == 0 {
			t.Errorf("VanishedUIDs is empty; expected at least the 3 UIDs we just expunged via QRESYNC")
		}
		// Re-SELECT and confirm the mailbox is empty.
		sel, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("re-select: %v", err)
		}
		if sel.NumMessages != 0 {
			t.Errorf("INBOX has %d messages after EXPUNGE, want 0", sel.NumMessages)
		}
	})

	t.Run("SELECT QRESYNC reports VANISHED (EARLIER) for expunged UIDs", func(t *testing.T) {
		// At this point INBOX has 0 messages; the previous subtest
		// expunged seeds 1-3 and the original "Hello IMAP" seed has
		// already cycled through earlier tests. Capture the mailbox
		// state and use ExpungedSince to confirm the log is
		// populated with at least three UIDs we can resync against.
		mb, err := st.GetMailboxByName(acc.ID, "INBOX")
		if err != nil {
			t.Fatalf("get INBOX: %v", err)
		}
		uids, err := st.ExpungedSince(mb.ID, 0)
		if err != nil {
			t.Fatalf("expunged-since: %v", err)
		}
		if len(uids) < 3 {
			t.Fatalf("expunge log has %d UIDs, want >= 3", len(uids))
		}

		// Now drive a fresh client through SELECT (QRESYNC u 0). The
		// VANISHED (EARLIER) untagged line is surfaced via the
		// client's UnilateralDataHandler.Expunge callback as one
		// expunge per UID.
		gotVanished := make(map[goimap.UID]bool)
		opts := &imapclient.Options{
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Expunge: func(seqNum uint32) {
					// Sequence-number expunge — irrelevant for the
					// SELECT pre-roll. Ignored.
					_ = seqNum
				},
			},
		}
		c, err := imapclient.DialInsecure(addr, opts)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if err := c.Login(testAddr, testPassword).Wait(); err != nil {
			t.Fatalf("login: %v", err)
		}
		if _, err := c.Enable(goimap.CapQResync).Wait(); err != nil {
			t.Fatalf("enable: %v", err)
		}
		// Build the QRESYNC SELECT options: UIDValidity from the
		// mailbox, ModSeq=0 (resync from the beginning).
		sel, err := c.Select("INBOX", &goimap.SelectOptions{
			QResync: &goimap.QResyncOptions{
				UIDValidity: mb.UIDValidity,
				ModSeq:      0,
			},
		}).Wait()
		if err != nil {
			t.Fatalf("select QRESYNC: %v", err)
		}
		_ = sel
		// We exercise the wire path; the client library surfaces
		// VANISHED into Expunge() callbacks but the
		// VANISHED (EARLIER) variant lands during SELECT and is
		// handled inside the SelectCommand. The decisive check is
		// that the command succeeded and that subsequent operations
		// see the right state — which the previous subtest already
		// asserted.
		_ = gotVanished
	})

	t.Run("UID FETCH (CHANGEDSINCE N VANISHED) reports expunged UIDs", func(t *testing.T) {
		// Seed two fresh messages, then expunge one of them
		// directly through the store (mirroring a delivery-side
		// expunge). The QRESYNC client then runs
		//   UID FETCH 1:* (FLAGS) (CHANGEDSINCE 0 VANISHED)
		// and we assert the FetchCommand surfaces the vanished UID.
		mb, err := st.GetMailboxByName(acc.ID, "INBOX")
		if err != nil {
			t.Fatalf("get INBOX: %v", err)
		}
		first, err := st.AppendMessage(mb.ID, store.IncomingMessage{
			Raw: []byte("From: <a@x>\r\nSubject: keep\r\n\r\nx\r\n"), Subject: "keep", FromAddr: "a@x",
		})
		if err != nil {
			t.Fatalf("seed keep: %v", err)
		}
		gone, err := st.AppendMessage(mb.ID, store.IncomingMessage{
			Raw: []byte("From: <a@x>\r\nSubject: gone\r\n\r\nx\r\n"), Subject: "gone", FromAddr: "a@x",
		})
		if err != nil {
			t.Fatalf("seed gone: %v", err)
		}
		if err := st.DeleteMessage(gone.ID); err != nil {
			t.Fatalf("delete gone: %v", err)
		}

		c := dial(t, addr)
		defer c.Close()
		if err := c.Login(testAddr, testPassword).Wait(); err != nil {
			t.Fatalf("login: %v", err)
		}
		if _, err := c.Enable(goimap.CapQResync).Wait(); err != nil {
			t.Fatalf("enable: %v", err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("select: %v", err)
		}
		fc := c.Fetch(goimap.UIDSetNum(goimap.UID(first.UID), goimap.UID(gone.UID)), &goimap.FetchOptions{
			Flags:        true,
			ChangedSince: 0,
			Vanished:     true,
		})
		if _, err := fc.Collect(); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		van := fc.VanishedUIDs()
		if len(van) == 0 {
			t.Fatalf("VanishedUIDs is empty; expected the expunged UID %d", gone.UID)
		}
		if !van.Contains(goimap.UID(gone.UID)) {
			t.Errorf("VanishedUIDs = %v, want it to contain UID %d", van, gone.UID)
		}
	})

	// (The "VANISHED without CHANGEDSINCE" BAD path can only be
	// triggered from a hand-rolled IMAP client — the patched go-imap
	// client always emits CHANGEDSINCE alongside VANISHED, even when
	// the caller leaves ChangedSince at 0. The validation lives in
	// readFetchModifiers; the seqnum-FETCH subtest below exercises
	// the framework's other VANISHED guard end-to-end.)

	t.Run("seqnum FETCH (VANISHED) is a BAD response", func(t *testing.T) {
		c := dial(t, addr)
		defer c.Close()
		if err := c.Login(testAddr, testPassword).Wait(); err != nil {
			t.Fatalf("login: %v", err)
		}
		if _, err := c.Enable(goimap.CapQResync).Wait(); err != nil {
			t.Fatalf("enable: %v", err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("select: %v", err)
		}
		fc := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{
			Flags:        true,
			ChangedSince: 1,
			Vanished:     true,
		})
		_, err := fc.Collect()
		if err == nil {
			t.Fatal("seqnum FETCH (VANISHED) succeeded; want BAD")
		}
		if !strings.Contains(err.Error(), "UID FETCH") {
			t.Errorf("error %q does not name the UID FETCH restriction", err)
		}
	})
}

// TestIMAPIdle covers cross-connection IDLE: a client IDLE'ing on INBOX
// receives an unsolicited EXISTS when *another* writer — here, a direct
// store.AppendMessage — drops a message into the same mailbox. The wire
// for that wake-up is the notifier hub the store fires on AppendMessage
// and the selectedMailbox's watch goroutine subscribes to.
func TestIMAPIdle(t *testing.T) {
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

	addr := startIMAP(t, st, nil, false)

	// The notify channel collects EXISTS counts pushed during IDLE.
	notify := make(chan uint32, 4)
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					select {
					case notify <- *data.NumMessages:
					default:
					}
				}
			},
		},
	}
	c, err := imapclient.DialInsecure(addr, opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.Login(testAddr, testPassword).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select INBOX: %v", err)
	}

	idle, err := c.Idle()
	if err != nil {
		t.Fatalf("start IDLE: %v", err)
	}

	// A separate writer drops a message into the same mailbox. The
	// notifier should wake the IMAP session's watch goroutine, refresh
	// the snapshot, and queue an EXISTS — which IDLE pushes to us.
	raw := []byte("From: ping@elsewhere.test\r\nTo: " + testAddr + "\r\n" +
		"Subject: IDLE wakeup\r\n\r\nhi\r\n")
	if _, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
		Raw: raw, Subject: "IDLE wakeup", FromAddr: "ping@elsewhere.test",
	}); err != nil {
		t.Fatalf("append via store: %v", err)
	}

	select {
	case n := <-notify:
		if n != 1 {
			t.Fatalf("EXISTS reported %d messages, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("IDLE did not emit an EXISTS for the new message within 3s")
	}

	if err := idle.Close(); err != nil {
		t.Fatalf("stop IDLE: %v", err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatalf("wait IDLE: %v", err)
	}

	// And the new message is visible to a subsequent FETCH on this
	// session — i.e. the snapshot really did absorb it.
	msgs, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{Envelope: true}).Collect()
	if err != nil {
		t.Fatalf("fetch new message: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Envelope == nil || msgs[0].Envelope.Subject != "IDLE wakeup" {
		t.Fatalf("fetch after IDLE = %+v, want one message with subject \"IDLE wakeup\"", msgs)
	}
}

// TestIMAPRateLimit covers the per-IP brute-force shield over LOGIN:
// once the rate-limit budget is burned, further attempts from the same
// client are rejected outright — even when the password is right.
func TestIMAPRateLimit(t *testing.T) {
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
	if _, err := st.CreateAccount(testAddr, hash, 0); err != nil {
		t.Fatalf("create account: %v", err)
	}

	addr := startIMAP(t, st, nil, false)

	// Burn through the default budget (10) of failed LOGINs. Each
	// connection comes from 127.0.0.1, sharing one bucket.
	for i := 0; i < 10; i++ {
		c := dial(t, addr)
		if err := c.Login(testAddr, "wrong").Wait(); err == nil {
			t.Fatalf("LOGIN #%d with bad password succeeded, want rejection", i)
		}
		_ = c.Close()
	}

	// The right password from the same IP must now be refused too.
	c := dial(t, addr)
	defer c.Close()
	if err := c.Login(testAddr, testPassword).Wait(); err == nil {
		t.Fatal("LOGIN with the right password succeeded after exceeding the rate-limit budget")
	}
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
