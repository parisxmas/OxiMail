// Package spam is OxiMail's layered spam pipeline, run by the inbound
// SMTP server on every incoming message. The stages run cheapest-first
// and short-circuit — the first stage that does not return Accept wins:
//
//  1. connection-time — rate limiting, DNS blocklists, greylisting
//  2. envelope        — SPF, DKIM, DMARC (native Go, no Rspamd)
//
// Rate limiting and blocklists depend only on the client IP, so the
// SMTP server runs them through CheckIP at MAIL FROM, before it has
// read a body; greylisting and the envelope stage run through
// CheckMessage after DATA.
//
// Both stages are implemented. Stage 1 (ratelimit.go, dnsbl.go,
// greylist.go) keeps its state in memory, which is fine because it is
// disposable — losing it on a restart just re-greylists and resets
// counters. Stage 2 (envelope.go) runs SPF / DKIM / DMARC via
// blitiri.com.ar/go/spf + emersion/go-msgauth, rejects only on a DMARC
// p=reject failure, and reports its results as an AuthResult.
//
// Earlier revisions of this package also called Rspamd as a stage-3
// content scanner. That gave us URL/MIME heuristics and a phishing
// classifier but cost ~140 MB of resident memory in a sidecar
// container. For a single-mailbox personal server the auth-only
// pipeline (stage 1 + stage 2) plus the AV scanner in internal/av
// covers the realistic threat surface, so the rspamd dependency is
// gone.
//
// TODO: move stage-1 state to OxiMem so a multi-instance deployment
// shares one view of each sender.
package spam

import (
	"context"
	"log"
	"time"
)

// Verdict is the outcome of running the pipeline against a message.
type Verdict int

const (
	// Accept — deliver the message into INBOX.
	Accept Verdict = iota
	// Greylist — temporarily reject (4xx); a legitimate sender retries.
	Greylist
	// Reject — permanently reject (5xx).
	Reject
	// Quarantine — accept the message, but file it into the Junk
	// folder rather than INBOX. Used by the envelope stage on a DMARC
	// p=quarantine failure, and when the From header cannot be parsed.
	Quarantine
	// RateLimited — temporarily reject (4xx): the client is over the
	// per-IP rate limit and may come back once its window has passed.
	RateLimited
)

// Connection-time defaults. They are reasonable starting points; TODO:
// make them operator-configurable via internal/config.
const (
	defaultRateLimit     = 100         // messages per IP...
	defaultRateWindow    = time.Minute // ...per this window
	defaultGreylistDelay = time.Minute // a pending tuple must wait this long
	sweepInterval        = 10 * time.Minute
)

// defaultDNSBLZones is the set of DNS blocklists queried by default.
var defaultDNSBLZones = []string{"zen.spamhaus.org"}

// Pipeline runs the layered checks. It is created once at startup, shared
// by the SMTP servers, and runs a background sweeper (Start) to bound the
// memory its connection-time state uses.
type Pipeline struct {
	// disabled short-circuits Check to Accept — see Permissive.
	disabled bool

	rateLimit *rateLimiter
	dnsbl     *dnsblChecker
	greylist  *greylister
	envelope  *envelopeChecker
}

// New builds the pipeline. A nil dnsblZones uses the default list, an
// explicit empty slice disables DNSBL queries entirely. A zero
// greylistDelay short-circuits greylisting (it accepts on first
// contact instead); a negative value uses the package default.
func New(dnsblZones []string, greylistDelay time.Duration) *Pipeline {
	if dnsblZones == nil {
		dnsblZones = defaultDNSBLZones
	}
	if greylistDelay < 0 {
		greylistDelay = defaultGreylistDelay
	}
	return &Pipeline{
		rateLimit: newRateLimiter(defaultRateLimit, defaultRateWindow),
		dnsbl:     newDNSBLChecker(dnsblZones),
		greylist:  newGreylister(greylistDelay),
		envelope:  newEnvelopeChecker(),
	}
}

// Permissive returns a pipeline that accepts every message without any
// checks. It is for tests, and for operators who run their spam
// filtering elsewhere (or not at all).
func Permissive() *Pipeline {
	return &Pipeline{disabled: true}
}

// Start runs the background sweeper that expires stale connection-time
// state, until ctx is cancelled.
func (p *Pipeline) Start(ctx context.Context) error {
	if p.disabled {
		log.Print("spam: pipeline disabled — accepting all mail")
		<-ctx.Done()
		return nil
	}
	log.Print("spam: pipeline started")
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.rateLimit.sweep()
			p.greylist.sweep()
		}
	}
}

// Stop logs the transition; the sweeper actually stops when the context
// passed to Start is cancelled.
func (p *Pipeline) Stop() error {
	log.Print("spam: stopped")
	return nil
}

// Check runs the whole pipeline against one inbound message. `remoteIP`
// is the connecting client, `mailFrom`/`rcptTo` are the SMTP envelope,
// and `raw` is the complete RFC 5322 message. The SMTP server runs the
// same stages in two steps, CheckIP and CheckMessage, so that it can
// turn a client away before reading the body.
func (p *Pipeline) Check(remoteIP, mailFrom string, rcptTo []string, raw []byte) (Verdict, error) {
	if v := p.CheckIP(remoteIP); v != Accept {
		return v, nil
	}
	v, _, err := p.CheckMessage(remoteIP, mailFrom, rcptTo, raw)
	return v, err
}

// CheckIP runs the checks that need only the client IP: rate limiting,
// then DNS blocklists — cheapest first. Each call counts as one message
// against the rate limit, so call it once per mail transaction. A
// message with no connection information (e.g. a local injection)
// skips them.
func (p *Pipeline) CheckIP(remoteIP string) Verdict {
	if p.disabled || remoteIP == "" {
		return Accept
	}
	if !p.rateLimit.allow(remoteIP) {
		log.Printf("spam: %s is over the rate limit — deferring", remoteIP)
		return RateLimited
	}
	if p.dnsbl.listed(remoteIP) {
		log.Printf("spam: %s is on a DNS blocklist — rejecting", remoteIP)
		return Reject
	}
	return Accept
}

// CheckMessage runs the stages that follow CheckIP: greylisting, then
// the envelope stage. A greylisted message returns a zero AuthResult,
// which does not matter because it is not delivered.
func (p *Pipeline) CheckMessage(remoteIP, mailFrom string, _ []string, raw []byte) (Verdict, AuthResult, error) {
	if p.disabled {
		return Accept, AuthResult{}, nil
	}
	if remoteIP != "" {
		if v := p.greylist.check(remoteIP, mailFrom); v != Accept {
			return v, AuthResult{}, nil
		}
	}
	// Stage 2 — envelope (SPF / DKIM / DMARC).
	v, res := p.envelope.check(remoteIP, mailFrom, raw)
	return v, res, nil
}
