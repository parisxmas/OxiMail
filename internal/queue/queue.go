// Package queue is the outbound delivery queue: a background worker that
// scans the outbound_queue collection for due messages, attempts SMTP
// delivery to each recipient's MX, and reschedules temporary failures
// with exponential backoff.
//
// Queue state lives in OxiDB, so delivery survives a restart; the
// message bodies it sends are read from the blob store.
//
// A permanent failure — or a recipient still failing when the message
// reaches maxQueueAge — produces a bounce (a DSN) back to the original
// sender.
//
// TLS: every dial attempts STARTTLS first and falls back to cleartext
// only if the remote refuses it. When the recipient domain publishes
// an MTA-STS policy (RFC 8461) in enforce mode, the dial additionally
// validates the certificate against the system trust store and
// requires the MX hostname to match the policy's mx patterns — a
// failure moves on to the next MX, and never downgrades to cleartext.
package queue

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/mtasts"
	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/srs"
	"github.com/parisxmas/OxiMail/internal/store"
)

const (
	// pollInterval is how often the worker scans for due deliveries.
	pollInterval = 30 * time.Second
	// batchSize caps how many due messages one scan processes.
	batchSize = 50
	// dueCountLimit caps the scan behind the due-messages gauge. It is
	// far above any queue this server should ever hold.
	dueCountLimit = 100000
	// maxQueueAge is how long a message keeps being retried before the
	// recipients still failing are bounced. Five days is the usual MTA
	// default (RFC 5321 §4.5.4.1 asks for at least four).
	maxQueueAge = 5 * 24 * time.Hour
	// maxAttemptsUndated stands in for maxQueueAge on a queue row whose
	// created_at cannot be read: roughly the attempts five days takes.
	maxAttemptsUndated = 26
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
	mtasts   *mtasts.Cache
	now      func() time.Time // swapped out in tests
	rootCAs  *x509.CertPool   // nil means the system trust store; set by tests
}

// New builds the outbound queue worker. `hostname` is announced in EHLO
// when connecting to remote mail servers.
func New(st *store.Store, hostname string) *Queue {
	return &Queue{
		store:    st,
		hostname: hostname,
		resolve:  lookupMX,
		mtasts:   mtasts.NewCache(),
		now:      time.Now,
	}
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
	q.refreshDue()
	defer q.refreshDue()
	lastRefresh := time.Now()
	for i := range msgs {
		if ctx.Err() != nil {
			return
		}
		q.deliver(&msgs[i])
		// A batch can take many minutes against slow remotes; keep the
		// gauge moving through it.
		if time.Since(lastRefresh) >= pollInterval {
			q.refreshDue()
			lastRefresh = time.Now()
		}
	}
}

// refreshDue sets the due-messages gauge from a real count, not from
// the batch being processed. The store has no count query, so this
// lists the rows; they carry queue metadata only, never bodies.
func (q *Queue) refreshDue() {
	due, err := q.store.ListDueOutbound(dueCountLimit)
	if err != nil {
		log.Printf("queue: count due messages: %v", err)
		return
	}
	observability.QueueDue.Set(float64(len(due)))
}

// deliver attempts one queued message and settles the outcome. A body
// that cannot be read can never be delivered, so every recipient
// fails permanently.
func (q *Queue) deliver(m *store.OutboundMessage) {
	raw, err := q.store.FetchOutboundBody(m)
	if err != nil {
		log.Printf("queue: message %d: body unreadable (%v) — bouncing", m.ID, err)
		perm := failuresWithStatus(m.Recipients, "the message could not be read from the mail queue", "5.3.0")
		q.settle(m, nil, nil, perm)
		return
	}
	// DKIM-sign before delivery — this is the point mail leaves OxiMail
	// for another domain. signMessage is a no-op for domains without a
	// configured key.
	signed := raw
	if q.maySign(m, raw) {
		signed = signMessage(q.store, raw)
	}

	var temp, perm []rcptFailure
	for domain, rcpts := range groupByDomain(m.Recipients) {
		t, p := q.deliverDomain(m, domain, rcpts, signed)
		temp = append(temp, t...)
		perm = append(perm, p...)
	}
	q.settle(m, raw, temp, perm)
}

// maySign reports whether this queue row is mail OxiMail may sign for
// its From domain. Mail relayed for someone else is not: an inbound
// message reaches the queue through alias forwarding (an SRS sender)
// or as a forwarded bounce (a null sender) with whatever From its
// remote author chose, and signing that would vouch for a forgery of
// any hosted domain. The one null-sender message that is ours is a
// bounce from this server's MAILER-DAEMON.
func (q *Queue) maySign(m *store.OutboundMessage, raw []byte) bool {
	if srs.Is(m.From) {
		return false
	}
	if m.From != "" {
		return true
	}
	addrs, err := spam.FromAddresses(raw)
	return err == nil && len(addrs) == 1 &&
		strings.EqualFold(addrs[0], "MAILER-DAEMON@"+q.hostname)
}

