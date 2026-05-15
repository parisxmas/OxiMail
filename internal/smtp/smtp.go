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

// New builds the inbound SMTP (MX) server bound to `addr`, announcing
// `hostname` in its greeting. A non-nil tlsConfig advertises STARTTLS.
func New(addr, hostname string, st *store.Store, sp *spam.Pipeline, tlsConfig *tls.Config) *Server {
	return &Server{
		name: "smtp",
		addr: addr,
		srv:  newServer(addr, hostname, &backend{store: st, spam: sp}, tlsConfig),
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
}

func (b *backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	remoteIP := ""
	if addr := c.Conn().RemoteAddr(); addr != nil {
		if host, _, err := net.SplitHostPort(addr.String()); err == nil {
			remoteIP = host
		}
	}
	return &session{backend: b, remoteIP: remoteIP}, nil
}

// session is the per-connection state machine. go-smtp serializes the
// calls for one connection, so the envelope fields need no locking.
type session struct {
	backend  *backend
	remoteIP string

	// Envelope state for the message currently being received; cleared
	// by Reset (which go-smtp calls after each delivered message).
	from      string
	rcptAddrs []string        // envelope recipients, as given, for the spam pipeline
	rcptAccts map[uint64]bool // resolved local account IDs, deduplicated across recipients
}

// Mail records the envelope sender (MAIL FROM). An empty sender is the
// null reverse-path used by bounces and is accepted as such.
func (s *session) Mail(from string, _ *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

// Rcpt resolves an envelope recipient (RCPT TO) to local accounts. The
// inbound MX does not relay: an address that is not a local mailbox (or
// an alias to one) is rejected.
func (s *session) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	accts, err := s.backend.store.ResolveRecipient(to)
	if err != nil {
		log.Printf("smtp: resolve recipient %q: %v", to, err)
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message:      "Temporary local problem, please try again later",
		}
	}
	if len(accts) == 0 {
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 1, 1},
			Message:      "No such user here",
		}
	}
	if s.rcptAccts == nil {
		s.rcptAccts = make(map[uint64]bool)
	}
	for _, id := range accts {
		s.rcptAccts[id] = true
	}
	s.rcptAddrs = append(s.rcptAddrs, to)
	return nil
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

	log.Printf("smtp: accepted message from <%s> (%d bytes) for %d recipient(s)",
		s.from, len(raw), len(s.rcptAccts))
	return nil
}

// Reset discards the in-progress message's envelope state.
func (s *session) Reset() {
	s.from = ""
	s.rcptAddrs = nil
	s.rcptAccts = nil
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
