// Package webmail is OxiMail's HTTP+JSON API for browser and mobile
// clients. It is backed directly by the store — it does not go through
// IMAP — and authenticates against the same account credentials.
//
// Endpoints cover login, logout, mailbox listing, message listing
// (with an optional ?q= substring search), fetching one parsed
// message, downloading an attachment, sending (text or multipart/
// alternative HTML), flag changes, move, and delete.
//
// Auth carries two transports:
//
//   - "Authorization: Bearer <token>" for programmatic clients (the
//     integration tests, a future CLI / mobile client). No CSRF check.
//   - HttpOnly oximail_session cookie for browser SPAs, paired with a
//     readable oximail_csrf cookie. Mutating requests over cookie auth
//     must echo the CSRF value in X-CSRF-Token (the double-submit
//     cookie pattern); SameSite=Strict on both cookies blocks the
//     classic cross-origin replay vector.
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
	"strconv"
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
	mtasts   MTASTSPolicy
	// secure reports whether this server is serving HTTPS. It controls
	// the Secure flag on session and CSRF cookies — a Secure cookie
	// would never travel over a development plaintext listener and the
	// browser would silently drop the login.
	secure bool
	stop   sync.Once
}

// MTASTSPolicy is the operator-published MTA-STS policy (RFC 8461)
// served at /.well-known/mta-sts.txt. A zero value disables publishing.
type MTASTSPolicy struct {
	Mode   string        // "enforce", "testing", "none"
	MX     []string      // hostname patterns
	MaxAge time.Duration // policy lifetime
}

// New builds the webmail server bound to `addr`. A non-nil tlsConfig
// makes it serve HTTPS. If staticDir is non-empty and exists, the built
// frontend SPA is served from it; otherwise only the API is served.
// mtasts, when non-zero, enables the /.well-known/mta-sts.txt handler.
func New(addr, staticDir string, st *store.Store, tlsConfig *tls.Config, mtasts MTASTSPolicy) *Server {
	s := &Server{
		addr:     addr,
		store:    st,
		sessions: newSessionStore(),
		limiter:  ratelimit.NewDefault(),
		secure:   tlsConfig != nil,
		mtasts:   mtasts,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/mailboxes", s.auth(s.handleMailboxes))
	mux.HandleFunc("GET /api/mailboxes/{mailbox}/messages", s.auth(s.handleListMessages))
	mux.HandleFunc("GET /api/messages/{id}", s.auth(s.handleGetMessage))
	mux.HandleFunc("GET /api/messages/{id}/attachments/{n}", s.auth(s.handleAttachment))
	mux.HandleFunc("POST /api/messages", s.auth(s.handleSend))
	mux.HandleFunc("POST /api/drafts", s.auth(s.handleSaveDraft))
	mux.HandleFunc("GET /api/vacation", s.auth(s.handleGetVacation))
	mux.HandleFunc("PUT /api/vacation", s.auth(s.handlePutVacation))
	mux.HandleFunc("DELETE /api/vacation", s.auth(s.handleDeleteVacation))
	mux.HandleFunc("GET /api/sieve", s.auth(s.handleGetSieve))
	mux.HandleFunc("PUT /api/sieve", s.auth(s.handlePutSieve))
	mux.HandleFunc("DELETE /api/sieve", s.auth(s.handleDeleteSieve))
	mux.HandleFunc("PATCH /api/messages/{id}/flags", s.auth(s.handleFlags))
	mux.HandleFunc("POST /api/messages/{id}/move", s.auth(s.handleMove))
	mux.HandleFunc("DELETE /api/messages/{id}", s.auth(s.handleDelete))
	// An unknown /api/ path is a JSON 404, not the SPA shell.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})

	// MTA-STS policy publication (RFC 8461). The path is fixed; the
	// content is gated by the operator-provided MTASTSPolicy. Per the
	// spec it should be served on https://mta-sts.<domain>/, which
	// operators arrange via DNS + (typically) the same TLS cert.
	if s.mtasts.Mode != "" {
		mux.HandleFunc("GET /.well-known/mta-sts.txt", s.handleMTASTSPolicy)
	}

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

// handleMTASTSPolicy renders the operator's MTA-STS policy as the
// plain-text policy file RFC 8461 expects.
func (s *Server) handleMTASTSPolicy(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=3600")
	maxAge := int(s.mtasts.MaxAge.Seconds())
	if maxAge <= 0 {
		maxAge = 86400
	}
	body := "version: STSv1\nmode: " + s.mtasts.Mode + "\n"
	for _, mx := range s.mtasts.MX {
		body += "mx: " + mx + "\n"
	}
	body += "max_age: " + strconv.Itoa(maxAge) + "\n"
	_, _ = w.Write([]byte(body))
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
