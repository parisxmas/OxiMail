//go:build integration

package queue

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/mtasts"
)

// testQueue is a Queue with just enough set for dialMX.
func testQueue() *Queue { return &Queue{hostname: "mail.oximail.test"} }

// startStubMX starts a TCP listener that accepts a connection and
// closes it without saying anything — enough to make a real SMTP
// client fail its EHLO. Returns the "host:port" target and a cleanup
// hook.
func startStubMX(t *testing.T) (target string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestDialMXWithoutPolicyFallsBackToCleartext(t *testing.T) {
	// The capture MX speaks SMTP but offers no STARTTLS.
	_, target := startCaptureMX(t)

	c, err := testQueue().dialMX([]string{target}, "remote.test", nil)
	if err != nil {
		t.Fatalf("dialMX no policy: %v", err)
	}
	defer c.Close()
	if _, ok := c.TLSConnectionState(); ok {
		t.Fatal("got a TLS connection from a server without STARTTLS")
	}
}

func TestDialMXEnforceRejectsWhenStartTLSFails(t *testing.T) {
	_, target := startCaptureMX(t)

	// Enforce mode whitelists the MX's hostname but it does not speak
	// STARTTLS, so the dial must fail with the MTA-STS sentinel error
	// — and not silently fall back to cleartext.
	policy := &mtasts.Policy{
		Version: "STSv1",
		Mode:    mtasts.ModeEnforce,
		MX:      []string{hostOnly(target)},
		MaxAge:  time.Hour,
	}
	c, err := testQueue().dialMX([]string{target}, "remote.test", policy)
	if c != nil {
		_ = c.Close()
		t.Fatal("dialMX returned a client under enforce mode against a server that cannot speak STARTTLS")
	}
	if !errors.Is(err, errMTASTSReject) {
		t.Fatalf("err = %v, want errMTASTSReject", err)
	}
}

func TestDialMXEnforceRejectsDisallowedMX(t *testing.T) {
	target, cleanup := startStubMX(t)
	defer cleanup()

	// Policy in enforce mode does not list our stub's hostname.
	policy := &mtasts.Policy{
		Version: "STSv1",
		Mode:    mtasts.ModeEnforce,
		MX:      []string{"only-this-host.example.test"},
		MaxAge:  time.Hour,
	}
	c, err := testQueue().dialMX([]string{target}, "remote.test", policy)
	if c != nil {
		_ = c.Close()
		t.Fatal("dialMX returned a client for an MX not on the enforce list")
	}
	if err == nil || !strings.Contains(err.Error(), "MTA-STS") {
		t.Fatalf("err = %v, want one mentioning MTA-STS", err)
	}
}

func TestDialMXTestingModeWarnsButProceeds(t *testing.T) {
	_, target := startCaptureMX(t)

	policy := &mtasts.Policy{
		Version: "STSv1",
		Mode:    mtasts.ModeTesting,
		MX:      []string{"only-this-host.example.test"},
		MaxAge:  time.Hour,
	}
	// Testing-mode policy with a non-matching MX should LOG but still
	// fall through to the opportunistic-STARTTLS dial, eventually
	// producing a non-nil client over cleartext.
	c, err := testQueue().dialMX([]string{target}, "remote.test", policy)
	if err != nil {
		t.Fatalf("dialMX in testing mode failed: %v", err)
	}
	if c == nil {
		t.Fatal("dialMX in testing mode returned no client")
	}
	_ = c.Close()
}

// fixedPolicy is an mtasts.Lookuper returning one policy for any domain.
type fixedPolicy struct{ p *mtasts.Policy }

func (f fixedPolicy) Lookup(context.Context, string) (*mtasts.Policy, error) {
	if f.p == nil {
		return nil, mtasts.ErrNoPolicy
	}
	return f.p, nil
}

// lockedBuffer is a bytes.Buffer safe to share with a server goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startTLSMX runs a capture MX offering STARTTLS with the given config.
// It returns the MX, its address and a transcript of the session.
func startTLSMX(t *testing.T, cfg *tls.Config) (*captureMX, string, *lockedBuffer) {
	t.Helper()
	mx := &captureMX{}
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	transcript := &lockedBuffer{}
	srv := gosmtp.NewServer(mx)
	srv.Addr = addr
	srv.Domain = "tls-mx.test"
	srv.TLSConfig = cfg
	srv.Debug = transcript
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	itest.WaitTCP(t, addr)
	return mx, addr, transcript
}

// selfSigned returns a server TLS config, and a pool that trusts it.
func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	certFile, keyFile := itest.WriteSelfSignedCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{Certificates: []tls.Certificate{cert}}, pool
}

