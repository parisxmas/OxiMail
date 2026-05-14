// Package imap is the IMAP server: it serves mailbox access on port 143,
// reading message metadata and flags from the store and message bodies
// from the OxiDB blob store.
//
// State model: the source of truth is OxiDB. On SELECT the server loads
// a snapshot of the mailbox's message metadata; message bodies are read
// from the blob store lazily, only when a FETCH asks for them. Mutations
// a session makes itself (STORE, EXPUNGE, APPEND) are tracked and
// reported correctly within that session; changes from *other*
// connections or from SMTP delivery are not seen until the mailbox is
// re-selected.
//
// TODO: real-time cross-connection updates (IDLE seeing freshly
// delivered mail) need either polling OxiDB or an OxiMem notification
// channel.
// TODO: STARTTLS / IMAPS, and SASL AUTHENTICATE — only LOGIN today.
// TODO: SEARCH, COPY, and mailbox DELETE / RENAME are not implemented.
//
// Built on emersion/go-imap/v2.
package imap

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/parisxmas/OxiMail/internal/store"
)

// mailboxDelim is the IMAP hierarchy separator OxiMail uses.
const mailboxDelim = '/'

// Server is the IMAP listener.
type Server struct {
	addr string
	srv  *imapserver.Server
	stop sync.Once
}

// New builds the IMAP server bound to `addr`.
func New(addr string, st *store.Store) *Server {
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{store: st}, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIdle:      {},
			imap.CapUnselect:  {},
			imap.CapUIDPlus:   {},
		},
		// No TLS is configured yet, so LOGIN has to be allowed in the
		// clear. TODO: ship STARTTLS / IMAPS and drop InsecureAuth.
		InsecureAuth: true,
	})
	return &Server{addr: addr, srv: srv}
}

// Start listens and serves until `ctx` is cancelled, then shuts the
// server down.
func (s *Server) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.ListenAndServe(s.addr) }()

	log.Printf("imap: listening on %s", s.addr)
	select {
	case <-ctx.Done():
		return s.Stop()
	case err := <-errc:
		// A Close from another goroutine surfaces here as a closed-conn
		// error; that is an orderly shutdown, not a failure.
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

// Stop shuts the listener down. Safe to call more than once and after
// Start has returned.
func (s *Server) Stop() error {
	s.stop.Do(func() {
		_ = s.srv.Close()
		log.Print("imap: stopped")
	})
	return nil
}
