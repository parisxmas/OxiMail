// Package spam is the layered spam pipeline, run by the inbound SMTP
// server on every incoming message. Checks run cheapest-first:
//
//  1. connection-time — DNSBL lookups, greylisting, rate limits
//  2. envelope        — SPF, DKIM, DMARC (emersion/go-msgauth)
//  3. content         — Rspamd over HTTP
//
// Connection-time state (greylisting tuples, rate counters, DNSBL cache)
// lives in OxiMem; it is disposable, so losing it just re-greylists.
package spam

// Verdict is the outcome of running the pipeline against a message.
type Verdict int

const (
	// Accept — deliver the message.
	Accept Verdict = iota
	// Greylist — temporarily reject (4xx); a legitimate sender retries.
	Greylist
	// Reject — permanently reject (5xx).
	Reject
)

// Pipeline runs the layered checks. It is created once at startup and
// shared by the SMTP servers.
type Pipeline struct {
	// rspamdURL is the Rspamd HTTP endpoint; empty disables content scan.
	rspamdURL string
}

// New builds the pipeline. An empty rspamdURL disables the content
// (Rspamd) stage; the connection-time and envelope stages still run.
func New(rspamdURL string) *Pipeline {
	return &Pipeline{rspamdURL: rspamdURL}
}

// Check runs the full pipeline against one inbound message and returns a
// verdict. `remoteIP` is the connecting client, `mailFrom`/`rcptTo` are
// the SMTP envelope, and `raw` is the complete RFC 5322 message.
//
// TODO: implement the three stages. For now every message is accepted so
// the rest of the pipeline can be built and tested end to end.
func (p *Pipeline) Check(remoteIP, mailFrom string, rcptTo []string, raw []byte) (Verdict, error) {
	_ = remoteIP
	_ = mailFrom
	_ = rcptTo
	_ = raw
	return Accept, nil
}
