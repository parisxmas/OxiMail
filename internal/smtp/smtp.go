// Package smtp provides OxiMail's two SMTP listeners — the inbound MX
// (New) and the mail submission server (NewSubmission) — both built on
// emersion/go-smtp and sharing the lifecycle in this file.
//
// The inbound MX accepts mail on port 25, runs the spam pipeline on each
// message, resolves recipients against the store, and files accepted
// messages into the right mailboxes. It does not relay — a recipient
// that is not a local mailbox (or an alias to one) is rejected. The
// submission server (submission.go) requires SMTP AUTH and relays.
//
// When a TLS configuration is supplied, STARTTLS is advertised on the
// plaintext listeners; NewSubmissionTLS additionally serves implicit
// TLS (SMTPS).
package smtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/mail"
	"strings"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/srs"
	"github.com/parisxmas/OxiMail/internal/store"
)

// Server tuning. These are conservative defaults; they move into
// internal/config once they need to be operator-tunable.
const (
	readTimeout     = 5 * time.Minute
	writeTimeout    = 5 * time.Minute
	shutdownTimeout = 10 * time.Second
	maxRecipients   = 100
	// maxMessageBytes caps an accepted message. Bodies up to this size go
	// through OxiDB's native PutObject; raising it well past the OxiDB
	// wire-frame limit will require routing bodies via the S3 blob path.
	maxMessageBytes = 25 << 20 // 25 MiB
)

// Server is an SMTP listener — either the inbound MX (New) or the
// submission server (NewSubmission / NewSubmissionTLS). They share this
// lifecycle; they differ only in their go-smtp backend and in whether
// the listener is plaintext (with optional STARTTLS) or implicit TLS.
type Server struct {
	name        string // "smtp", "submission", "submission-tls" — for log lines
	addr        string
	srv         *gosmtp.Server
	implicitTLS bool // serve TLS from the first byte, rather than STARTTLS-on-plaintext
}

// newServer builds a go-smtp server with OxiMail's shared tuning, for
// the given backend. A non-nil tlsConfig enables STARTTLS (and, for the
// implicit-TLS variants, the encrypted listener).
func newServer(addr, hostname string, be gosmtp.Backend, tlsConfig *tls.Config) *gosmtp.Server {
	srv := gosmtp.NewServer(be)
	srv.Addr = addr
	srv.Domain = hostname
	srv.TLSConfig = tlsConfig
	srv.ReadTimeout = readTimeout
	srv.WriteTimeout = writeTimeout
	srv.MaxMessageBytes = maxMessageBytes
	srv.MaxRecipients = maxRecipients
	srv.ErrorLog = log.Default()
	return srv
}

// ForwarderConfig carries the operator-tunable knobs for SRS-based
// alias forwarding. A zero value disables forwarding: aliases that
// resolve to remote addresses are not relayed and the MX behaves as
// before (local-only delivery).
type ForwarderConfig struct {
	// SRSSecret signs and verifies the rewritten sender. It must be at
	// least 16 bytes long and stable across restarts — losing it
	// invalidates every outstanding bounce address.
	SRSSecret []byte
	// SRSMaxAge is how long an SRS-encoded bounce address stays
	// valid. Zero means "no age check"; default 21 days is sensible.
	SRSMaxAge time.Duration
	// ForwarderDomain is the domain that hosts the rewritten sender —
	// usually the MX's own hostname. Bounces come back here.
	ForwarderDomain string
}

// New builds the inbound SMTP (MX) server bound to `addr`, announcing
// `hostname` in its greeting. A non-nil tlsConfig advertises STARTTLS.
// `fwd` enables SRS-based alias forwarding to remote addresses; pass
// the zero value to disable it.
func New(addr, hostname string, st *store.Store, sp *spam.Pipeline, tlsConfig *tls.Config, fwd ForwarderConfig) *Server {
	if fwd.ForwarderDomain == "" {
		fwd.ForwarderDomain = hostname
	}
	return &Server{
		name: "smtp",
		addr: addr,
		srv:  newServer(addr, hostname, &backend{store: st, spam: sp, fwd: fwd}, tlsConfig),
	}
}

