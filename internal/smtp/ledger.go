package smtp

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// ledgerTTL is how long a partly delivered message is remembered. It
// outlasts the retry schedule of any usual sending MTA.
const ledgerTTL = 7 * 24 * time.Hour

// deliveryLedger remembers which local accounts already hold a message
// whose DATA ended in a 4xx.
//
// SMTP gives one status for the whole of DATA, and go-smtp has
// per-recipient status only for LMTP, which a remote sender does not
// speak. So when some recipients succeed and others fail, the only
// honest reply is a 4xx for all of them, and the sender retries for all
// of them. The ledger is what stops that retry from filing a second
// copy with the accounts that got the first.
//
// It lives in memory: a restart between the 4xx and the retry costs one
// duplicate, never a lost message.
type deliveryLedger struct {
	mu      sync.Mutex
	entries map[ledgerKey]*ledgerEntry
}

type ledgerKey [sha256.Size]byte

type ledgerEntry struct {
	accounts map[uint64]bool
	expires  time.Time
}

func newDeliveryLedger() *deliveryLedger {
	return &deliveryLedger{entries: make(map[ledgerKey]*ledgerEntry)}
}

// keyOf identifies a message across retries. A sending MTA may
// regenerate its own Received header on each attempt, so with a
// Message-Id the header block is left out. Without one, the whole
// message counts: two sends of an identical body must not pass for a
// retry just because neither has an id.
func keyOf(from, messageID string, raw []byte) ledgerKey {
	content := raw
	if messageID != "" {
		if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
			content = raw[i:]
		}
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s%d:%s", len(from), from, len(messageID), messageID)
	h.Write(content)
	var k ledgerKey
	h.Sum(k[:0])
	return k
}

// delivered returns the accounts already holding this message.
func (l *deliveryLedger) delivered(k ledgerKey) map[uint64]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[uint64]bool)
	if e := l.entries[k]; e != nil {
		for id := range e.accounts {
			out[id] = true
		}
	}
	return out
}

// record notes that accounts now hold this message.
func (l *deliveryLedger) record(k ledgerKey, accounts []uint64, now time.Time) {
	if len(accounts) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[k]
	if e == nil {
		e = &ledgerEntry{accounts: make(map[uint64]bool)}
		l.entries[k] = e
	}
	for _, id := range accounts {
		e.accounts[id] = true
	}
	e.expires = now.Add(ledgerTTL)
}

// forget drops a message whose delivery has finished.
func (l *deliveryLedger) forget(k ledgerKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, k)
}

// sweep drops entries for senders that never came back.
func (l *deliveryLedger) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, e := range l.entries {
		if !now.Before(e.expires) {
			delete(l.entries, k)
		}
	}
}
