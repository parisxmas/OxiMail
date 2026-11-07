//go:build integration

package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/parisxmas/OxiDB/go/oxidb"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/store"
)

// regressionEnv is a store plus a raw OxiDB client, which reaches past
// the store to damage or inspect state the store API does not expose.
type regressionEnv struct {
	st  *store.Store
	raw *oxidb.Client
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
	return &regressionEnv{st: st, raw: raw}
}

// account creates an account with the default mailboxes.
func (e *regressionEnv) account(t *testing.T, address string, quota int64) *store.Account {
	t.Helper()
	acc, err := e.st.CreateAccount(address, "h", quota)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := e.st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("default mailboxes: %v", err)
	}
	return acc
}

func (e *regressionEnv) mailbox(t *testing.T, acc *store.Account, name string) *store.Mailbox {
	t.Helper()
	mb, err := e.st.GetMailboxByName(acc.ID, name)
	if err != nil {
		t.Fatalf("get mailbox %q: %v", name, err)
	}
	return mb
}

func (e *regressionEnv) append(t *testing.T, acc *store.Account, mb *store.Mailbox, size int) *store.Message {
	t.Helper()
	msg, err := e.st.AppendMessage(acc.ID, mb.ID, store.IncomingMessage{Raw: bytes.Repeat([]byte("x"), size)})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return msg
}

func (e *regressionEnv) refRows(t *testing.T) int {
	t.Helper()
	n, err := e.raw.Count(store.CollBlobRefs, map[string]any{})
	if err != nil {
		t.Fatalf("count blob refs: %v", err)
	}
	return n
}

