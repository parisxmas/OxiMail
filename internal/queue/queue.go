// Package queue is the outbound delivery queue: a background worker that
// scans the outbound_queue collection for due messages, attempts SMTP
// delivery to each recipient's MX, and reschedules temporary failures
// with exponential backoff.
//
// Queue state lives in OxiDB, so delivery survives a restart; the
// message bodies it sends are read from the blob store.
//
// TODO: bounce messages for permanent failures and exhausted retries —
// abandoned recipients are currently just logged.
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

// deliver attempts one queued message and updates the queue: a message
// whose every recipient is resolved (delivered or permanently
// abandoned) is removed, one with recipients still temp-failing is
// deferred with backoff.
func (q *Queue) deliver(m *store.OutboundMessage) {
	raw, err := q.store.FetchOutboundBody(m)
	if err != nil {
		log.Printf("queue: message %d: body unreadable (%v) — abandoning", m.ID, err)
		if err := q.store.CompleteOutbound(m); err != nil {
			log.Printf("queue: message %d: cleanup: %v", m.ID, err)
		}
		return
	}

	var (
		deferred []string
		lastErr  string
	)
	for domain, rcpts := range groupByDomain(m.Recipients) {
		temp, perm, errStr := q.deliverDomain(m.From, domain, rcpts, raw)
		deferred = append(deferred, temp...)
		if errStr != "" {
			lastErr = errStr
		}
		for _, pf := range perm {
			log.Printf("queue: message %d: %q permanently rejected — dropping (TODO: bounce)", m.ID, pf)
		}
	}

	if len(deferred) == 0 {
		if err := q.store.CompleteOutbound(m); err != nil {
			log.Printf("queue: message %d: cleanup: %v", m.ID, err)
		}
		return
	}

	attempts := m.Attempts + 1
	if attempts >= maxAttempts {
		log.Printf("queue: message %d: %d recipient(s) undelivered after %d attempts — giving up (TODO: bounce)",
			m.ID, len(deferred), attempts)
		if err := q.store.CompleteOutbound(m); err != nil {
			log.Printf("queue: message %d: cleanup: %v", m.ID, err)
		}
		return
	}

	next := time.Now().Add(backoff(attempts))
	if err := q.store.DeferOutbound(m.ID, deferred, attempts, next, lastErr); err != nil {
		log.Printf("queue: message %d: defer: %v", m.ID, err)
		return
	}
	log.Printf("queue: message %d: %d recipient(s) deferred, attempt %d at %s",
		m.ID, len(deferred), attempts, next.Format(time.RFC3339))
}

// deliverDomain delivers to every recipient at one domain over a single
// SMTP connection. It returns the recipients that temp-failed (retry
// later), the ones that permanently failed (drop), and a representative
// error string; recipients in neither list were delivered.
func (q *Queue) deliverDomain(from, domain string, rcpts []string, raw []byte) (temp, perm []string, lastErr string) {
	targets, err := q.resolve(domain)
	if err != nil || len(targets) == 0 {
		return rcpts, nil, fmt.Sprintf("resolve %s: %v", domain, err)
	}

	var conn net.Conn
	for _, target := range targets {
		conn, err = net.DialTimeout("tcp", target, dialTimeout)
		if err == nil {
			break
		}
	}
	if conn == nil {
		return rcpts, nil, fmt.Sprintf("dial %s: %v", domain, err)
	}

	c := gosmtp.NewClient(conn)
	defer c.Close()

	if err := c.Hello(q.hostname); err != nil {
		return rcpts, nil, fmt.Sprintf("EHLO %s: %v", domain, err)
	}
	if err := c.Mail(from, nil); err != nil {
		// A MAIL FROM rejection applies to the whole transaction.
		if isPermanent(err) {
			return nil, rcpts, err.Error()
		}
		return rcpts, nil, err.Error()
	}

	var accepted []string
	for _, rcpt := range rcpts {
		if err := c.Rcpt(rcpt, nil); err != nil {
			lastErr = err.Error()
			if isPermanent(err) {
				perm = append(perm, rcpt)
			} else {
				temp = append(temp, rcpt)
			}
			continue
		}
		accepted = append(accepted, rcpt)
	}
	if len(accepted) == 0 {
		return temp, perm, lastErr
	}

	if err := writeData(c, raw); err != nil {
		// The body was rejected after RCPT — applies to every accepted
		// recipient.
		if isPermanent(err) {
			perm = append(perm, accepted...)
		} else {
			temp = append(temp, accepted...)
		}
		return temp, perm, err.Error()
	}
	_ = c.Quit()
	// `accepted` were delivered: in neither the temp nor the perm list.
	return temp, perm, lastErr
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
