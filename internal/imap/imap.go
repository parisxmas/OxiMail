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
// TODO: SASL AUTHENTICATE — only LOGIN today.
// TODO: SEARCH, COPY, and mailbox DELETE / RENAME are not implemented.
//
// When a TLS configuration is supplied, STARTTLS is advertised on the
// plaintext listener; NewTLS additionally serves implicit TLS (IMAPS).
//
// Built on emersion/go-imap/v2.
package imap

import (
	"context"
	"crypto/tls"
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

// Server is the IMAP listener — plaintext (New, with optional STARTTLS)
// or implicit TLS (NewTLS).
type Server struct {
	name        string // "imap" or "imap-tls", for log lines
	addr        string
	srv         *imapserver.Server
	implicitTLS bool
	stop        sync.Once
}

// New builds the IMAP server bound to `addr`. A non-nil tlsConfig
// advertises STARTTLS and requires LOGIN to run over an encrypted
// connection; with no TLS configured at all, cleartext LOGIN is
// permitted so the server still works for local development.
func New(addr string, st *store.Store, tlsConfig *tls.Config) *Server {
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
		TLSConfig:    tlsConfig,
		InsecureAuth: tlsConfig == nil,
	})
	return &Server{name: "imap", addr: addr, srv: srv}
}

// NewTLS builds the implicit-TLS IMAP server (IMAPS, conventionally port
// 993): the connection is encrypted from the first byte. tlsConfig is
// required.
func NewTLS(addr string, st *store.Store, tlsConfig *tls.Config) *Server {
	s := New(addr, st, tlsConfig)
	s.name = "imap-tls"
	s.implicitTLS = true
	return s
}

// Start listens and serves until `ctx` is cancelled, then shuts the
// server down.
func (s *Server) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		if s.implicitTLS {
			errc <- s.srv.ListenAndServeTLS(s.addr)
		} else {
			errc <- s.srv.ListenAndServe(s.addr)
		}
	}()

	log.Printf("%s: listening on %s", s.name, s.addr)
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
		log.Printf("%s: stopped", s.name)
	})
	return nil
}
