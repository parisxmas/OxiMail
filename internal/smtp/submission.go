package smtp

import (
	"crypto/tls"
	"io"
	"log"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/store"
)

// errAuthRequired is returned by the submission server's commands until
// the client has authenticated.
var errAuthRequired = &gosmtp.SMTPError{
	Code:         530,
	EnhancedCode: gosmtp.EnhancedCode{5, 7, 0},
	Message:      "Authentication required",
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
	srv := newServer(addr, hostname, &submissionBackend{store: st}, tlsConfig)
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
	store *store.Store
}

func (b *submissionBackend) NewSession(*gosmtp.Conn) (gosmtp.Session, error) {
	return &submissionSession{store: b.store}, nil
}

// submissionSession is the per-connection state for the submission
// server. go-smtp serializes the calls for one connection, so the
// fields need no locking.
type submissionSession struct {
	store *store.Store

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
// are checked against the account store.
func (s *submissionSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		// A PLAIN authorization identity, if given, must match the
		// authentication identity — OxiMail has no proxy-auth.
		if identity != "" && identity != username {
			return gosmtp.ErrAuthFailed
		}
		acc, err := s.store.Authenticate(username, password)
		if err != nil {
			return gosmtp.ErrAuthFailed
		}
		s.account = acc
		return nil
	}), nil
}

// Mail records the envelope sender. The client must have authenticated.
func (s *submissionSession) Mail(from string, _ *gosmtp.MailOptions) error {
	if s.account == nil {
		return errAuthRequired
	}
	// TODO: enforce that `from` is the account's own address or an alias
	// it owns; for now an authenticated user may set any envelope sender.
	s.from = from
	return nil
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

	subject, messageID, fromAddr := parseHeaders(raw)
	in := store.IncomingMessage{
		Raw:       raw,
		MessageID: messageID,
		Subject:   subject,
		FromAddr:  fromAddr,
	}

	var (
		remote     []string
		localCount int
		failed     int
	)
	for _, rcpt := range s.rcpts {
		accts, err := s.store.ResolveRecipient(rcpt)
		if err != nil {
			log.Printf("submission: resolve %q: %v", rcpt, err)
			return tempError("Temporary local problem, please try again later")
		}
		if len(accts) == 0 {
			remote = append(remote, rcpt) // not a local mailbox — relay it
			continue
		}
		for _, id := range accts {
			if _, err := s.store.Deliver(id, in); err != nil {
				log.Printf("submission: deliver to account %d failed: %v", id, err)
				failed++
				continue
			}
			localCount++
		}
	}

	if len(remote) > 0 {
		if _, err := s.store.Enqueue(s.from, remote, raw); err != nil {
			log.Printf("submission: enqueue for %v failed: %v", remote, err)
			return tempError("Could not queue message for delivery, please try again later")
		}
	}
	if failed > 0 {
		return tempError("Temporary delivery failure, please try again later")
	}

	log.Printf("submission: accepted from %s (%d bytes) — %d local, %d queued for relay",
		s.account.Address, len(raw), localCount, len(remote))
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