// settle records the outcome of one attempt. Recipients that
// permanently failed — and, once the message is too old to retry,
// those still temp-failing — are bounced back to the sender; recipients
// still worth a retry are deferred with backoff; a message with nothing
// left to retry is removed from the queue. `original` is nil when the
// body could not be read.
func (q *Queue) settle(m *store.OutboundMessage, original []byte, temp, perm []rcptFailure) {
	now := q.now()
	attempts := m.Attempts + 1
	deadline, dated := queueDeadline(m)

	// Permanent failures bounce now; recipients still temp-failing bounce
	// too once the retry budget is spent.
	bounced := perm
	var stillDeferred []rcptFailure
	if retriesSpent(now, deadline, dated, attempts) {
		for _, f := range temp {
			f.status = expiredStatus(f.status)
			bounced = append(bounced, f)
		}
	} else {
		stillDeferred = temp
	}
	if len(bounced) > 0 {
		if err := q.bounce(m, original, bounced); err != nil {
			// Dropping the row now would lose the message and tell
			// nobody; keep it and go through this again later.
			log.Printf("queue: message %d: could not return a bounce to %s: %v", m.ID, m.From, err)
			stillDeferred = append(stillDeferred, bounced...)
		} else {
			observability.QueueDeliveries.WithLabelValues("bounced").Add(float64(len(bounced)))
		}
	}
	if len(stillDeferred) > 0 {
		observability.QueueDeliveries.WithLabelValues("deferred").Add(float64(len(stillDeferred)))
	}

	if len(stillDeferred) == 0 {
		// Everything is resolved — delivered, or bounced.
		if err := q.store.CompleteOutbound(m); err != nil {
			log.Printf("queue: message %d: cleanup: %v", m.ID, err)
		}
		return
	}

	next := now.Add(backoff(attempts))
	if dated && now.Before(deadline) && next.After(deadline) {
		next = deadline // one last try right at the deadline
	}
	recipients := failureRecipients(stillDeferred)
	lastErr := stillDeferred[len(stillDeferred)-1].reason
	if err := q.store.DeferOutbound(m.ID, recipients, attempts, next, lastErr); err != nil {
		log.Printf("queue: message %d: defer: %v", m.ID, err)
		return
	}
	log.Printf("queue: message %d: %d recipient(s) deferred, attempt %d, next at %s",
		m.ID, len(recipients), attempts, next.Format(time.RFC3339))
}

// queueDeadline returns when a message stops being retried. dated is
// false if the row's created_at cannot be read.
func queueDeadline(m *store.OutboundMessage) (deadline time.Time, dated bool) {
	created, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return created.Add(maxQueueAge), true
}

// retriesSpent reports whether a message that just made its attempt
// number `attempts` has run out of retries.
func retriesSpent(now, deadline time.Time, dated bool, attempts int) bool {
	if !dated {
		return attempts >= maxAttemptsUndated
	}
	return !now.Before(deadline)
}

// rcptFailure is one recipient that could not be delivered to, with the
// reason why.
type rcptFailure struct {
	recipient string
	reason    string
	status    string // RFC 3463 enhanced status, "" if nobody gave one
}

