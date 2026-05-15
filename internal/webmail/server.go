// Package webmail is OxiMail's HTTP+JSON API for browser and mobile
// clients. It is backed directly by the store — it does not go through
// IMAP — and authenticates against the same account credentials.
//
// Endpoints cover login, mailbox listing, message listing (with an
// optional ?q= substring search), fetching one parsed message,
// downloading an attachment, sending (text or multipart/alternative
// HTML), flag changes, move, and delete.
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
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/parisxmas/OxiMail/internal/ratelimit"
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
	limiter  *ratelimit.Limiter
	stop     sync.Once
}

// New builds the webmail server bound to `addr`. A non-nil tlsConfig
// makes it serve HTTPS. If staticDir is non-empty and exists, the built
// frontend SPA is served from it; otherwise only the API is served.
func New(addr, staticDir string, st *store.Store, tlsConfig *tls.Config) *Server {
	s := &Server{
		addr:     addr,
		store:    st,
		sessions: newSessionStore(),
		limiter:  ratelimit.NewDefault(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/mailboxes", s.auth(s.handleMailboxes))
	mux.HandleFunc("GET /api/mailboxes/{mailbox}/messages", s.auth(s.handleListMessages))
	mux.HandleFunc("GET /api/messages/{id}", s.auth(s.handleGetMessage))
	mux.HandleFunc("GET /api/messages/{id}/attachments/{n}", s.auth(s.handleAttachment))
	mux.HandleFunc("POST /api/messages", s.auth(s.handleSend))
	mux.HandleFunc("PATCH /api/messages/{id}/flags", s.auth(s.handleFlags))
	mux.HandleFunc("POST /api/messages/{id}/move", s.auth(s.handleMove))
	mux.HandleFunc("DELETE /api/messages/{id}", s.auth(s.handleDelete))
	// An unknown /api/ path is a JSON 404, not the SPA shell.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})

	// Serve the frontend SPA, if it has been built.
	if staticDir != "" {
		if info, err := os.Stat(staticDir); err == nil && info.IsDir() {
			mux.Handle("/", spaHandler(staticDir))
			log.Printf("webmail: serving frontend from %s", staticDir)
		} else {
			log.Printf("webmail: frontend dir %q not found — serving API only", staticDir)
		}
	}

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

// clientIP returns the IP portion of r.RemoteAddr. X-Forwarded-For is
// intentionally not consulted — trusting it without a configured proxy
// list would let any client spoof the rate-limit key.
//
// TODO: an OXIMAIL_TRUSTED_PROXIES env var would let an operator opt
// in to X-Forwarded-For when running behind a reverse proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}
