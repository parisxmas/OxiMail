package spam

import (
	"bytes"
	"context"
	"net"
	"strings"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// authTimeout bounds the DNS work of one SPF evaluation; it runs on
// every inbound message, inside the SMTP DATA exchange.
const authTimeout = 20 * time.Second

// envelopeChecker is the envelope stage: it runs SPF and DKIM, looks up
// the DMARC policy of every From domain, and rejects a message only
// when such a policy is p=reject and the message fails DMARC — neither
// SPF nor DKIM passes, aligned with that From domain. A From that does
// not parse cannot be checked at all, so that message is quarantined
// rather than waved through. Anything else — a DMARC pass, p=none, no
// DMARC record, or any DNS error — yields Accept. The stage never
// temp-fails mail.
//
// The resolvers are struct fields so tests can supply fake DNS.
type envelopeChecker struct {
	resolver  spf.DNSResolver                       // for SPF
	lookupTXT func(domain string) ([]string, error) // for DKIM + DMARC
}

func newEnvelopeChecker() *envelopeChecker {
	return &envelopeChecker{
		resolver:  net.DefaultResolver,
		lookupTXT: net.LookupTXT,
	}
}

// check authenticates an inbound message and returns its verdict along
// with the SPF, DKIM and DMARC results behind it. SPF and DKIM run for
// every message, DMARC policy or not, because the results are reported
// on delivered mail.
func (e *envelopeChecker) check(remoteIP, mailFrom string, raw []byte) (Verdict, AuthResult) {
	res := AuthResult{Checked: true, MailFrom: mailFrom, DMARC: "none"}

	// SPF authenticates the envelope (MAIL FROM) domain.
	spfResult := spf.None
	spfDomain := domainOf(mailFrom)
	if ip := net.ParseIP(remoteIP); ip != nil && spfDomain != "" {
		ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
		spfResult, _ = spf.CheckHostWithSender(ip, "", mailFrom,
			spf.WithResolver(e.resolver), spf.WithContext(ctx))
		cancel()
	}
	res.SPF = string(spfResult)

	// DKIM — gather the domains of the message's valid signatures.
	var dkimDomains []string
	if verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: e.lookupTXT,
	}); err == nil {
		for _, v := range verifs {
			res.DKIM = append(res.DKIM, DKIMResult{Result: dkimResultOf(v.Err), Domain: v.Domain})
			if v.Err == nil {
				dkimDomains = append(dkimDomains, v.Domain)
			}
		}
	}

	domains, ok := fromDomains(raw)
	if !ok {
		res.DMARC = "permerror"
		return Quarantine, res
	}

	// Every From domain is held to its own policy, and the harshest
	// outcome wins: an added address must not dilute a forged one.
	verdict := Accept
	res.FromDomain = domains[0]
	for _, from := range domains {
		record, policy, found := e.dmarcPolicy(from)
		if !found {
			continue // no DMARC record — nothing to hold this domain to
		}
		if dmarcPass(record, from, spfResult, spfDomain, dkimDomains) {
			if res.DMARC == "none" {
				res.DMARC, res.FromDomain = "pass", from
			}
			continue
		}
		res.DMARC, res.FromDomain = "fail", from
		if v := evaluate(record, policy, from, spfResult, spfDomain, dkimDomains); v == Reject || verdict == Accept {
			verdict = v
		}
	}
	return verdict, res
}

// dkimResultOf maps a signature's verification error to its RFC 8601
// result.
func dkimResultOf(err error) string {
	switch {
	case err == nil:
		return "pass"
	case dkim.IsTempFail(err):
		return "temperror"
	case dkim.IsPermFail(err):
		return "permerror"
	default:
		return "fail"
	}
}

// evaluate applies a DMARC policy to the SPF and DKIM results. It is
// pure — no DNS — so the verdict logic is unit-testable on its own.
func evaluate(record *dmarc.Record, policy dmarc.Policy, fromDomain string,
	spfResult spf.Result, spfDomain string, dkimDomains []string) Verdict {

	if dmarcPass(record, fromDomain, spfResult, spfDomain, dkimDomains) {
		return Accept
	}
	// DMARC failed. Honor the policy: p=reject is a 5xx rejection;
	// p=quarantine files the message into the Junk folder rather
	// than INBOX; p=none accepts.
	switch policy {
	case dmarc.PolicyReject:
		return Reject
	case dmarc.PolicyQuarantine:
		return Quarantine
	default:
		return Accept
	}
}

// dmarcPass reports whether SPF or DKIM passed, aligned with the From
// domain.
func dmarcPass(record *dmarc.Record, fromDomain string,
	spfResult spf.Result, spfDomain string, dkimDomains []string) bool {

	if spfResult == spf.Pass && aligned(spfDomain, fromDomain, record.SPFAlignment) {
		return true
	}
	for _, d := range dkimDomains {
		if aligned(d, fromDomain, record.DKIMAlignment) {
			return true
		}
	}
	return false
}

// dmarcPolicy discovers the DMARC record and effective policy for a
// domain: the domain's own record if it has one, otherwise the
// organizational domain's record (applying its subdomain policy).
func (e *envelopeChecker) dmarcPolicy(fromDomain string) (*dmarc.Record, dmarc.Policy, bool) {
	opts := &dmarc.LookupOptions{LookupTXT: e.lookupTXT}
	if rec, err := dmarc.LookupWithOptions(fromDomain, opts); err == nil {
		return rec, rec.Policy, true
	}
	org := orgDomain(fromDomain)
	if org == "" || org == fromDomain {
		return nil, "", false
	}
	rec, err := dmarc.LookupWithOptions(org, opts)
	if err != nil {
		return nil, "", false
	}
	policy := rec.Policy
	if rec.SubdomainPolicy != "" {
		policy = rec.SubdomainPolicy // a subdomain inherits sp=, when set
	}
	return rec, policy, true
}

// aligned reports whether an authenticated domain is DMARC-aligned with
// the From header domain. Strict alignment needs an exact match; relaxed
// (the default) needs the organizational domains to match.
func aligned(authDomain, fromDomain string, mode dmarc.AlignmentMode) bool {
	a := strings.ToLower(strings.TrimSuffix(authDomain, "."))
	f := strings.ToLower(fromDomain)
	if a == "" || f == "" {
		return false
	}
	if a == f {
		return true
	}
	if mode == dmarc.AlignmentStrict {
		return false
	}
	return orgDomain(a) == orgDomain(f)
}

// orgDomain returns the organizational domain (eTLD+1) of a hostname,
// e.g. "example.com" for "mail.example.com". On any failure it returns
// the input lower-cased.
func orgDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	org, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return domain
	}
	return org
}

// fromDomains returns the distinct domains of the message's From
// addresses. ok is false when the From header is missing or does not
// parse, or an address has no domain.
func fromDomains(raw []byte) (domains []string, ok bool) {
	addrs, err := FromAddresses(raw)
	if err != nil {
		return nil, false
	}
	seen := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		d := domainOf(addr)
		if d == "" {
			return nil, false
		}
		if !seen[d] {
			seen[d] = true
			domains = append(domains, d)
		}
	}
	return domains, true
}

// domainOf returns the lower-cased domain part of an email address.
func domainOf(address string) string {
	at := strings.LastIndexByte(address, '@')
	if at < 0 || at == len(address)-1 {
		return ""
	}
	return strings.ToLower(address[at+1:])
}
