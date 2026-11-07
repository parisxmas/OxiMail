//go:build integration

package imap_test

import (
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/parisxmas/OxiDB/go/oxidb"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/notifier"
	"github.com/parisxmas/OxiMail/internal/store"
)

type regressionEnv struct {
	st   *store.Store
	raw  *oxidb.Client // reaches past the store to damage state
	addr string
}

func newRegressionEnv(t *testing.T) *regressionEnv {
	t.Helper()
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	raw, err := oxidb.Connect(host, port, 5*time.Second)
	if err != nil {
		t.Fatalf("raw connect: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &regressionEnv{st: st, raw: raw, addr: startIMAP(t, st, nil, false)}
}

func (e *regressionEnv) account(t *testing.T, address string, quota int64) (*store.Account, *store.Mailbox) {
	t.Helper()
	hash, err := store.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	acc, err := e.st.CreateAccount(address, hash, quota)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := e.st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("default mailboxes: %v", err)
	}
	inbox, err := e.st.GetMailboxByName(acc.ID, "INBOX")
	if err != nil {
		t.Fatalf("get INBOX: %v", err)
	}
	return acc, inbox
}

func (e *regressionEnv) login(t *testing.T, address string) *imapclient.Client {
	t.Helper()
	c := dial(t, e.addr)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login(address, testPassword).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

func seedMessage(t *testing.T, st *store.Store, acc *store.Account, mb *store.Mailbox, subject string) *store.Message {
	t.Helper()
	msg, err := st.AppendMessage(acc.ID, mb.ID, store.IncomingMessage{
		Raw:     []byte("From: <a@x>\r\nSubject: " + subject + "\r\n\r\nbody of " + subject + "\r\n"),
		Subject: subject, FromAddr: "a@x",
	})
	if err != nil {
		t.Fatalf("seed %q: %v", subject, err)
	}
	return msg
}

// within fails the test instead of hanging it if fn does not return.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no reply within 5s", what)
	}
}

func TestFetchMissingBody(t *testing.T) {
	e := newRegressionEnv(t)
	acc, inbox := e.account(t, "lost@reg.test", 0)
	lost := seedMessage(t, e.st, acc, inbox, "lost")
	seedMessage(t, e.st, acc, inbox, "fine")
	if err := e.raw.DeleteObject(store.BlobBucket, lost.BodyBlob); err != nil {
		t.Fatalf("damage blob: %v", err)
	}

	c := e.login(t, "lost@reg.test")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	for name, opts := range map[string]*goimap.FetchOptions{
		"body":     {BodySection: []*goimap.FetchItemBodySection{{}}},
		"envelope": {Envelope: true},
	} {
		t.Run(name, func(t *testing.T) {
			var (
				msgs []*imapclient.FetchMessageBuffer
				err  error
			)
			within(t, "FETCH", func() {
				msgs, err = c.Fetch(goimap.SeqSetNum(1, 2), opts).Collect()
			})
			if err == nil || !strings.Contains(err.Error(), "NO") {
				t.Fatalf("FETCH err = %v, want a tagged NO", err)
			}
			if len(msgs) != 1 || msgs[0].SeqNum != 2 {
				t.Fatalf("FETCH returned %d messages, want just the readable one", len(msgs))
			}
		})
	}

	t.Run("session survives", func(t *testing.T) {
		within(t, "NOOP", func() {
			if err := c.Noop().Wait(); err != nil {
				t.Errorf("noop: %v", err)
			}
		})
		within(t, "metadata FETCH", func() {
			msgs, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{Flags: true}).Collect()
			if err != nil || len(msgs) != 1 {
				t.Errorf("fetch: %v (%d messages)", err, len(msgs))
				return
			}
			// The non-peek fetch failed, so the message was not read.
			if hasIMAPFlag(msgs[0].Flags, goimap.FlagSeen) {
				t.Error("\\Seen was set on a message whose body could not be read")
			}
		})
	})
}

func TestCopyThenDeleteFolderKeepsOriginal(t *testing.T) {
	e := newRegressionEnv(t)
	acc, inbox := e.account(t, "keep@reg.test", 0)
	seedMessage(t, e.st, acc, inbox, "precious")

	c := e.login(t, "keep@reg.test")
	if err := c.Create("A", nil).Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}
	if _, err := c.Copy(goimap.SeqSetNum(1), "A").Wait(); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := c.Delete("A").Wait(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	within(t, "FETCH", func() {
		msgs, err := c.Fetch(goimap.SeqSetNum(1), &goimap.FetchOptions{
			BodySection: []*goimap.FetchItemBodySection{{Peek: true}},
		}).Collect()
		if err != nil || len(msgs) != 1 {
			t.Errorf("fetch: %v (%d messages)", err, len(msgs))
			return
		}
		for _, body := range msgs[0].BodySection {
			if !strings.Contains(string(body.Bytes), "body of precious") {
				t.Errorf("body = %q", body.Bytes)
			}
		}
	})
}

