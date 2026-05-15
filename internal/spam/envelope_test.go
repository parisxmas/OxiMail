package spam

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
)

func TestOrgDomain(t *testing.T) {
	cases := map[string]string{
		"example.com":         "example.com",
		"mail.example.com":    "example.com",
		"a.b.c.example.co.uk": "example.co.uk",
		"EXAMPLE.COM":         "example.com",
	}
	for in, want := range cases {
		if got := orgDomain(in); got != want {
			t.Errorf("orgDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAligned(t *testing.T) {
	cases := []struct {
		auth, from string
		mode       dmarc.AlignmentMode
		want       bool
	}{
		{"example.com", "example.com", dmarc.AlignmentRelaxed, true},
		{"example.com", "example.com", dmarc.AlignmentStrict, true},
		{"mail.example.com", "example.com", dmarc.AlignmentRelaxed, true},  // org domains match
		{"mail.example.com", "example.com", dmarc.AlignmentStrict, false}, // not an exact match
		{"example.com", "evil.test", dmarc.AlignmentRelaxed, false},
		{"", "example.com", dmarc.AlignmentRelaxed, false},
	}
	for _, c := range cases {
		if got := aligned(c.auth, c.from, c.mode); got != c.want {
			t.Errorf("aligned(%q, %q, %q) = %v, want %v", c.auth, c.from, c.mode, got, c.want)
		}
	}
}

func TestEvaluate(t *testing.T) {
	relaxed := &dmarc.Record{
		SPFAlignment:  dmarc.AlignmentRelaxed,
		DKIMAlignment: dmarc.AlignmentRelaxed,
	}
	cases := []struct {
		name      string
		policy    dmarc.Policy
		spfResult spf.Result
		spfDomain string
		dkim      []string
		want      Verdict
	}{
		{"spf pass, aligned", dmarc.PolicyReject, spf.Pass, "example.com", nil, Accept},
		{"spf pass, org-aligned", dmarc.PolicyReject, spf.Pass, "mail.example.com", nil, Accept},
		{"spf pass, not aligned", dmarc.PolicyReject, spf.Pass, "evil.test", nil, Reject},
		{"dkim valid, aligned", dmarc.PolicyReject, spf.Fail, "evil.test", []string{"example.com"}, Accept},
		{"all fail, p=reject", dmarc.PolicyReject, spf.Fail, "evil.test", nil, Reject},
		{"all fail, p=quarantine", dmarc.PolicyQuarantine, spf.Fail, "evil.test", nil, Quarantine},
		{"all fail, p=none", dmarc.PolicyNone, spf.Fail, "evil.test", nil, Accept},
	}
	for _, c := range cases {
		got := evaluate(relaxed, c.policy, "example.com", c.spfResult, c.spfDomain, c.dkim)
		if got != c.want {
			t.Errorf("%s: evaluate = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEnvelopeCheck(t *testing.T) {
	dns := &fakeDNS{txt: map[string][]string{
		"_dmarc.strict.test": {"v=DMARC1; p=reject"},
		"strict.test":        {"v=spf1 ip4:10.0.0.1 -all"},
		"_dmarc.lax.test":    {"v=DMARC1; p=none"},
	}}
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}

	// No DMARC record at all — DMARC cannot apply.
	if v := e.check("9.9.9.9", "x@nodmarc.test", envMsg("x@nodmarc.test")); v != Accept {
		t.Errorf("no DMARC record: got %v, want Accept", v)
	}
	// p=none is monitor-only — accept regardless of SPF.
	if v := e.check("9.9.9.9", "x@lax.test", envMsg("x@lax.test")); v != Accept {
		t.Errorf("p=none: got %v, want Accept", v)
	}
	// p=reject, SPF authorizes the sending IP, MAIL FROM aligned — pass.
	if v := e.check("10.0.0.1", "x@strict.test", envMsg("x@strict.test")); v != Accept {
		t.Errorf("p=reject + aligned SPF pass: got %v, want Accept", v)
	}
	// p=reject, SPF does not authorize the IP, no DKIM — DMARC fails.
	if v := e.check("9.9.9.9", "x@strict.test", envMsg("x@strict.test")); v != Reject {
		t.Errorf("p=reject + SPF fail + no DKIM: got %v, want Reject", v)
	}
}

func TestEnvelopeCheckDKIM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	dkimTXT := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)

	dns := &fakeDNS{txt: map[string][]string{
		"_dmarc.signed.test":         {"v=DMARC1; p=reject"},
		"signed.test":                {"v=spf1 -all"}, // SPF fails for everyone
		"sel._domainkey.signed.test": {dkimTXT},
	}}
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}

	// Sign a message from signed.test with the matching key.
	plain := "From: alice@signed.test\r\nTo: dest@local.test\r\n" +
		"Subject: signed\r\n\r\nhello\r\n"
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, strings.NewReader(plain), &dkim.SignOptions{
		Domain:   "signed.test",
		Selector: "sel",
		Signer:   key,
	}); err != nil {
		t.Fatalf("dkim sign: %v", err)
	}

	// SPF fails (-all), but the DKIM signature is valid and aligned, so
	// DMARC passes — accepted even under p=reject.
	if v := e.check("9.9.9.9", "alice@signed.test", signed.Bytes()); v != Accept {
		t.Errorf("valid aligned DKIM under p=reject: got %v, want Accept", v)
	}
	// The same domain, but unsigned: SPF -all and no DKIM — DMARC fails.
	if v := e.check("9.9.9.9", "alice@signed.test", envMsg("alice@signed.test")); v != Reject {
		t.Errorf("unsigned under p=reject: got %v, want Reject", v)
	}
}

// envMsg builds a minimal RFC 5322 message from the given From address.
func envMsg(from string) []byte {
	return []byte("From: " + from + "\r\nTo: dest@local.test\r\n" +
		"Subject: test\r\n\r\nbody\r\n")
}

// fakeDNS is an in-memory DNS stub: it satisfies spf.DNSResolver and
// also provides a LookupTXT function for the DKIM and DMARC libraries.
type fakeDNS struct {
	txt map[string][]string
}

func (f *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if r, ok := f.txt[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (f *fakeDNS) LookupMX(context.Context, string) ([]*net.MX, error) {
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (f *fakeDNS) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (f *fakeDNS) LookupAddr(context.Context, string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

// lookupTXT is the context-free form the DKIM and DMARC libraries take.
func (f *fakeDNS) lookupTXT(name string) ([]string, error) {
	return f.LookupTXT(context.Background(), name)
}
