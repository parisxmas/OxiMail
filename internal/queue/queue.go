// Package queue is the outbound delivery queue: a background worker that
// scans the outbound_queue collection for due messages, attempts SMTP
// delivery to each recipient's MX, and reschedules temporary failures
// with exponential backoff.
//
// Queue state lives in OxiDB, so delivery survives a restart; the
// message bodies it sends are read from the blob store.
//
// A permanent failure — or a recipient still failing after the last
// retry — produces a bounce (a DSN) back to the original sender.
//
// TODO: opportunistic STARTTLS when delivering to remote MX hosts.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/store"
)

const (
	// pollInterval is how often the worker scans for due deliveries.
	pollInterval = 30 * time.Second
	// batchSize caps how many due messages one scan processes.
	batchSize = 50
	// maxAttempts is how many times a message is retried before the
	// recipients still failing are abandoned.
	maxAttempts = 5
	// baseBackoff is the delay before the first retry; it doubles on
	// each subsequent attempt up to maxBackoff.
	baseBackoff = 5 * time.Minute
	maxBackoff  = 6 * time.Hour
	// dialTimeout bounds connecting to one remote MX host.
	dialTimeout = 30 * time.Second
)

// resolver maps a recipient domain to the SMTP endpoints ("host:port")
// to try, in preference order. It is a struct field so tests can point
// a domain at a local server instead of doing real DNS.
type resolver func(domain string) ([]string, error)

// Queue is the outbound delivery worker.
type Queue struct {
	store    *store.Store
	hostname string // announced in EHLO to remote servers
	resolve  resolver
}

// New builds the outbound queue worker. `hostname` is announced in EHLO
// when connecting to remote mail servers.
func New(st *store.Store, hostname string) *Queue {
	return &Queue{store: st, hostname: hostname, resolve: lookupMX}
}

// Start runs the delivery loop until `ctx` is cancelled, then returns.
func (q *Queue) Start(ctx context.Context) error {
	log.Print("queue: outbound worker started")
	q.runOnce(ctx)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			q.runOnce(ctx)
		}
	}
}

// Stop logs the transition. The loop actually winds down when the
// context passed to Start is cancelled.
func (q *Queue) Stop() error {
	log.Print("queue: stopped")
	return nil
}

// runOnce processes one batch of due messages.
func (q *Queue) runOnce(ctx context.Context) {
	msgs, err := q.store.ListDueOutbound(batchSize)
	if err != nil {
		log.Printf("queue: list due messages: %v", err)
		return
	}
	for i := range msgs {
		if ctx.Err() != nil {
			return
		}
		q.deliver(&msgs[i])
	}
}

// deliver attempts one queued message. Recipients that permanently
// fail — and, after the final attempt, those still temp-failing — are
// bounced back to the sender; recipients still worth a retry are
// deferred with backoff; a message with nothing left to retry is
// removed from the queue.
func (q *Queue) deliver(m *store.OutboundMessage) {
	raw, err := q.store.FetchOutboundBody(m)
	if err != nil {
		log.Printf("queue: message %d: body unreadable (%v) — abandoning", m.ID, err)
		if err := q.store.CompleteOutbound(m); err != nil {
			log.Printf("queue: message %d: cleanup: %v", m.ID, err)
		}
		return
	}
	// DKIM-sign before delivery — this is the point mail leaves OxiMail
	// for another domain. signMessage is a no-op for domains without a
	// configured key.
	signed := signMessage(q.store, raw)

	var temp, perm []rcptFailure
	for domain, rcpts := range groupByDomain(m.Recipients) {
		t, p := q.deliverDomain(m.From, domain, rcpts, signed)
		temp = append(temp, t...)
		perm = append(perm, p...)
	}

	attempts := m.Attempts + 1

	// Permanent failures bounce now; recipients still temp-failing bounce
	// too once the retry budget is spent.
	bounced := perm
	var stillDeferred []rcptFailure
	if attempts >= maxAttempts {
		bounced = append(bounced, temp...)
	} else {
		stillDeferred = temp
	}
	if len(bounced) > 0 {
		q.bounce(m, raw, bounced)
	}

	if len(stillDeferred) == 0 {
		// Everything is resolved — delivered, or bounced.
		if err := q.store.CompleteOutbound(m); err != nil {
			log.Printf("queue: message %d: cleanup: %v", m.ID, err)
		}
		return
	}

	next := time.Now().Add(backoff(attempts))
	recipients := failureRecipients(stillDeferred)
	lastErr := stillDeferred[len(stillDeferred)-1].reason
	if err := q.store.DeferOutbound(m.ID, recipients, attempts, next, lastErr); err != nil {
		log.Printf("queue: message %d: defer: %v", m.ID, err)
		return
	}
	log.Printf("queue: message %d: %d recipient(s) deferred, attempt %d at %s",
		m.ID, len(recipients), attempts, next.Format(time.RFC3339))
}