// Start listens and serves until `ctx` is cancelled, then shuts the
// server down gracefully.
func (s *Server) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		var err error
		if s.implicitTLS {
			err = s.srv.ListenAndServeTLS()
		} else {
			err = s.srv.ListenAndServe()
		}
		if errors.Is(err, gosmtp.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	log.Printf("%s: listening on %s", s.name, s.addr)
	select {
	case <-ctx.Done():
		return s.Stop()
	case err := <-errc:
		return err
	}
}

// Stop shuts the listener down gracefully. Safe to call more than once
// and after Start has returned.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil && !errors.Is(err, gosmtp.ErrServerClosed) {
		return err
	}
	log.Printf("%s: stopped", s.name)
	return nil
}

// -----------------------------------------------------------------------
// Backend / Session
// -----------------------------------------------------------------------

// backend builds one session per incoming connection.
type backend struct {
	store *store.Store
	spam  *spam.Pipeline
	fwd   ForwarderConfig
}

func (b *backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &session{backend: b, remoteIP: remoteIPOf(c.Conn())}, nil
}

// remoteIPOf returns the IP part of a connection's remote address, or
// "" if the address is missing or unparseable. Used by both SMTP
// backends to key the rate limiter and the spam pipeline.
func remoteIPOf(c net.Conn) string {
	if c == nil {
		return ""
	}
	addr := c.RemoteAddr()
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return host
}

// session is the per-connection state machine. go-smtp serializes the
// calls for one connection, so the envelope fields need no locking.
type session struct {
	backend  *backend
	remoteIP string

	// Envelope state for the message currently being received; cleared
	// by Reset (which go-smtp calls after each delivered message).
	from       string
	rcptAddrs  []string        // envelope recipients, as given, for the spam pipeline
	rcptAccts  map[uint64]bool // resolved local account IDs, deduplicated across recipients
	rcptRemote []string        // alias forwarding: remote addresses to relay to
}

// Mail records the envelope sender (MAIL FROM). An empty sender is the
// null reverse-path used by bounces and is accepted as such.
func (s *session) Mail(from string, _ *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

// Rcpt resolves an envelope recipient (RCPT TO). Three paths:
//
//  1. The address is an SRS-rewritten bounce target: decode it,
//     dispatch as if the original sender had been written into RCPT.
//     This is how alias-forwarding bounces find their way home.
//  2. The address is a local mailbox or alias: file copies for the
//     local accounts at DATA, and (for aliases with remote
//     destinations) queue forwards with an SRS-rewritten sender.
//  3. Neither: reject with 550. The inbound MX does not relay open
//     mail.
func (s *session) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	if decoded, ok := s.decodeIfSRS(to); ok {
		to = decoded
	}
	dests, err := s.backend.store.ResolveDestinations(to)
	if err != nil {
		log.Printf("smtp: resolve recipient %q: %v", to, err)
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message:      "Temporary local problem, please try again later",
		}
	}
	if dests.Empty() {
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 1, 1},
			Message:      "No such user here",
		}
	}
	// An alias points off-server but the operator has not enabled SRS:
	// refuse rather than emit mail without a verifiable envelope sender
	// (it would fail SPF / DMARC at the next hop). A purely-local
	// alias is still allowed.
	if len(dests.RemoteAddrs) > 0 && !s.forwardingEnabled() {
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
			Message:      "Alias forwarding to remote addresses is not configured",
		}
	}
	if s.rcptAccts == nil {
		s.rcptAccts = make(map[uint64]bool)
	}
	for _, id := range dests.LocalAccounts {
		s.rcptAccts[id] = true
	}
	for _, addr := range dests.RemoteAddrs {
		if !containsString(s.rcptRemote, addr) {
			s.rcptRemote = append(s.rcptRemote, addr)
		}
	}
	s.rcptAddrs = append(s.rcptAddrs, to)
	return nil
}

// decodeIfSRS turns an SRS-rewritten RCPT TO back into the original
// sender. Returns the original address and true on a successful decode;
// returns (to, false) if the address is not an SRS address or SRS is
// not configured, and logs / returns (to, false) on a tampered or
// expired SRS address (the caller falls back to the usual rejection
// path).
func (s *session) decodeIfSRS(to string) (string, bool) {
	if !s.forwardingEnabled() {
		return to, false
	}
	fwd := s.backend.fwd
	if !srs.Is(to) {
		return to, false
	}
	orig, err := srs.Decode(fwd.SRSSecret, to, fwd.SRSMaxAge)
	if err != nil {
		log.Printf("smtp: decode SRS %q: %v", to, err)
		return to, false
	}
	return orig, true
}

