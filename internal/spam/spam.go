// Package spam is OxiMail's layered spam pipeline, run by the inbound
// SMTP server on every incoming message. The stages run cheapest-first
// and short-circuit — the first stage that does not return Accept wins:
//
//  1. connection-time — rate limiting, DNS blocklists, greylisting
//  2. envelope        — SPF, DKIM, DMARC
//  3. content         — Rspamd over HTTP
//
// All three stages are implemented. Stage 1 (ratelimit.go, dnsbl.go,
// greylist.go) keeps its state in memory, which is fine because it is
// disposable — losing it on a restart just re-greylists and resets
// counters. Stage 2 (envelope.go) runs SPF / DKIM / DMARC and rejects
// only on a DMARC p=reject failure. Stage 3 (rspamd.go) runs only when a
// Rspamd endpoint is configured.
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
	// p=quarantine failure.
	Quarantine
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
	// rspamd is the content stage; nil when no Rspamd endpoint is
	// configured.
	rspamd *rspamdChecker
}

// New builds the pipeline. An empty rspamdURL disables the content
// (Rspamd) stage; the connection-time stage always runs.
func New(rspamdURL string) *Pipeline {
	p := &Pipeline{
		rateLimit: newRateLimiter(defaultRateLimit, defaultRateWindow),
		dnsbl:     newDNSBLChecker(defaultDNSBLZones),
		greylist:  newGreylister(defaultGreylistDelay),
		envelope:  newEnvelopeChecker(),
	}
	if rspamdURL != "" {
		p.rspamd = newRspamdChecker(rspamdURL)
	}
	return p
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

// Check runs the pipeline against one inbound message. `remoteIP` is the
// connecting client, `mailFrom`/`rcptTo` are the SMTP envelope, and `raw`
// is the complete RFC 5322 message. rcptTo and raw are unused until the
// envelope and content stages land.
func (p *Pipeline) Check(remoteIP, mailFrom string, rcptTo []string, raw []byte) (Verdict, error) {
	if p.disabled {
		return Accept, nil
	}
	// Stage 1 — connection-time.
	if v := p.checkConnection(remoteIP, mailFrom); v != Accept {
		return v, nil
	}
	// Stage 2 — envelope (SPF / DKIM / DMARC).
	if v := p.envelope.check(remoteIP, mailFrom, raw); v != Accept {
		return v, nil
	}
	// Stage 3 — content: Rspamd, when an endpoint is configured.
	if p.rspamd != nil {
		if v := p.rspamd.check(remoteIP, mailFrom, rcptTo, raw); v != Accept {
			return v, nil
		}
	}
	return Accept, nil
}

// checkConnection runs the connection-time stage: rate limiting, DNS
// blocklists, then greylisting — cheapest first. A message with no
// connection information (e.g. a local injection) skips the stage.
func (p *Pipeline) checkConnection(remoteIP, mailFrom string) Verdict {
	if remoteIP == "" {
		return Accept
	}
	if !p.rateLimit.allow(remoteIP) {
		log.Printf("spam: %s is over the rate limit — rejecting", remoteIP)
		return Reject
	}
	if p.dnsbl.listed(remoteIP) {
		log.Printf("spam: %s is on a DNS blocklist — rejecting", remoteIP)
		return Reject
	}
	return p.greylist.check(remoteIP, mailFrom)
}
