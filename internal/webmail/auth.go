package webmail

import (
	"net/http"
	"strings"

	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/store"
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

// handleLogin authenticates an account and issues a bearer token.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	acc, err := s.store.Authenticate(req.Address, req.Password)
	if err != nil {
		// Authenticate collapses every failure to ErrAuthFailed, so we
		// cannot — and should not — tell the client which part was wrong.
		observability.Logins.WithLabelValues("webmail", "fail").Inc()
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, err := s.sessions.create(acc.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a session")
		return
	}
	observability.Logins.WithLabelValues("webmail", "ok").Inc()
	writeJSON(w, http.StatusOK, loginResponse{Token: token, Address: acc.Address})
}

// authedHandler is an HTTP handler that has been handed the
// authenticated account.
type authedHandler func(w http.ResponseWriter, r *http.Request, acc *store.Account)

// auth wraps a handler, requiring a valid bearer token and resolving it
// to the account it belongs to.
func (s *Server) auth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := s.sessions.lookup(bearerToken(r))
		if !ok {
			writeError(w, http.StatusUnauthorized, "not authenticated")
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