// deliverDomain delivers to every recipient at one domain over a single
// SMTP connection. It returns the recipients that temp-failed (worth a
// retry) and the ones that permanently failed; recipients in neither
// list were delivered.
func (q *Queue) deliverDomain(m *store.OutboundMessage, domain string, rcpts []string, raw []byte) (temp, perm []rcptFailure) {
	targets, err := q.resolve(domain)
	if err != nil || len(targets) == 0 {
		reason := fmt.Sprintf("could not resolve %s: %v", domain, err)
		switch {
		case errors.Is(err, errNullMX):
			return nil, failuresWithStatus(rcpts, reason, "5.1.10")
		case errors.Is(err, errNoSuchDomain):
			return nil, failuresWithStatus(rcpts, reason, "5.1.2")
		}
		return failuresFor(rcpts, reason, err), nil
	}

	// MTA-STS lookup happens once per recipient domain per batch; the
	// Cache memoises so a chatty domain does not re-fetch on every
	// outbound message.
	stsCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	policy, stsErr := q.mtasts.Get(stsCtx, domain)
	cancel()
	if stsErr != nil && !errors.Is(stsErr, mtasts.ErrNoPolicy) {
		log.Printf("queue: MTA-STS lookup for %s: %v (treating as no policy)", domain, stsErr)
		policy = nil
	}

	c, dialErr := q.dialMX(targets, domain, policy)
	if c == nil {
		reason := fmt.Sprintf("could not connect to %s: %v", domain, dialErr)
		// An MTA-STS enforce-mode failure is permanent: the policy
		// is meant to refuse insecure delivery, not retry it.
		if errors.Is(dialErr, errMTASTSReject) {
			return nil, failuresWithStatus(rcpts, reason, "5.7.10")
		}
		return failuresFor(rcpts, reason, dialErr), nil
	}
	defer c.Close()

	if err := c.Mail(m.From, nil); err != nil {
		// A MAIL FROM rejection applies to the whole transaction.
		reason := fmt.Sprintf("MAIL FROM rejected by %s: %v", domain, err)
		if isPermanent(err) {
			return nil, failuresFor(rcpts, reason, err)
		}
		return failuresFor(rcpts, reason, err), nil
	}

	var accepted []string
	for _, rcpt := range rcpts {
		if err := c.Rcpt(rcpt, nil); err != nil {
			f := rcptFailure{recipient: rcpt, reason: err.Error(), status: enhancedStatus(err)}
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
			perm = append(perm, failuresFor(accepted, reason, err)...)
		} else {
			temp = append(temp, failuresFor(accepted, reason, err)...)
		}
		return temp, perm
	}
	_ = c.Quit()
	// `accepted` were delivered: in neither list.
	observability.QueueDeliveries.WithLabelValues("delivered").Add(float64(len(accepted)))
	log.Printf("queue: message %d: delivered to %d recipient(s) at %s", m.ID, len(accepted), domain)
	return temp, perm
}

// failuresFor pairs each recipient with a shared reason, and with the
// enhanced status carried by err, if any.
func failuresFor(rcpts []string, reason string, err error) []rcptFailure {
	return failuresWithStatus(rcpts, reason, enhancedStatus(err))
}

// failuresWithStatus pairs each recipient with a shared reason and
// enhanced status.
func failuresWithStatus(rcpts []string, reason, status string) []rcptFailure {
	out := make([]rcptFailure, len(rcpts))
	for i, r := range rcpts {
		out[i] = rcptFailure{recipient: r, reason: reason, status: status}
	}
	return out
}

// enhancedStatus returns the RFC 3463 status of an SMTP error, or "" if
// err is not an SMTP reply or the remote did not give one.
func enhancedStatus(err error) string {
	var smtpErr *gosmtp.SMTPError
	if !errors.As(err, &smtpErr) {
		return ""
	}
	e := smtpErr.EnhancedCode
	if e[0] < 2 || e[0] > 5 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", e[0], e[1], e[2])
}

// expiredStatus is the final status of a recipient that was still
// temp-failing when the message ran out of time: what the remote said,
// promoted to a permanent class, or 5.4.7 (delivery time expired) if it
// said nothing, as when no connection could be made.
func expiredStatus(status string) string {
	if status == "" {
		return "5.4.7"
	}
	return "5" + status[1:]
}

// failureRecipients pulls the recipient addresses out of a failure list.
func failureRecipients(fs []rcptFailure) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.recipient
	}
	return out
}

// errMTASTSReject is the sentinel used when an MTA-STS enforce-mode
// policy refuses every MX. Callers turn it into a permanent failure
// rather than a retryable one — the policy is designed to refuse
// delivery in this case, not delay it. An MX that could not be reached
// at all has not been refused: its failure stays temporary.
var errMTASTSReject = errors.New("queue: MTA-STS policy refused every MX")

