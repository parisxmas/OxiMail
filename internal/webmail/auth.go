package webmail

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"strings"

	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/store"
)

// Cookie and header names for browser-based session auth.
const (
	sessionCookie = "oximail_session"
	csrfCookie    = "oximail_csrf"
	csrfHeader    = "X-CSRF-Token"
)

// loginRequest / loginResponse are the JSON bodies for POST /api/login.
type loginRequest struct {
	Address  string `json:"address"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token   string `json:"token"`
	Address string `json:"address"`
}

// handleLogin authenticates an account, issues a bearer token, and
// also sets HttpOnly session + readable CSRF cookies so a browser SPA
// can rely on them instead of localStorage.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limiter.Blocked(ip) {
		// The 429 status is honest about *why* we are refusing; the
		// per-IP key still leaks zero account information.
		observability.Logins.WithLabelValues("webmail", "fail").Inc()
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Normalise the address before the per-account limiter so case
	// variants don't bypass the gate. We check this BEFORE the store
	// lookup — an IP-rotating attacker who's already burned through
	// the account's budget gets stalled here even if their next IP
	// is fresh.
	acctKey := strings.ToLower(strings.TrimSpace(req.Address))
	if acctKey != "" && s.accountLimiter.Blocked(acctKey) {
		observability.Logins.WithLabelValues("webmail", "fail").Inc()
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	acc, err := s.store.Authenticate(req.Address, req.Password)
	if err != nil {
		// Authenticate collapses every failure to ErrAuthFailed, so we
		// cannot — and should not — tell the client which part was wrong.
		s.limiter.RecordFailure(ip)
		if acctKey != "" {
			s.accountLimiter.RecordFailure(acctKey)
			// Surface a one-line warning the moment an account hits
			// its threshold; helps operators notice targeted attacks
			// in the logs without parsing Prometheus counters.
			if s.accountLimiter.Blocked(acctKey) {
				log.Printf("webmail: rate-limit triggered for account %q from %s", acctKey, ip)
			}
		}
		observability.Logins.WithLabelValues("webmail", "fail").Inc()
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := s.sessions.create(acc.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a session")
		return
	}
	csrf, err := newCSRFToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a session")
		return
	}
	s.setSessionCookies(w, token, csrf)
	observability.Logins.WithLabelValues("webmail", "ok").Inc()
	writeJSON(w, http.StatusOK, loginResponse{Token: token, Address: acc.Address})
}

// handleLogout revokes the caller's session token and clears the
// browser cookies. The handler is wrapped with auth, so it implicitly
// requires a valid session (and, for cookie-based callers, a valid
// CSRF token).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ *store.Account) {
	if token := bearerToken(r); token != "" {
		s.sessions.delete(token)
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.delete(c.Value)
	}
	clearCookie(w, sessionCookie, s.secure)
	clearCookie(w, csrfCookie, s.secure)
	w.WriteHeader(http.StatusNoContent)
}

// authedHandler is an HTTP handler that has been handed the
// authenticated account.
type authedHandler func(w http.ResponseWriter, r *http.Request, acc *store.Account)

// auth wraps a handler, resolving the request's session credential
// (Authorization: Bearer for programmatic clients, or the
// oximail_session cookie for browsers) and — for cookie-authenticated
// mutating requests — enforcing a double-submit CSRF token.
func (s *Server) auth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, viaCookie := resolveToken(r)
		accountID, ok := s.sessions.lookup(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		if viaCookie && isMutating(r.Method) && !validCSRF(r) {
			writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		acc, err := s.store.GetAccountByID(accountID)
		if err != nil {
			// The session outlived the account it points at.
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		h(w, r, acc)
	}
}

// resolveToken extracts the session token from the request, preferring
// the Authorization header (programmatic clients) over the session
// cookie (browser). viaCookie reports which path was used — the CSRF
// check applies only to cookie auth.
func resolveToken(r *http.Request) (token string, viaCookie bool) {
	if t := bearerToken(r); t != "" {
		return t, false
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value, true
	}
	return "", false
}

// validCSRF checks the double-submit cookie pattern: a request is
// considered CSRF-safe when the X-CSRF-Token header is present and
// constant-time equal to the oximail_csrf cookie. Same-origin browser
// JavaScript can read its own cookies; an attacker-controlled origin
// (subject to SameSite=Strict) cannot.
func validCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	header := r.Header.Get(csrfHeader)
	if header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) == 1
}

// isMutating reports whether method may change server state — the
// methods that need CSRF protection. RFC 7231 §4.2.1 puts GET, HEAD,
// and OPTIONS in the "safe" bucket; everything else is potentially
// mutating.
func isMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// setSessionCookies emits the session and CSRF cookies. The session
// cookie is HttpOnly so XSS cannot steal it; the CSRF cookie is
// readable from JavaScript on purpose, because the SPA echoes it in
// the X-CSRF-Token header.
func (s *Server) setSessionCookies(w http.ResponseWriter, sessionToken, csrf string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionToken,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		Secure:   s.secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    csrf,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		Secure:   s.secure,
		HttpOnly: false, // JS reads this and echoes it in X-CSRF-Token
		SameSite: http.SameSiteStrictMode,
	})
}

// clearCookie writes a Set-Cookie header that asks the browser to
// drop the named cookie.
func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   secure,
		HttpOnly: name == sessionCookie, // mirror what setSessionCookies set
		SameSite: http.SameSiteStrictMode,
	})
}

// newCSRFToken returns a random hex-encoded token. The value is opaque
// to clients; it just has to be unguessable.
func newCSRFToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// request header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