// forwardingEnabled reports whether SRS-based alias forwarding is
// configured on this server.
func (s *session) forwardingEnabled() bool {
	return len(s.backend.fwd.SRSSecret) >= 16 && s.backend.fwd.ForwarderDomain != ""
}

// containsString reports whether s contains v.
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Data receives the message body, runs the spam pipeline, and on an
// Accept verdict files the message into every resolved recipient's
// INBOX.
func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message:      "Error reading message data",
		}
	}

	subject, messageID, fromAddr := parseHeaders(raw)

	verdict, err := s.backend.spam.Check(s.remoteIP, s.from, s.rcptAddrs, raw)
	if err != nil {
		log.Printf("smtp: spam pipeline error: %v", err)
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 7, 0},
			Message:      "Temporary local problem, please try again later",
		}
	}
	observability.SMTPMessages.WithLabelValues(verdictLabel(verdict)).Inc()
	switch verdict {
	case spam.Greylist:
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 7, 1},
			Message:      "Greylisted, please try again later",
		}
	case spam.Reject:
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
			Message:      "Message rejected by local policy",
		}
	}

	in := store.IncomingMessage{
		Raw:       raw,
		MessageID: messageID,
		Subject:   subject,
		FromAddr:  fromAddr,
	}
	var failed int
	for acctID := range s.rcptAccts {
		if _, err := s.backend.store.Deliver(acctID, in); err != nil {
			log.Printf("smtp: deliver to account %d failed: %v", acctID, err)
			failed++
		}
	}
	if failed > 0 {
		// One DATA command covers every recipient, so we can only return
		// a single status. Asking the sender to retry re-delivers to the
		// recipients that already succeeded.
		// TODO: switch local delivery to LMTP for per-recipient status.
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message:      "Temporary delivery failure, please try again later",
		}
	}

	// Alias forwarding: relay to the remote alias destinations with an
	// SRS-rewritten envelope sender so SPF / DMARC line up at the next
	// hop. An empty envelope sender (a bounce) is not rewritten — it
	// stays empty so the receiving MX knows not to bounce it again.
	if len(s.rcptRemote) > 0 {
		sender := s.from
		if sender != "" {
			rewritten, err := srs.Encode(s.backend.fwd.SRSSecret, sender, s.backend.fwd.ForwarderDomain)
			if err != nil {
				log.Printf("smtp: SRS rewrite of %q: %v", sender, err)
				return &gosmtp.SMTPError{
					Code:         451,
					EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
					Message:      "Could not rewrite envelope sender for forwarding",
				}
			}
			sender = rewritten
		}
		if _, err := s.backend.store.Enqueue(sender, s.rcptRemote, raw); err != nil {
			log.Printf("smtp: enqueue forward to %v: %v", s.rcptRemote, err)
			return &gosmtp.SMTPError{
				Code:         451,
				EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
				Message:      "Temporary forwarding failure, please try again later",
			}
		}
	}

	log.Printf("smtp: accepted message from <%s> (%d bytes) — %d local, %d forwarded",
		s.from, len(raw), len(s.rcptAccts), len(s.rcptRemote))
	return nil
}

// Reset discards the in-progress message's envelope state.
func (s *session) Reset() {
	s.from = ""
	s.rcptAddrs = nil
	s.rcptAccts = nil
	s.rcptRemote = nil
}

// Logout releases the session. There is nothing connection-scoped to
// free yet.
func (s *session) Logout() error { return nil }

// verdictLabel maps a spam.Verdict to its Prometheus label value.
func verdictLabel(v spam.Verdict) string {
	switch v {
	case spam.Greylist:
		return "greylist"
	case spam.Reject:
		return "reject"
	default:
		return "accept"
	}
}

// parseHeaders pulls the metadata fields the store records from the raw
// message. A malformed header block is not fatal — the raw bytes and the
// SMTP envelope are authoritative — so any parse failure yields empty
// strings.
func parseHeaders(raw []byte) (subject, messageID, fromAddr string) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", "", ""
	}
	subject = msg.Header.Get("Subject")
	messageID = strings.Trim(msg.Header.Get("Message-Id"), "<>")
	if addr, err := mail.ParseAddress(msg.Header.Get("From")); err == nil {
		fromAddr = addr.Address
	}
	return subject, messageID, fromAddr
}