func (e *regressionEnv) usedBytes(t *testing.T, acc *store.Account) int64 {
	t.Helper()
	got, err := e.st.GetAccountByID(acc.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return got.UsedBytes
}

func TestDeleteMailboxRespectsBlobRefs(t *testing.T) {
	e := newRegressionEnv(t)
	acc := e.account(t, "refs@reg.test", 0)
	inbox := e.mailbox(t, acc, "INBOX")
	folder, err := e.st.CreateMailbox(acc.ID, "A")
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	orig := e.append(t, acc, inbox, 100)
	if _, err := e.st.CopyMessage(acc.ID, orig.ID, folder.ID); err != nil {
		t.Fatalf("copy: %v", err)
	}

	if err := e.st.DeleteMailbox(acc.ID, folder.ID); err != nil {
		t.Fatalf("delete mailbox: %v", err)
	}
	if _, err := e.st.FetchBody(orig); err != nil {
		t.Fatalf("original lost its body when the copy's folder was deleted: %v", err)
	}
	if got := e.usedBytes(t, acc); got != 100 {
		t.Errorf("used_bytes after folder delete = %d, want 100", got)
	}

	// The last reference still takes the blob and its row with it.
	if err := e.st.DeleteMessage(acc.ID, orig.ID); err != nil {
		t.Fatalf("delete original: %v", err)
	}
	if _, err := e.st.FetchBody(orig); err == nil {
		t.Error("blob survived its last reference")
	}
	if n := e.refRows(t); n != 0 {
		t.Errorf("blob_refs rows = %d, want 0", n)
	}
	if got := e.usedBytes(t, acc); got != 0 {
		t.Errorf("used_bytes after last delete = %d, want 0", got)
	}
}

func TestQuota(t *testing.T) {
	e := newRegressionEnv(t)

	t.Run("delivery stops at the quota", func(t *testing.T) {
		acc := e.account(t, "full@reg.test", 100)
		inbox := e.mailbox(t, acc, "INBOX")
		e.append(t, acc, inbox, 100) // exactly full is allowed
		refs := e.refRows(t)

		in := store.IncomingMessage{Raw: []byte("x")}
		if _, err := e.st.AppendMessage(acc.ID, inbox.ID, in); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Errorf("AppendMessage: err = %v, want ErrQuotaExceeded", err)
		}
		if _, err := e.st.Deliver(acc.ID, in); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Errorf("Deliver: err = %v, want ErrQuotaExceeded", err)
		}
		if _, err := e.st.DeliverTo(acc.ID, "Junk", in); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Errorf("DeliverTo: err = %v, want ErrQuotaExceeded", err)
		}
		if _, err := e.st.Route("s@x", []string{acc.Address}, in); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Errorf("Route: err = %v, want ErrQuotaExceeded", err)
		}

		// A refusal writes nothing.
		if n := e.refRows(t); n != refs {
			t.Errorf("blob_refs rows went %d -> %d on a refused delivery", refs, n)
		}
		if got := e.mailbox(t, acc, "INBOX").UIDNext; got != 2 {
			t.Errorf("UIDNext = %d, want 2: a refusal burned a UID", got)
		}
	})

	t.Run("deleting frees quota", func(t *testing.T) {
		acc := e.account(t, "freed@reg.test", 100)
		inbox := e.mailbox(t, acc, "INBOX")
		msg := e.append(t, acc, inbox, 100)
		if err := e.st.DeleteMessage(acc.ID, msg.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		e.append(t, acc, inbox, 100)
	})

	t.Run("a deleted folder frees quota", func(t *testing.T) {
		acc := e.account(t, "folder@reg.test", 100)
		folder, err := e.st.CreateMailbox(acc.ID, "Old")
		if err != nil {
			t.Fatalf("create mailbox: %v", err)
		}
		e.append(t, acc, folder, 100)
		if err := e.st.DeleteMailbox(acc.ID, folder.ID); err != nil {
			t.Fatalf("delete mailbox: %v", err)
		}
		e.append(t, acc, e.mailbox(t, acc, "INBOX"), 100)
	})

	t.Run("a copy counts and is refused past the quota", func(t *testing.T) {
		acc := e.account(t, "copy@reg.test", 150)
		inbox := e.mailbox(t, acc, "INBOX")
		archive := e.mailbox(t, acc, "Archive")
		msg := e.append(t, acc, inbox, 70)
		cp, err := e.st.CopyMessage(acc.ID, msg.ID, archive.ID)
		if err != nil {
			t.Fatalf("first copy: %v", err)
		}
		if got := e.usedBytes(t, acc); got != 140 {
			t.Fatalf("used_bytes = %d, want 140", got)
		}
		if _, err := e.st.CopyMessage(acc.ID, msg.ID, archive.ID); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Fatalf("second copy: err = %v, want ErrQuotaExceeded", err)
		}
		// The refused copy must not have leaked a reference: both
		// documents gone means the blob is gone.
		for _, id := range []uint64{msg.ID, cp.ID} {
			if err := e.st.DeleteMessage(acc.ID, id); err != nil {
				t.Fatalf("delete: %v", err)
			}
		}
		if _, err := e.st.FetchBody(msg); err == nil {
			t.Error("refused copy leaked a blob reference")
		}
		if got := e.usedBytes(t, acc); got != 0 {
			t.Errorf("used_bytes after deleting both = %d, want 0", got)
		}
	})

	t.Run("zero means unlimited", func(t *testing.T) {
		acc := e.account(t, "free@reg.test", 0)
		e.append(t, acc, e.mailbox(t, acc, "INBOX"), 100000)
	})
}

func TestConcurrentFlagChanges(t *testing.T) {
	e := newRegressionEnv(t)
	acc := e.account(t, "flags@reg.test", 0)
	msg := e.append(t, acc, e.mailbox(t, acc, "INBOX"), 10)

	const writers = 24
	want := make([]string, writers)
	for i := range want {
		want[i] = fmt.Sprintf("$k%02d", i)
	}
	run := func(op func(flag string) error) {
		var wg sync.WaitGroup
		for _, f := range want {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := op(f); err != nil {
					t.Errorf("flag %s: %v", f, err)
				}
			}()
		}
		wg.Wait()
	}

	run(func(f string) error { return e.st.AddFlags(acc.ID, msg.ID, f) })
	got, err := e.st.GetMessage(acc.ID, msg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	slices.Sort(got.Flags)
	if !slices.Equal(got.Flags, want) {
		t.Fatalf("after concurrent AddFlags: %d flags %v, want all %d", len(got.Flags), got.Flags, writers)
	}

	run(func(f string) error { return e.st.RemoveFlags(acc.ID, msg.ID, f) })
	got, err = e.st.GetMessage(acc.ID, msg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Flags) != 0 {
		t.Fatalf("after concurrent RemoveFlags: flags %v survive", got.Flags)
	}
}

