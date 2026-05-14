// Package imap is the IMAP server: it serves mailbox access on port 143,
// reading message metadata and flags from the store and message bodies
// from the OxiDB blob store.
//
// Note: IMAP SEARCH over body text is backed by OxiDB's full-text index,
// which is eventually consistent — a just-delivered message may not be
// searchable for a few seconds.
//
// Implementation will be built on emersion/go-imap.
package imap

import (
	"context"
	"log"

	"github.com/parisxmas/OxiMail/internal/store"
)

// Server is the IMAP listener.
type Server struct {
	addr  string
	store *store.Store
}

// New builds the IMAP server bound to `addr`.
func New(addr string, st *store.Store) *Server {
	return &Server{addr: addr, store: st}
}

// Start accepts connections until `ctx` is cancelled, then returns.
//
// TODO: implement on go-imap — the listener, per-connection session
// (LOGIN/SELECT/FETCH/STORE/SEARCH), and the store/blob-store reads.
func (s *Server) Start(ctx context.Context) error {
	log.Printf("imap: listening on %s (stub — not yet implemented)", s.addr)
	<-ctx.Done()
	return nil
}

// Stop shuts the listener down. Safe to call after Start has returned.
func (s *Server) Stop() error {
	log.Print("imap: stopped")
	return nil
}