func TestOverQuota(t *testing.T) {
	e := newRegressionEnv(t)
	acc, inbox := e.account(t, "tight@reg.test", 120)
	seedMessage(t, e.st, acc, inbox, "one")
	seedMessage(t, e.st, acc, inbox, "two")
	used, err := e.st.GetAccountByID(acc.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if used.UsedBytes*2 <= 120 {
		t.Fatalf("seed too small to overflow on copy: %d bytes", used.UsedBytes)
	}

	c := e.login(t, "tight@reg.test")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	t.Run("APPEND", func(t *testing.T) {
		big := strings.Repeat("x", 500)
		cmd := c.Append("INBOX", int64(len(big)), nil)
		if _, err := cmd.Write([]byte(big)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		_, err := cmd.Wait()
		if err == nil || !strings.Contains(err.Error(), "NO [OVERQUOTA]") {
			t.Fatalf("APPEND err = %v, want NO [OVERQUOTA]", err)
		}
	})

	t.Run("COPY of a set refuses the whole set", func(t *testing.T) {
		_, err := c.Copy(goimap.SeqSetNum(1, 2), "Archive").Wait()
		if err == nil || !strings.Contains(err.Error(), "NO [OVERQUOTA]") {
			t.Fatalf("COPY err = %v, want NO [OVERQUOTA]", err)
		}
		archive, err := e.st.GetMailboxByName(acc.ID, "Archive")
		if err != nil {
			t.Fatalf("get Archive: %v", err)
		}
		if n, err := e.st.CountMessages(acc.ID, archive.ID); err != nil || n != 0 {
			t.Fatalf("Archive holds %d messages (%v), want 0", n, err)
		}
	})
}

func TestStoreUnchangedSinceUsesStoredModSeq(t *testing.T) {
	e := newRegressionEnv(t)
	acc, inbox := e.account(t, "cond@reg.test", 0)
	msg := seedMessage(t, e.st, acc, inbox, "contested")

	c := e.login(t, "cond@reg.test")
	if _, err := c.Enable(goimap.CapCondStore).Wait(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	// Change the document without a notifier wake-up, so the session's
	// snapshot still holds the old mod-sequence.
	if _, err := e.raw.FindAndModify(store.MessagesColl(acc.ID), map[string]any{"_id": msg.ID},
		map[string]any{"$set": map[string]any{"modseq": msg.ModSeq + 50}}); err != nil {
		t.Fatalf("advance modseq: %v", err)
	}

	if err := c.Store(goimap.SeqSetNum(1), &goimap.StoreFlags{
		Op:    goimap.StoreFlagsAdd,
		Flags: []goimap.Flag{goimap.FlagAnswered},
	}, &goimap.StoreOptions{UnchangedSince: msg.ModSeq}).Close(); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, err := e.st.GetMessage(acc.ID, msg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if hasStringFlag(got.Flags, string(goimap.FlagAnswered)) {
		t.Fatal("UNCHANGEDSINCE was judged on the stale snapshot: the update was applied")
	}
}

func TestHighestModSeqSurvivesExpunge(t *testing.T) {
	e := newRegressionEnv(t)
	acc, inbox := e.account(t, "hms@reg.test", 0)
	seedMessage(t, e.st, acc, inbox, "stays")
	top := seedMessage(t, e.st, acc, inbox, "goes")

	c := e.login(t, "hms@reg.test")
	before, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if err := e.st.DeleteMessage(acc.ID, top.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("reselect: %v", err)
	}
	if after.HighestModSeq <= before.HighestModSeq {
		t.Fatalf("HIGHESTMODSEQ %d -> %d across an expunge, want it to rise",
			before.HighestModSeq, after.HighestModSeq)
	}
}

func TestLateLowerUIDIsSeen(t *testing.T) {
	e := newRegressionEnv(t)
	acc, inbox := e.account(t, "late@reg.test", 0)

	// Another process allocated a UID, and a higher one landed first.
	lateUID, err := e.st.NextUID(acc.ID, inbox.ID)
	if err != nil {
		t.Fatalf("next uid: %v", err)
	}
	high := seedMessage(t, e.st, acc, inbox, "early")

	c := e.login(t, "late@reg.test")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}

	late := *high
	late.UID = lateUID
	late.BodyBlob = "late-blob"
	late.Subject = "late"
	if _, err := e.st.RestoreMessage(&late, []byte("Subject: late\r\n\r\nx\r\n"), true); err != nil {
		t.Fatalf("insert late message: %v", err)
	}
	notifier.Default.Notify(inbox.ID)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("noop: %v", err)
		}
		if c.Mailbox().NumMessages == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never saw UID %d: NumMessages = %d", lateUID, c.Mailbox().NumMessages)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Sequence numbers already handed out must not have moved.
	msgs, err := c.Fetch(goimap.SeqSetNum(1, 2), &goimap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 2 {
		t.Fatalf("fetch: %v (%d messages)", err, len(msgs))
	}
	if msgs[0].UID != goimap.UID(high.UID) || msgs[1].UID != goimap.UID(lateUID) {
		t.Fatalf("seq 1,2 = UID %d,%d, want %d,%d", msgs[0].UID, msgs[1].UID, high.UID, lateUID)
	}
}

func TestQResyncReportsMovedUIDs(t *testing.T) {
	e := newRegressionEnv(t)
	acc, inbox := e.account(t, "offline@reg.test", 0)
	msg := seedMessage(t, e.st, acc, inbox, "moved away")
	seedMessage(t, e.st, acc, inbox, "stays")
	synced, err := e.st.GetMailbox(acc.ID, inbox.ID)
	if err != nil {
		t.Fatalf("get mailbox: %v", err)
	}

	// A MOVE while the QRESYNC client is offline.
	mover := e.login(t, "offline@reg.test")
	if _, err := mover.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}
	if _, err := mover.Move(goimap.UIDSetNum(goimap.UID(msg.UID)), "Archive").Wait(); err != nil {
		t.Fatalf("move: %v", err)
	}

	c := e.login(t, "offline@reg.test")
	if _, err := c.Enable(goimap.CapQResync).Wait(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}
	fc := c.Fetch(goimap.UIDSetNum(goimap.UID(msg.UID)), &goimap.FetchOptions{
		UID: true, Flags: true, ChangedSince: synced.HighestModSeq, Vanished: true,
	})
	if _, err := fc.Collect(); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	van := fc.VanishedUIDs()
	if !van.Contains(goimap.UID(msg.UID)) {
		t.Fatalf("VANISHED = %v, want UID %d", van, msg.UID)
	}
}