// The plaintext EHLO before STARTTLS must carry the real hostname, not
// the library's "localhost".
func TestFirstEHLOUsesRealHostname(t *testing.T) {
	cfg, _ := selfSigned(t)
	_, target, transcript := startTLSMX(t, cfg)

	c, err := testQueue().dialMX([]string{target}, "remote.test", nil)
	if err != nil {
		t.Fatalf("dialMX: %v", err)
	}
	defer c.Close()
	if _, ok := c.TLSConnectionState(); !ok {
		t.Fatal("connection is not TLS")
	}
	got := transcript.String()
	if strings.Contains(got, "localhost") {
		t.Errorf("EHLO said localhost:\n%s", got)
	}
	if n := strings.Count(got, "EHLO mail.oximail.test"); n != 2 {
		t.Errorf("saw %d EHLOs with the real hostname, want 2 (before and after STARTTLS):\n%s", n, got)
	}
}

// Under enforce a bad certificate on the first MX must not end the
// attempt: the next MX is tried, over verified TLS.
func TestDialMXEnforceTriesNextMXAfterCertFailure(t *testing.T) {
	badCfg, _ := selfSigned(t)
	goodCfg, trusted := selfSigned(t)
	_, bad, _ := startTLSMX(t, badCfg)
	_, good, _ := startTLSMX(t, goodCfg)

	q := testQueue()
	q.rootCAs = trusted
	policy := &mtasts.Policy{Version: "STSv1", Mode: mtasts.ModeEnforce, MX: []string{"127.0.0.1"}, MaxAge: time.Hour}
	c, err := q.dialMX([]string{bad, good}, "remote.test", policy)
	if err != nil {
		t.Fatalf("dialMX: %v", err)
	}
	defer c.Close()
	state, ok := c.TLSConnectionState()
	if !ok || !state.HandshakeComplete {
		t.Fatalf("connection is not verified TLS: ok=%v state=%+v", ok, state)
	}
}

// A certificate failure on every MX is a policy refusal, which is
// permanent, and it must never reach cleartext.
func TestDialMXEnforceCertFailureIsPolicyRejection(t *testing.T) {
	cfg, _ := selfSigned(t)
	mx, target, transcript := startTLSMX(t, cfg)

	policy := &mtasts.Policy{Version: "STSv1", Mode: mtasts.ModeEnforce, MX: []string{"127.0.0.1"}, MaxAge: time.Hour}
	c, err := testQueue().dialMX([]string{target}, "remote.test", policy)
	if c != nil {
		_ = c.Close()
		t.Fatal("dialMX returned a client despite an untrusted certificate")
	}
	if !errors.Is(err, errMTASTSReject) {
		t.Fatalf("err = %v, want errMTASTSReject", err)
	}
	if strings.Contains(transcript.String(), "MAIL FROM") || len(mx.messages()) != 0 {
		t.Fatal("mail went out after a certificate failure")
	}
}

// An unreachable MX has not been refused by the policy. That failure
// must stay temporary, or a network blip bounces mail on the spot.
func TestDialMXEnforceUnreachableIsNotPolicyRejection(t *testing.T) {
	dead := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	policy := &mtasts.Policy{Version: "STSv1", Mode: mtasts.ModeEnforce, MX: []string{"127.0.0.1"}, MaxAge: time.Hour}

	c, err := testQueue().dialMX([]string{dead}, "remote.test", policy)
	if c != nil {
		_ = c.Close()
		t.Fatal("got a client from a dead MX")
	}
	if err == nil || errors.Is(err, errMTASTSReject) {
		t.Fatalf("err = %v, want a plain connection error", err)
	}

	// A connection dropped before the greeting says no more than that.
	stub, cleanup := startStubMX(t)
	defer cleanup()
	_, err = testQueue().dialMX([]string{stub}, "remote.test", policy)
	if err == nil || errors.Is(err, errMTASTSReject) {
		t.Fatalf("dropped connection: err = %v, want a temporary error", err)
	}
	if !strings.Contains(err.Error(), "MTA-STS") {
		t.Errorf("err = %v, want one mentioning MTA-STS", err)
	}
}

// Outside enforce a first MX that accepts the connection and then hangs
// up must not end the attempt either.
func TestDialMXTriesNextMXAfterHelloFailure(t *testing.T) {
	stub, cleanup := startStubMX(t)
	defer cleanup()
	_, good := startCaptureMX(t)

	c, err := testQueue().dialMX([]string{stub, good}, "remote.test", nil)
	if err != nil {
		t.Fatalf("dialMX: %v", err)
	}
	_ = c.Close()
}
