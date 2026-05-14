// Package webmail is OxiMail's HTTP+JSON API for browser and mobile
// clients. It is backed directly by the store — it does not go through
// IMAP — and authenticates against the same account credentials.
//
// This is the read-side scaffold: login, mailbox listing, message
// listing, and fetching one parsed message. Sending, search, flag
// changes, move / delete, and attachment download are follow-ups.
//
// TODO: HttpOnly session cookies + CSRF protection for browser
// frontends — today auth is a bearer token, which is simplest for an
// API and for tests.
package webmail

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/parisxmas/OxiMail/internal/store"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Server is the webmail HTTP listener.
type Server struct {
	addr     string
	srv      *http.Server
	store    *store.Store
	sessions *sessionStore
	stop     sync.Once
}

// New builds the webmail server bound to `addr`. A non-nil tlsConfig
// makes it serve HTTPS.
func New(addr string, st *store.Store, tlsConfig *tls.Config) *Server {
	s := &Server{
		addr:     addr,
		store:    st,
		sessions: newSessionStore(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/mailboxes", s.auth(s.handleMailboxes))
	mux.HandleFunc("GET /api/mailboxes/{mailbox}/messages", s.auth(s.handleListMessages))
	mux.HandleFunc("GET /api/messages/{id}", s.auth(s.handleGetMessage))
	mux.HandleFunc("POST /api/messages", s.auth(s.handleSend))
	mux.HandleFunc("PATCH /api/messages/{id}/flags", s.auth(s.handleFlags))
	mux.HandleFunc("POST /api/messages/{id}/move", s.auth(s.handleMove))
	mux.HandleFunc("DELETE /api/messages/{id}", s.auth(s.handleDelete))

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		TLSConfig:         tlsConfig,
	}
	return s
}

// Start serves until `ctx` is cancelled, then shuts the server down.
func (s *Server) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		if s.srv.TLSConfig != nil {
			// The certificate comes from TLSConfig, so the file
			// arguments are empty.
			errc <- s.srv.ListenAndServeTLS("", "")
		} else {
			errc <- s.srv.ListenAndServe()
		}
	}()
	go s.sessions.sweepLoop(ctx)

	scheme := "http"
	if s.srv.TLSConfig != nil {
		scheme = "https"
	}
	log.Printf("webmail: listening on %s (%s)", s.addr, scheme)
	select {
	case <-ctx.Done():
		return s.Stop()
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Stop shuts the server down gracefully. Safe to call more than once
// and after Start has returned.
func (s *Server) Stop() error {
	s.stop.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
		log.Print("webmail: stopped")
	})
	return nil
}

// -----------------------------------------------------------------------
// JSON helpers
// -----------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("webmail: encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