// dialMX tries each target in order until one has greeted us and been
// said hello to, attempting opportunistic STARTTLS first and falling
// back to cleartext if the remote does not speak it — UNLESS an MTA-STS
// policy is in enforce mode for the destination, in which case the
// certificate is verified strictly against the system trust store, the
// MX hostname must match one of the policy's patterns, and a TLS or
// policy failure moves on to the next target (no cleartext fallback).
// Testing-mode policies log violations but still deliver.
//
// Returns (nil, lastErr) if no target can be used.
func (q *Queue) dialMX(targets []string, domain string, policy *mtasts.Policy) (*gosmtp.Client, error) {
	strict := policy != nil && policy.Mode == mtasts.ModeEnforce

	var lastErr error
	refused := 0 // targets the enforce policy ruled out, as opposed to unreachable ones
	for _, target := range targets {
		mxHost := hostOnly(target)

		// MTA-STS policy gates the MX hostname before we even dial.
		// Testing mode logs a hint but proceeds.
		if policy != nil && !policy.Allows(mxHost) {
			lastErr = fmt.Errorf("MX %s not allowed by MTA-STS policy for %s", mxHost, domain)
			if strict {
				refused++
				continue
			}
			log.Printf("queue: MTA-STS testing-mode warning: %v", lastErr)
		}

		tlsConfig := &tls.Config{ServerName: mxHost, RootCAs: q.rootCAs}
		if !strict {
			tlsConfig.InsecureSkipVerify = true
		}

		conn, err := net.DialTimeout("tcp", target, dialTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		c, err := startTLS(conn, tlsConfig, q.hostname)
		if err == nil {
			return c, nil
		}
		if strict {
			// Enforce mode forbids the cleartext fallback.
			lastErr = fmt.Errorf("STARTTLS to %s failed under MTA-STS enforce: %w", mxHost, err)
			if !isTransient(err) {
				refused++
			}
			continue
		}
		// Cleartext fallback (only outside enforce mode).
		conn, err = net.DialTimeout("tcp", target, dialTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		c = gosmtp.NewClient(conn)
		if err := c.Hello(q.hostname); err != nil {
			_ = c.Close()
			lastErr = fmt.Errorf("EHLO to %s failed: %w", mxHost, err)
			continue
		}
		return c, nil
	}
	if strict && refused == len(targets) {
		return nil, fmt.Errorf("%w: %v", errMTASTSReject, lastErr)
	}
	return nil, lastErr
}

// startTLS upgrades a fresh connection and says hello over it. The
// library does STARTTLS but leaves the TLS handshake to the next
// command, so the Hello here is what surfaces a certificate failure,
// while there is still a next MX to try. It closes conn on error.
func startTLS(conn net.Conn, tlsConfig *tls.Config, hostname string) (*gosmtp.Client, error) {
	c, err := gosmtp.NewClientStartTLS(&heloConn{Conn: conn, hostname: hostname}, tlsConfig)
	if err != nil {
		return nil, err
	}
	if err := c.Hello(hostname); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// heloConn corrects the one thing NewClientStartTLS gives no control
// over: it sends its plaintext greeting as "localhost". Everything after
// STARTTLS is encrypted by the time it gets here, so it cannot match.
type heloConn struct {
	net.Conn
	hostname string
}

func (h *heloConn) Write(b []byte) (int, error) {
	for _, verb := range []string{"EHLO", "HELO"} {
		if string(b) == verb+" localhost\r\n" {
			if _, err := h.Conn.Write([]byte(verb + " " + h.hostname + "\r\n")); err != nil {
				return 0, err
			}
			return len(b), nil
		}
	}
	return h.Conn.Write(b)
}

// isTransient reports whether a failure setting up TLS says something
// about the network or the moment, as opposed to the remote's TLS
// support or certificate: a dropped connection, a timeout, or a 4xx.
func isTransient(err error) bool {
	var smtpErr *gosmtp.SMTPError
	if errors.As(err, &smtpErr) {
		return !isPermanent(err)
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, os.ErrDeadlineExceeded)
}

// hostOnly trims the ":port" off a "host:port" target.
func hostOnly(target string) string {
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
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

// Errors from the resolver that mean a retry cannot help.
var (
	// errNullMX is a domain publishing RFC 7505 null MX: it accepts no
	// mail at all.
	errNullMX = errors.New("domain publishes a null MX and accepts no mail")
	// errNoSuchDomain is a domain with neither MX nor address records.
	errNoSuchDomain = errors.New("domain has no MX or address records")
)

// lookupMX is the production resolver.
func lookupMX(domain string) ([]string, error) {
	return resolveMX(domain, net.LookupMX, net.LookupHost)
}

// resolveMX maps a domain to its SMTP endpoints. Only an authoritative
// "no MX records" falls back to the domain's own address records (RFC
// 5321 §5.1); any other lookup failure, such as SERVFAIL or a timeout,
// is returned so the message is deferred instead of going to a host
// the domain never named. A null MX is errNullMX.
func resolveMX(domain string, lookupMXs func(string) ([]*net.MX, error), lookupHost func(string) ([]string, error)) ([]string, error) {
	mxs, err := lookupMXs(domain)
	// LookupMX can return usable records alongside an error, when it
	// dropped a malformed one; those records are still good.
	if len(mxs) == 0 && err != nil {
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			return nil, err
		}
	}
	if len(mxs) == 0 {
		if _, err := lookupHost(domain); err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				return nil, errNoSuchDomain
			}
			return nil, err
		}
		return []string{net.JoinHostPort(domain, "25")}, nil
	}
	targets := make([]string, 0, len(mxs))
	for _, mx := range mxs {
		host := strings.TrimSuffix(mx.Host, ".")
		if host == "" {
			continue // "." — only meaningful alone, handled below
		}
		targets = append(targets, net.JoinHostPort(host, "25"))
	}
	if len(targets) == 0 {
		return nil, errNullMX
	}
	return targets, nil
}