func TestUpdateFlagsUnchangedSince(t *testing.T) {
	e := newRegressionEnv(t)
	acc := e.account(t, "since@reg.test", 0)
	msg := e.append(t, acc, e.mailbox(t, acc, "INBOX"), 10)
	add := func(cur []string) []string { return append(cur, "$x") }

	// Move past mod-seq 1, so a floor below it is non-zero.
	if err := e.st.AddFlags(acc.ID, msg.ID, "$first"); err != nil {
		t.Fatalf("add flag: %v", err)
	}
	msg, err := e.st.GetMessage(acc.ID, msg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	_, err = e.st.UpdateFlags(acc.ID, msg.ID, msg.ModSeq-1, add)
	if !errors.Is(err, store.ErrModified) {
		t.Fatalf("stale floor: err = %v, want ErrModified", err)
	}
	updated, err := e.st.UpdateFlags(acc.ID, msg.ID, msg.ModSeq, add)
	if err != nil {
		t.Fatalf("current floor: %v", err)
	}
	if updated.ModSeq <= msg.ModSeq || !slices.Contains(updated.Flags, "$x") {
		t.Fatalf("update not reflected: %+v", updated)
	}
	if err := e.st.DeleteMessage(acc.ID, msg.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.st.UpdateFlags(acc.ID, msg.ID, 0, add); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted message: err = %v, want ErrNotFound", err)
	}
}

func TestExpungeLogCoversEveryRemoval(t *testing.T) {
	e := newRegressionEnv(t)

	t.Run("move", func(t *testing.T) {
		acc := e.account(t, "move@reg.test", 0)
		inbox := e.mailbox(t, acc, "INBOX")
		msg := e.append(t, acc, inbox, 10)
		floor := e.mailbox(t, acc, "INBOX").HighestModSeq

		if _, err := e.st.MoveMessage(acc.ID, msg.ID, e.mailbox(t, acc, "Archive").ID); err != nil {
			t.Fatalf("move: %v", err)
		}
		uids, err := e.st.ExpungedSince(acc.ID, inbox.ID, floor)
		if err != nil {
			t.Fatalf("expunged since: %v", err)
		}
		if !slices.Equal(uids, []uint32{msg.UID}) {
			t.Fatalf("expunge log = %v, want [%d]", uids, msg.UID)
		}
		if got := e.mailbox(t, acc, "INBOX").HighestModSeq; got <= floor {
			t.Errorf("source HIGHESTMODSEQ %d did not pass %d", got, floor)
		}
	})

	t.Run("delete mailbox", func(t *testing.T) {
		acc := e.account(t, "delbox@reg.test", 0)
		folder, err := e.st.CreateMailbox(acc.ID, "Doomed")
		if err != nil {
			t.Fatalf("create mailbox: %v", err)
		}
		a, b := e.append(t, acc, folder, 10), e.append(t, acc, folder, 10)
		if err := e.st.DeleteMailbox(acc.ID, folder.ID); err != nil {
			t.Fatalf("delete mailbox: %v", err)
		}
		uids, err := e.st.ExpungedSince(acc.ID, folder.ID, 0)
		if err != nil {
			t.Fatalf("expunged since: %v", err)
		}
		if !slices.Equal(uids, []uint32{a.UID, b.UID}) {
			t.Fatalf("expunge log = %v, want [%d %d]", uids, a.UID, b.UID)
		}
	})

	t.Run("delete of a message whose blob is gone", func(t *testing.T) {
		acc := e.account(t, "lost@reg.test", 0)
		inbox := e.mailbox(t, acc, "INBOX")
		msg := e.append(t, acc, inbox, 10)
		if err := e.raw.DeleteObject(store.BlobBucket, msg.BodyBlob); err != nil {
			t.Fatalf("damage blob: %v", err)
		}
		if _, err := e.raw.Delete(store.CollBlobRefs, map[string]any{"blob_key": msg.BodyBlob}); err != nil {
			t.Fatalf("damage ref: %v", err)
		}

		if err := e.st.DeleteMessage(acc.ID, msg.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		uids, err := e.st.ExpungedSince(acc.ID, inbox.ID, 0)
		if err != nil {
			t.Fatalf("expunged since: %v", err)
		}
		if !slices.Equal(uids, []uint32{msg.UID}) {
			t.Fatalf("expunge log = %v, want [%d]", uids, msg.UID)
		}
		if got := e.usedBytes(t, acc); got != 0 {
			t.Errorf("used_bytes = %d, want 0", got)
		}
	})
}

func TestFailedAppendLeavesNoResidue(t *testing.T) {
	e := newRegressionEnv(t)
	acc := e.account(t, "residue@reg.test", 0)
	inbox := e.mailbox(t, acc, "INBOX")

	// A unique index makes the document insert fail after the blob and
	// its ref row have been written.
	if err := e.raw.CreateUniqueIndex(store.MessagesColl(acc.ID), "message_id"); err != nil {
		t.Fatalf("unique index: %v", err)
	}
	in := store.IncomingMessage{Raw: []byte("x"), MessageID: "dup@reg.test"}
	if _, err := e.st.AppendMessage(acc.ID, inbox.ID, in); err != nil {
		t.Fatalf("first append: %v", err)
	}
	refs := e.refRows(t)
	if _, err := e.st.AppendMessage(acc.ID, inbox.ID, in); err == nil {
		t.Fatal("second append should have failed on the unique index")
	}
	if n := e.refRows(t); n != refs {
		t.Errorf("blob_refs rows went %d -> %d: failed append left a ref row", refs, n)
	}
	objects, err := e.raw.ListObjects(store.BlobBucket, nil, nil)
	if err != nil {
		t.Fatalf("list blobs: %v", err)
	}
	if len(objects) != refs {
		t.Errorf("%d blobs for %d ref rows: failed append left a blob", len(objects), refs)
	}
	if got := e.usedBytes(t, acc); got != 1 {
		t.Errorf("used_bytes = %d, want 1", got)
	}
}

// A reader must never see UID N before every lower UID: a client that
// has seen 6 will not ask for 5 again.
func TestUIDsBecomeVisibleInOrder(t *testing.T) {
	e := newRegressionEnv(t)
	acc := e.account(t, "order@reg.test", 0)
	inbox := e.mailbox(t, acc, "INBOX")

	const writers, perWriter = 8, 15
	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				// Uneven sizes, so body uploads finish out of order.
				size := 1 + ((w*perWriter+i)%5)*200000
				if _, err := e.st.AppendMessage(acc.ID, inbox.ID, store.IncomingMessage{
					Raw: bytes.Repeat([]byte("x"), size),
				}); err != nil {
					t.Errorf("append: %v", err)
				}
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()

	for {
		msgs, err := e.st.ListMessages(acc.ID, inbox.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for i := range msgs {
			if msgs[i].UID != uint32(i)+1 {
				t.Fatalf("UID %d visible at position %d: a lower UID is still missing", msgs[i].UID, i+1)
			}
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestRecountUsedBytes(t *testing.T) {
	e := newRegressionEnv(t)
	acc := e.account(t, "recount@reg.test", 0)
	inbox := e.mailbox(t, acc, "INBOX")
	archive := e.mailbox(t, acc, "Archive")
	e.append(t, acc, inbox, 30)
	e.append(t, acc, archive, 12)
	if _, err := e.raw.FindAndModify(store.CollAccounts, map[string]any{"_id": acc.ID},
		map[string]any{"$set": map[string]any{"used_bytes": 9999}}); err != nil {
		t.Fatalf("skew counter: %v", err)
	}
	got, err := e.st.RecountUsedBytes(acc.ID)
	if err != nil {
		t.Fatalf("recount: %v", err)
	}
	if got != 42 || e.usedBytes(t, acc) != 42 {
		t.Fatalf("recount = %d, stored %d, want 42", got, e.usedBytes(t, acc))
	}
}