// rcptFailure is one recipient that could not be delivered to, with the
// reason why.
type rcptFailure struct {
	recipient string
	reason    string
}

// deliverDomain delivers to every recipient at one domain over a single
// SMTP connection. It returns the recipients that temp-failed (worth a
// retry) and the ones that permanently failed; recipients in neither
// list were delivered.
func (q *Queue) deliverDomain(from, domain string, rcpts []string, raw []byte) (temp, perm []rcptFailure) {
	targets, err := q.resolve(domain)
	if err != nil || len(targets) == 0 {
		return failuresFor(rcpts, fmt.Sprintf("could not resolve %s: %v", domain, err)), nil
	}

	var conn net.Conn
	for _, target := range targets {
		conn, err = net.DialTimeout("tcp", target, dialTimeout)
		if err == nil {
			break
		}
	}
	if conn == nil {
		return failuresFor(rcpts, fmt.Sprintf("could not connect to %s: %v", domain, err)), nil
	}

	c := gosmtp.NewClient(conn)
	defer c.Close()

	if err := c.Hello(q.hostname); err != nil {
		return failuresFor(rcpts, fmt.Sprintf("EHLO to %s failed: %v", domain, err)), nil
	}
	if err := c.Mail(from, nil); err != nil {
		// A MAIL FROM rejection applies to the whole transaction.
		reason := fmt.Sprintf("MAIL FROM rejected by %s: %v", domain, err)
		if isPermanent(err) {
			return nil, failuresFor(rcpts, reason)
		}
		return failuresFor(rcpts, reason), nil
	}

	var accepted []string
	for _, rcpt := range rcpts {
		if err := c.Rcpt(rcpt, nil); err != nil {
			f := rcptFailure{recipient: rcpt, reason: err.Error()}
			if isPermanent(err) {
				perm = append(perm, f)
			} else {
				temp = append(temp, f)
			}
			continue
		}
		accepted = append(accepted, rcpt)
	}
	if len(accepted) == 0 {
		return temp, perm
	}

	if err := writeData(c, raw); err != nil {
		// The body was rejected after RCPT — applies to every accepted
		// recipient.
		reason := fmt.Sprintf("DATA rejected by %s: %v", domain, err)
		if isPermanent(err) {
			perm = append(perm, failuresFor(accepted, reason)...)
		} else {
			temp = append(temp, failuresFor(accepted, reason)...)
		}
		return temp, perm
	}
	_ = c.Quit()
	// `accepted` were delivered: in neither list.
	return temp, perm
}

// failuresFor pairs each recipient with a shared reason.
func failuresFor(rcpts []string, reason string) []rcptFailure {
	out := make([]rcptFailure, len(rcpts))
	for i, r := range rcpts {
		out[i] = rcptFailure{recipient: r, reason: reason}
	}
	return out
}

// failureRecipients pulls the recipient addresses out of a failure list.
func failureRecipients(fs []rcptFailure) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.recipient
	}
	return out
}

// writeData runs the SMTP DATA phase, writing the raw message.
func writeData(c *gosmtp.Client, raw []byte) error {
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// groupByDomain buckets recipient addresses by their lower-cased domain
// part. Malformed addresses are logged and dropped.
func groupByDomain(rcpts []string) map[string][]string {
	out := make(map[string][]string)
	for _, rcpt := range rcpts {
		at := strings.LastIndexByte(rcpt, '@')
		if at < 0 {
			log.Printf("queue: dropping malformed recipient %q", rcpt)
			continue
		}
		domain := strings.ToLower(rcpt[at+1:])
		out[domain] = append(out[domain], rcpt)
	}
	return out
}

// isPermanent reports whether an SMTP error is a permanent (5xx)
// rejection. Connection-level errors are treated as temporary.
func isPermanent(err error) bool {
	var smtpErr *gosmtp.SMTPError
	if errors.As(err, &smtpErr) {
		return smtpErr.Code >= 500 && smtpErr.Code < 600
	}
	return false
}

// backoff returns the delay before retry number `attempts`: baseBackoff
// doubled for each attempt, capped at maxBackoff.
func backoff(attempts int) time.Duration {
	d := baseBackoff
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

// lookupMX is the production resolver: a DNS MX lookup, falling back to
// the domain's own address records when it has no MX (RFC 5321 §5.1).
func lookupMX(domain string) ([]string, error) {
	mxs, err := net.LookupMX(domain)
	if err != nil || len(mxs) == 0 {
		// No usable MX record — try the domain itself on port 25.
		return []string{net.JoinHostPort(domain, "25")}, nil
	}
	targets := make([]string, 0, len(mxs))
	for _, mx := range mxs {
		host := strings.TrimSuffix(mx.Host, ".")
		targets = append(targets, net.JoinHostPort(host, "25"))
	}
	return targets, nil
}
