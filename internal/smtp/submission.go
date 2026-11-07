package smtp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/ratelimit"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/store"
)

// errAuthRequired is returned by the submission server's commands until
// the client has authenticated.
var errAuthRequired = &gosmtp.SMTPError{
	Code:         530,
	EnhancedCode: gosmtp.EnhancedCode{5, 7, 0},
	Message:      "Authentication required",
}

// errSenderRefused is returned when a client tries to send as an
// address that is not its own. The outbound queue DKIM-signs by the
// From domain, so this check is what stops one account from signing
// mail as any other user of any hosted domain.
var errSenderRefused = &gosmtp.SMTPError{
	Code:         550,
	EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
	Message:      "Sender address does not belong to the authenticated account",
}

// NewSubmission builds the mail submission server (RFC 6409) bound to
// `addr` — conventionally port 587. Unlike the inbound MX, every client
// must authenticate; and it relays — recipients that are local mailboxes
// are filed directly, the rest are handed to the outbound queue.
//
// A non-nil tlsConfig advertises STARTTLS and requires AUTH to run over
// an encrypted connection. With no TLS configured at all, cleartext
// AUTH is permitted so the server still works for local development.
func NewSubmission(addr, hostname string, st *store.Store, tlsConfig *tls.Config) *Server {
	be := &submissionBackend{hostname: hostname, store: st, limiter: ratelimit.NewDefault()}
	srv := newServer(addr, hostname, be, tlsConfig)
	srv.AllowInsecureAuth = tlsConfig == nil
	return &Server{name: "submission", addr: addr, srv: srv}
}

// NewSubmissionTLS builds the implicit-TLS submission server (SMTPS,
// conventionally port 465): the connection is encrypted from the first
// byte, with no STARTTLS upgrade step. tlsConfig is required.
func NewSubmissionTLS(addr, hostname string, st *store.Store, tlsConfig *tls.Config) *Server {
	s := NewSubmission(addr, hostname, st, tlsConfig)
	s.name = "submission-tls"
	s.implicitTLS = true
	return s
}

type submissionBackend struct {
	hostname string
	store    *store.Store
	limiter  *ratelimit.Limiter
}

func (b *submissionBackend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &submissionSession{
		hostname: b.hostname,
		store:    b.store,
		limiter:  b.limiter,
		conn:     c,
		remoteIP: remoteIPOf(c.Conn()),
	}, nil
}

// submissionSession is the per-connection state for the submission
// server. go-smtp serializes the calls for one connection, so the
// fields need no locking.
type submissionSession struct {
	hostname string
	store    *store.Store
	limiter  *ratelimit.Limiter
	conn     *gosmtp.Conn
	remoteIP string

	account *store.Account // set by Auth; nil until the client authenticates
	from    string
	rcpts   []string
}

var _ gosmtp.AuthSession = (*submissionSession)(nil)

// AuthMechanisms advertises the SASL mechanisms this server accepts.
func (s *submissionSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

// Auth handles SASL authentication. Only PLAIN is offered; credentials
// are checked against the account store, and the per-IP rate limiter
// guards against brute force.
func (s *submissionSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		// Block before consulting the store — and before paying the
		// bcrypt cost — when this IP has burned through its budget.
		if s.limiter.Blocked(s.remoteIP) {
			observability.Logins.WithLabelValues("submission", "fail").Inc()
			return gosmtp.ErrAuthFailed
		}
		// A PLAIN authorization identity, if given, must match the
		// authentication identity — OxiMail has no proxy-auth.
		if identity != "" && identity != username {
			s.limiter.RecordFailure(s.remoteIP)
			observability.Logins.WithLabelValues("submission", "fail").Inc()
			return gosmtp.ErrAuthFailed
		}
		acc, err := s.store.Authenticate(username, password)
		if err != nil {
			s.limiter.RecordFailure(s.remoteIP)
			observability.Logins.WithLabelValues("submission", "fail").Inc()
			return gosmtp.ErrAuthFailed
		}
		observability.Logins.WithLabelValues("submission", "ok").Inc()
		s.account = acc
		return nil
	}), nil
}

// Mail records the envelope sender. The client must have authenticated,
// and the sender must be its own. A null sender is refused: it means a
// bounce, which a mail client has no business submitting, and nothing
// could ever bounce back from it.
func (s *submissionSession) Mail(from string, _ *gosmtp.MailOptions) error {
	if s.account == nil {
		return errAuthRequired
	}
	if from == "" {
		return errSenderRefused
	}
	if err := s.checkSender(from); err != nil {
		return err
	}
	s.from = from
	return nil
}

// checkSender returns nil if address is the authenticated account's own
// or an alias that resolves to it.
func (s *submissionSession) checkSender(address string) error {
	if strings.EqualFold(address, s.account.Address) {
		return nil
	}
	dests, err := s.store.ResolveDestinations(address)
	if err != nil {
		log.Printf("submission: resolve sender %q: %v", address, err)
		return tempError("Temporary local problem, please try again later")
	}
	for _, id := range dests.LocalAccounts {
		if id == s.account.ID {
			return nil
		}
	}
	log.Printf("submission: %s refused as sender for account %s", address, s.account.Address)
	return errSenderRefused
}

// Rcpt records an envelope recipient. The submission server relays, so
// any address is accepted — local vs remote is sorted out in Data.
func (s *submissionSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	if s.account == nil {
		return errAuthRequired
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

// Data delivers the message: recipients that resolve to a local mailbox
// are filed directly, and the rest are handed to the outbound queue.
func (s *submissionSession) Data(r io.Reader) error {
	if s.account == nil {
		return errAuthRequired
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return tempError("Error reading message data")
	}

	// MAIL FROM was checked at Mail; what recipients see, and what DKIM
	// signs for, is the From header. Every address in it must pass, and
	// a message without a readable one cannot be vouched for at all.
	addrs, err := spam.FromAddresses(raw)
	if err != nil {
		log.Printf("submission: unusable From header from %s: %v", s.account.Address, err)
		return errSenderRefused
	}
	for _, addr := range addrs {
		if err := s.checkSender(addr); err != nil {
			return err
		}
	}

	var stamped bytes.Buffer
	stamped.WriteString(receivedHeader(s.hostname, s.conn, s.remoteIP, true, time.Now()))
	stamped.Write(raw)

	subject, messageID, fromAddr := parseHeaders(raw)
	routed, err := s.store.Route(s.from, s.rcpts, store.IncomingMessage{
		Raw:       stamped.Bytes(),
		MessageID: messageID,
		Subject:   subject,
		FromAddr:  fromAddr,
	})
	if err != nil {
		log.Printf("submission: routing message from %s: %v", s.account.Address, err)
		if errors.Is(err, store.ErrQuotaExceeded) {
			return mailboxFull()
		}
		return tempError("Temporary delivery failure, please try again later")
	}

	log.Printf("submission: accepted from %s (%d bytes) — %d local, %d queued for relay",
		s.account.Address, len(raw), routed.LocalCount, routed.QueuedCount)
	return nil
}

// Reset discards the in-progress message's envelope state. The
// authenticated account is connection-scoped and is kept.
func (s *submissionSession) Reset() {
	s.from = ""
	s.rcpts = nil
}

func (s *submissionSession) Logout() error { return nil }

// tempError builds a 451 temporary-failure SMTP error.
func tempError(msg string) error {
	return &gosmtp.SMTPError{
		Code:         451,
		EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
		Message:      msg,
	}
}
