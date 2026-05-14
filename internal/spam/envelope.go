package spam

import (
	"bytes"
	"context"
	"net"
	"net/mail"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// envelopeChecker is the envelope stage: it runs SPF and DKIM, looks up
// the From domain's DMARC policy, and rejects a message only when that
// policy is p=reject and the message fails DMARC — neither SPF nor DKIM
// passes, aligned with the From domain. Anything else — a DMARC pass,
// p=none, p=quarantine, no DMARC record, or any DNS error — yields
// Accept. The stage never temp-fails mail.
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

// check authenticates an inbound message and returns its verdict.
func (e *envelopeChecker) check(remoteIP, mailFrom string, raw []byte) Verdict {
	from := fromHeaderDomain(raw)
	if from == "" {
		return Accept // no usable From domain — DMARC cannot apply
	}
	record, policy, found := e.dmarcPolicy(from)
	if !found || policy == dmarc.PolicyNone {
		return Accept // no DMARC record, or monitor-only
	}

	// SPF authenticates the envelope (MAIL FROM) domain.
	spfResult := spf.None
	spfDomain := domainOf(mailFrom)
	if ip := net.ParseIP(remoteIP); ip != nil && spfDomain != "" {
		spfResult, _ = spf.CheckHostWithSender(ip, "", mailFrom,
			spf.WithResolver(e.resolver), spf.WithContext(context.Background()))
	}

	// DKIM — gather the domains of the message's valid signatures.
	var dkimDomains []string
	if verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: e.lookupTXT,
	}); err == nil {
		for _, v := range verifs {
			if v.Err == nil {
				dkimDomains = append(dkimDomains, v.Domain)
			}
		}
	}

	return evaluate(record, policy, from, spfResult, spfDomain, dkimDomains)
}

// evaluate applies a DMARC policy to the SPF and DKIM results. It is
// pure — no DNS — so the verdict logic is unit-testable on its own.
func evaluate(record *dmarc.Record, policy dmarc.Policy, fromDomain string,
	spfResult spf.Result, spfDomain string, dkimDomains []string) Verdict {

	pass := spfResult == spf.Pass &&
		aligned(spfDomain, fromDomain, record.SPFAlignment)
	if !pass {
		for _, d := range dkimDomains {
			if aligned(d, fromDomain, record.DKIMAlignment) {
				pass = true
				break
			}
		}
	}
	if pass {
		return Accept
	}
	// DMARC failed. Only an explicit p=reject rejects the message;
	// p=quarantine is delivered for now.
	// TODO: honor p=quarantine by tagging the message as junk.
	if policy == dmarc.PolicyReject {
		return Reject
	}
	return Accept
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

// fromHeaderDomain extracts the domain of the message's From header.
func fromHeaderDomain(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	addr, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		return ""
	}
	return domainOf(addr.Address)
}

// domainOf returns the lower-cased domain part of an email address.
func domainOf(address string) string {
	at := strings.LastIndexByte(address, '@')
	if at < 0 || at == len(address)-1 {
		return ""
	}
	return strings.ToLower(address[at+1:])
}
