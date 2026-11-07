//go:build integration

package store

import (
	"testing"

	"github.com/parisxmas/OxiMail/internal/itest"
)

// A delete that loses the race for the document must leave the blob
// reference alone: a second drop would destroy a body a copy still uses.
func TestRemoveMessageDropsRefOnlyOnce(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	acc, err := st.CreateAccount("twice@remove.test", "h", 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	inbox, _ := st.GetMailboxByName(acc.ID, "INBOX")
	archive, _ := st.GetMailboxByName(acc.ID, "Archive")
	msg, err := st.AppendMessage(acc.ID, inbox.ID, IncomingMessage{Raw: []byte("Subject: x\r\n\r\nbody\r\n")})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	cp, err := st.CopyMessage(acc.ID, msg.ID, archive.ID)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}

	for i, want := range []bool{true, false} {
		removed, err := st.removeMessage(acc.ID, msg, 99)
		if err != nil {
			t.Fatalf("remove #%d: %v", i, err)
		}
		if removed != want {
			t.Fatalf("remove #%d: removed = %v, want %v", i, removed, want)
		}
	}
	if _, err := st.FetchBody(cp); err != nil {
		t.Fatalf("copy lost its body after a repeated delete: %v", err)
	}
}
