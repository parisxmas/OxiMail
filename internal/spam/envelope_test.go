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
	if v, _ := e.check("9.9.9.9", "x@nodmarc.test", envMsg("x@nodmarc.test")); v != Accept {
		t.Errorf("no DMARC record: got %v, want Accept", v)
	}
	// p=none is monitor-only — accept regardless of SPF.
	if v, _ := e.check("9.9.9.9", "x@lax.test", envMsg("x@lax.test")); v != Accept {
		t.Errorf("p=none: got %v, want Accept", v)
	}
	// p=reject, SPF authorizes the sending IP, MAIL FROM aligned — pass.
	if v, _ := e.check("10.0.0.1", "x@strict.test", envMsg("x@strict.test")); v != Accept {
		t.Errorf("p=reject + aligned SPF pass: got %v, want Accept", v)
	}
	// p=reject, SPF does not authorize the IP, no DKIM — DMARC fails.
	if v, _ := e.check("9.9.9.9", "x@strict.test", envMsg("x@strict.test")); v != Reject {
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
	if v, _ := e.check("9.9.9.9", "alice@signed.test", signed.Bytes()); v != Accept {
		t.Errorf("valid aligned DKIM under p=reject: got %v, want Accept", v)
	}
	// The same domain, but unsigned: SPF -all and no DKIM — DMARC fails.
	if v, _ := e.check("9.9.9.9", "alice@signed.test", envMsg("alice@signed.test")); v != Reject {
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

// multiFromDNS has a p=reject domain, a p=quarantine domain and a
// domain with no DMARC record that authorizes the test client, so a
// message naming several tells whether each From domain is checked.
func multiFromDNS() *fakeDNS {
	return &fakeDNS{txt: map[string][]string{
		"_dmarc.strict.test": {"v=DMARC1; p=reject"},
		"strict.test":        {"v=spf1 ip4:10.0.0.1 -all"},
		"_dmarc.quiet.test":  {"v=DMARC1; p=quarantine"},
		"attacker.test":      {"v=spf1 ip4:9.9.9.9 -all"},
	}}
}

func TestEnvelopeChecksEveryFromDomain(t *testing.T) {
	dns := multiFromDNS()
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}

	cases := []struct {
		name string
		from string
		want Verdict
	}{
		{"forged address second", "x@attacker.test, ceo@strict.test", Reject},
		{"forged address first", "ceo@strict.test, x@attacker.test", Reject},
		{"quarantine domain", "x@attacker.test, a@quiet.test", Quarantine},
		{"reject outranks quarantine", "a@quiet.test, ceo@strict.test", Reject},
		{"quarantine does not soften reject", "ceo@strict.test, a@quiet.test", Reject},
		{"no policy anywhere", "x@attacker.test, y@attacker.test", Accept},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if v, _ := e.check("9.9.9.9", "x@attacker.test", envMsg(c.from)); v != c.want {
				t.Errorf("From %q: got %v, want %v", c.from, v, c.want)
			}
		})
	}
}

// A second From field must not hide a forged address from the check.
func TestEnvelopeChecksEveryFromField(t *testing.T) {
	dns := multiFromDNS()
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}
	raw := []byte("From: x@attacker.test\r\nFrom: ceo@strict.test\r\nSubject: t\r\n\r\nbody\r\n")
	if v, _ := e.check("9.9.9.9", "x@attacker.test", raw); v != Reject {
		t.Errorf("got %v, want Reject", v)
	}
}

func TestEnvelopeQuarantinesUnusableFrom(t *testing.T) {
	dns := multiFromDNS()
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}

	for name, raw := range map[string]string{
		"unparseable": "From: <<not an address\r\nSubject: t\r\n\r\nbody\r\n",
		"missing":     "Subject: t\r\n\r\nbody\r\n",
		"empty":       "From:\r\nSubject: t\r\n\r\nbody\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			v, res := e.check("9.9.9.9", "x@attacker.test", []byte(raw))
			if v != Quarantine {
				t.Errorf("got %v, want Quarantine", v)
			}
			if res.DMARC != "permerror" {
				t.Errorf("DMARC result = %q, want permerror", res.DMARC)
			}
		})
	}
}

// A display name in a charset net/mail cannot decode is not a broken
// From: the address is fine, so DMARC must still apply to it.
func TestEnvelopeHandlesEncodedDisplayNames(t *testing.T) {
	dns := multiFromDNS()
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}

	for _, name := range []string{
		"=?UTF-8?B?QmFsdGEgVmlzdGE=?=",
		"=?windows-1254?Q?Bar=FD=FE?=",
		"=?iso-8859-9?B?QmFy/Q==?=",
	} {
		from := name + " <ceo@strict.test>"
		if v, _ := e.check("9.9.9.9", "x@attacker.test", envMsg(from)); v != Reject {
			t.Errorf("From %q: got %v, want Reject (DMARC applied)", from, v)
		}
		if v, _ := e.check("10.0.0.1", "ceo@strict.test", envMsg(from)); v != Accept {
			t.Errorf("From %q from an authorized host: got %v, want Accept", from, v)
		}
	}
}

// SPF and DKIM are reported even when the domain has no DMARC policy.
func TestAuthResultWithoutDMARCPolicy(t *testing.T) {
	dns := multiFromDNS()
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}

	v, res := e.check("9.9.9.9", "x@attacker.test", envMsg("x@attacker.test"))
	if v != Accept {
		t.Fatalf("verdict = %v, want Accept", v)
	}
	got := res.Value("mx.oximail.test")
	want := "mx.oximail.test; spf=pass smtp.mailfrom=x@attacker.test; dkim=none; dmarc=none header.from=attacker.test"
	if got != want {
		t.Errorf("Value =\n  %s\nwant\n  %s", got, want)
	}
}

func TestAuthResultReportsDMARC(t *testing.T) {
	dns := multiFromDNS()
	e := &envelopeChecker{resolver: dns, lookupTXT: dns.lookupTXT}

	_, pass := e.check("10.0.0.1", "ceo@strict.test", envMsg("ceo@strict.test"))
	if v := pass.Value("mx"); !strings.Contains(v, "spf=pass") || !strings.Contains(v, "dmarc=pass header.from=strict.test") {
		t.Errorf("pass case: %s", v)
	}
	_, fail := e.check("9.9.9.9", "ceo@strict.test", envMsg("ceo@strict.test"))
	if v := fail.Value("mx"); !strings.Contains(v, "spf=fail") || !strings.Contains(v, "dmarc=fail header.from=strict.test") {
		t.Errorf("fail case: %s", v)
	}
}

func TestAuthResultValueQuotesHostileValues(t *testing.T) {
	res := AuthResult{
		Checked:  true,
		SPF:      "none",
		MailFrom: "\"a; dmarc=pass\r\nX-Evil: 1\"@evil.test",
		DMARC:    "none",
	}
	got := res.Value("mx")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("value contains a line break: %q", got)
	}
	if !strings.Contains(got, `smtp.mailfrom="\"a; dmarc=pass??X-Evil: 1\"@evil.test"`) {
		t.Errorf("value not quoted: %s", got)
	}
	if got := (AuthResult{}).Value("mx"); got != "mx; none" {
		t.Errorf("zero value = %q, want %q", got, "mx; none")
	}
}
