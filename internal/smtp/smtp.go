// Package smtp is the inbound SMTP (MX) server: it accepts mail on
// port 25, runs the spam pipeline, resolves recipients against the
// store, and files accepted messages into the right mailboxes.
//
// Submission (authenticated outbound on port 587) is a near-twin of this
// server and will share most of its machinery; it is added in a later
// phase.
//
// Implementation will be built on emersion/go-smtp.
package smtp

import (
	"context"
	"log"

	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/store"
)

// Server is the inbound SMTP listener.
type Server struct {
	addr  string
	store *store.Store
	spam  *spam.Pipeline
}

// New builds the inbound SMTP server bound to `addr`.
func New(addr string, st *store.Store, sp *spam.Pipeline) *Server {
	return &Server{addr: addr, store: st, spam: sp}
}

// Start accepts connections until `ctx` is cancelled, then returns.
//
// TODO: implement on go-smtp — the listener, the per-connection session
// (HELO/MAIL/RCPT/DATA), the spam-pipeline call, and delivery into the
// store.
func (s *Server) Start(ctx context.Context) error {
	log.Printf("smtp: listening on %s (stub — not yet implemented)", s.addr)
	<-ctx.Done()
	return nil
}

// Stop shuts the listener down. Safe to call after Start has returned.
func (s *Server) Stop() error {
	log.Print("smtp: stopped")
	return nil
}
