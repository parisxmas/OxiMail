//go:build integration

// Tests that need to reach inside the session: to make a store call
// fail at a chosen moment, or to control what the spam pipeline found.
package smtp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	gosmtp "github.com/emersion/go-smtp"
	"strings"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/store"
)

const srsTestSecret = "a-stable-srs-secret-32-byteslong!"

func openTestStore(t *testing.T) *store.Store {
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
	return st
}

func testBackend(st *store.Store) *backend {
	return newBackend("oximail.test", st, spam.Permissive(), nil, ForwarderConfig{
		SRSSecret:       []byte(srsTestSecret),
		ForwarderDomain: "oximail.test",
	})
}

// transact runs one mail transaction as go-smtp would, on a fresh
// session, as a sender's retry does.
func transact(t *testing.T, be *backend, from string, rcpts []string, msg string) error {
	t.Helper()
	s := &session{backend: be, remoteIP: "192.0.2.1"}
	if err := s.Mail(from, nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	for _, r := range rcpts {
		if err := s.Rcpt(r, nil); err != nil {
			t.Fatalf("RCPT %s: %v", r, err)
		}
	}
	return s.Data(strings.NewReader(msg))
}

func countInbox(t *testing.T, st *store.Store, acc *store.Account) int {
	t.Helper()
	inbox, err := st.GetMailboxByName(acc.ID, "INBOX")
	if errors.Is(err, store.ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("get INBOX: %v", err)
	}
	msgs, err := st.ListMessages(acc.ID, inbox.ID)
	if err != nil {
		t.Fatalf("list INBOX: %v", err)
	}
	return len(msgs)
}

// A retry after a partial failure reaches the account that missed out
// and leaves alone the account that already has the message.
func TestPartialFailureRetryDoesNotDuplicate(t *testing.T) {
	st := openTestStore(t)
	good, err := st.CreateAccount("good@oximail.test", "h", 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	flaky, err := st.CreateAccount("flaky@oximail.test", "h", 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	be := testBackend(st)
	realDeliver := be.deliver
	broken := true
	be.deliver = func(id uint64, folder string, in store.IncomingMessage) error {
		if id == flaky.ID && broken {
			return errors.New("store hiccup")
		}
		return realDeliver(id, folder, in)
	}

	rcpts := []string{"good@oximail.test", "flaky@oximail.test"}
	msg := "From: a@elsewhere.test\r\nMessage-Id: <p1@elsewhere.test>\r\nSubject: s\r\n\r\nbody\r\n"

	if err := transact(t, be, "a@elsewhere.test", rcpts, msg); err == nil {
		t.Fatal("partial failure returned success; the failed recipient would be lost")
	}
	if got := countInbox(t, st, good); got != 1 {
		t.Fatalf("good account has %d, want 1", got)
	}

	// Still broken: the sender retries, nobody gets a second copy.
	if err := transact(t, be, "a@elsewhere.test", rcpts, msg); err == nil {
		t.Fatal("second attempt returned success")
	}
	if got := countInbox(t, st, good); got != 1 {
		t.Fatalf("after a failed retry: good account has %d, want 1", got)
	}

	// Fixed: a retry with a regenerated Received field is the same message.
	broken = false
	retry := "Received: from relay.elsewhere.test; later\r\n" + msg
	if err := transact(t, be, "a@elsewhere.test", rcpts, retry); err != nil {
		t.Fatalf("final retry: %v", err)
	}
	if got := countInbox(t, st, good); got != 1 {
		t.Errorf("good account has %d after the retry, want 1 (duplicate)", got)
	}
	if got := countInbox(t, st, flaky); got != 1 {
		t.Errorf("flaky account has %d after the retry, want 1", got)
	}
	if len(be.ledger.entries) != 0 {
		t.Errorf("finished message still in the ledger: %d entries", len(be.ledger.entries))
	}

	// Delivery finished, so sending it again is a new message.
	if err := transact(t, be, "a@elsewhere.test", rcpts, msg); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if got := countInbox(t, st, good); got != 2 {
		t.Errorf("deliberate resend: good account has %d, want 2", got)
	}
}

// A forward that cannot be queued fails DATA after the local copies
// went in; the retry must not file them twice.
func TestForwardFailureRetryDoesNotDuplicate(t *testing.T) {
	st := openTestStore(t)
	local, err := st.CreateAccount("local@oximail.test", "h", 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := st.CreateAlias("team@oximail.test", []string{"local@oximail.test", "bob@elsewhere.test"}); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	be := testBackend(st)
	realEnqueue := be.enqueue
	broken := true
	be.enqueue = func(from string, rcpts []string, raw []byte) error {
		if broken {
			return errors.New("store hiccup")
		}
		return realEnqueue(from, rcpts, raw)
	}

	msg := "From: a@elsewhere.test\r\nMessage-Id: <f1@elsewhere.test>\r\nSubject: s\r\n\r\nbody\r\n"
	if err := transact(t, be, "a@elsewhere.test", []string{"team@oximail.test"}, msg); err == nil {
		t.Fatal("failed forward returned success")
	}
	broken = false
	if err := transact(t, be, "a@elsewhere.test", []string{"team@oximail.test"}, msg); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := countInbox(t, st, local); got != 1 {
		t.Errorf("local account has %d, want 1", got)
	}
	queued, err := st.ListDueOutbound(10)
	if err != nil {
		t.Fatalf("list outbound: %v", err)
	}
	if len(queued) != 1 {
		t.Errorf("queue has %d, want the one forward", len(queued))
	}
}

func TestLedgerKey(t *testing.T) {
	body := "\r\n\r\nbody\r\n"
	a := keyOf("s@x", "id@x", []byte("Subject: 1"+body))
	if b := keyOf("s@x", "id@x", []byte("Received: x\r\nSubject: 1"+body)); a != b {
		t.Error("a re-stamped header block changed the key of a message with an id")
	}
	if b := keyOf("s@x", "id@x", []byte("Subject: 1\r\n\r\nother\r\n")); a == b {
		t.Error("a different body kept the key")
	}
	if b := keyOf("t@x", "id@x", []byte("Subject: 1"+body)); a == b {
		t.Error("a different sender kept the key")
	}
	if keyOf("s@x", "", []byte("Subject: 1"+body)) == keyOf("s@x", "", []byte("Subject: 2"+body)) {
		t.Error("two messages with no id and different headers share a key")
	}
}

func TestLedgerSweep(t *testing.T) {
	l := newDeliveryLedger()
	now := time.Unix(1700000000, 0)
	l.record(ledgerKey{1}, []uint64{1}, now)
	l.sweep(now.Add(ledgerTTL - time.Hour))
	if len(l.delivered(ledgerKey{1})) != 1 {
		t.Fatal("entry swept before its TTL")
	}
	l.sweep(now.Add(ledgerTTL))
	if len(l.entries) != 0 {
		t.Fatal("expired entry survived the sweep")
	}
}

// A rule's SuppressDays decides the window, in both directions.
func TestVacationHonoursSuppressDays(t *testing.T) {
	st := openTestStore(t)
	be := testBackend(st)

	raw := []byte("From: alice@partners.test\r\nTo: %s\r\nSubject: hi\r\n\r\nbody\r\n")
	cases := []struct {
		name    string
		days    int
		ago     time.Duration
		replies int
	}{
		{"short window has passed", 1, 48 * time.Hour, 1},
		{"long window has not", 30, 10 * 24 * time.Hour, 0},
		{"zero means the default, which has not passed", 0, 3 * 24 * time.Hour, 0},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			address := fmt.Sprintf("ooo%d@oximail.test", i)
			acc, err := st.CreateAccount(address, "h", 0)
			if err != nil {
				t.Fatalf("create account: %v", err)
			}
			if _, err := st.SetVacation(acc.ID, true, "Away", "Back soon.", c.days); err != nil {
				t.Fatalf("set vacation: %v", err)
			}
			// An earlier reply went out `ago`.
			be.suppressor.AllowWithin(acc.ID, "alice@partners.test", time.Now().Add(-c.ago), be.suppressor.WindowOf(c.days))

			before, _ := st.ListDueOutbound(50)
			s := &session{backend: be, from: "alice@partners.test"}
			s.fireVacationReplies([]byte(fmt.Sprintf(string(raw), address)), []uint64{acc.ID})
			after, _ := st.ListDueOutbound(50)
			if got := len(after) - len(before); got != c.replies {
				t.Errorf("%d replies queued, want %d", got, c.replies)
			}
		})
	}
}

// The running server must actually call the sweep; a Sweep nobody calls
// is the bug this guards against.
func TestServerSweeps(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := New(addr, "oximail.test", nil, spam.Permissive(), nil, nil, ForwarderConfig{})
	swept := make(chan struct{}, 1)
	realSweep := srv.sweep
	srv.sweep = func(now time.Time) {
		realSweep(now)
		select {
		case swept <- struct{}{}:
		default:
		}
	}
	srv.sweepEvery = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("server never swept")
	}
}

func TestNewWiresTheSweeper(t *testing.T) {
	srv := New("127.0.0.1:0", "oximail.test", nil, spam.Permissive(), nil, nil, ForwarderConfig{})
	if srv.sweep == nil || srv.sweepEvery != sweepInterval {
		t.Fatal("inbound server has no sweeper")
	}
}

// The AAR on a forwarded message reports what the pipeline found.
func TestARCReportsRealAuthResults(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.CreateDomain("oximail.test"); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := st.SetDKIMKey("oximail.test", "s1", string(privPEM)); err != nil {
		t.Fatalf("set DKIM key: %v", err)
	}

	s := &session{
		backend: testBackend(st),
		from:    "alice@partners.test",
		auth: spam.AuthResult{
			Checked:    true,
			SPF:        "pass",
			MailFrom:   "alice@partners.test",
			DKIM:       []spam.DKIMResult{{Result: "pass", Domain: "partners.test"}},
			DMARC:      "pass",
			FromDomain: "partners.test",
		},
	}
	raw := []byte("From: alice@partners.test\r\nTo: team@oximail.test\r\nSubject: hi\r\n\r\nbody\r\n")
	got := string(s.sealARC(raw))
	for _, want := range []string{"spf=pass smtp.mailfrom=alice@partners.test", "dkim=pass header.d=partners.test", "dmarc=pass"} {
		if !strings.Contains(got, want) {
			t.Errorf("AAR lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "spf=none") {
		t.Errorf("AAR still claims spf=none:\n%s", got)
	}
}

// stripAuthResults leaves the body alone even when it looks like a
// header, and copes with a message that has no body.
func TestStripAuthResultsEdges(t *testing.T) {
	body := "Subject: s\r\n\r\nAuthentication-Results: mx; dmarc=pass\r\n"
	if got := string(stripAuthResults([]byte(body), "mx")); got != body {
		t.Errorf("body touched: %q", got)
	}
	for in, want := range map[string]string{
		"Authentication-Results: mx; x=y\r\nSubject: s":     "Subject: s",
		"AUTHENTICATION-RESULTS: MX; x=y\r\nSubject: s\r\n": "Subject: s\r\n",
		"Authentication-Results: (c) mx; x=y\r\n\r\nbody":   "\r\nbody",
		"Authentication-Results: mxother; x=y\r\n":          "Authentication-Results: mxother; x=y\r\n",
		"Authentication-Results: \"mx\"; x=y\r\n fold\r\n":  "",
	} {
		if got := string(stripAuthResults([]byte(in), "mx")); got != want {
			t.Errorf("strip(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReceivedHeaderDropsUnsafeHelo(t *testing.T) {
	if !isPlainHostname("mail.example.com") || !isPlainHostname("[192.0.2.1]") {
		t.Error("ordinary HELO names refused")
	}
	for _, bad := range []string{"", "evil\r\nX-Injected: 1", "a b", "(comment)", strings.Repeat("a", 300)} {
		if isPlainHostname(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A full mailbox is a temporary refusal that says so, not a generic failure.
func TestQuotaIsRefusedAsMailboxFull(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.CreateAccount("full@oximail.test", "h", 0); err != nil {
		t.Fatalf("create account: %v", err)
	}
	be := testBackend(st)
	be.deliver = func(uint64, string, store.IncomingMessage) error { return store.ErrQuotaExceeded }
	err := transact(t, be, "a@elsewhere.test", []string{"full@oximail.test"}, "From: a@elsewhere.test\r\nSubject: s\r\n\r\nbody\r\n")
	var se *gosmtp.SMTPError
	if !errors.As(err, &se) || se.Code != 452 {
		t.Fatalf("got %v, want a 452", err)
	}
}
