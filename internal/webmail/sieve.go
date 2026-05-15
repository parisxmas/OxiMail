package webmail

import (
	"errors"
	"net/http"

	"github.com/parisxmas/OxiMail/internal/sieve"
	"github.com/parisxmas/OxiMail/internal/store"
)

// sieveRequest is the body of PUT /api/sieve.
type sieveRequest struct {
	Source string `json:"source"`
}

// sieveResponse is what GET / PUT return.
type sieveResponse struct {
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// handleGetSieve returns the account's stored script, or an empty
// payload when none is set.
func (s *Server) handleGetSieve(w http.ResponseWriter, _ *http.Request, acc *store.Account) {
	sc, err := s.store.GetSieveScript(acc.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, sieveResponse{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read sieve script")
		return
	}
	writeJSON(w, http.StatusOK, sieveResponse{Source: sc.Source, UpdatedAt: sc.UpdatedAt})
}

// handlePutSieve validates the script (Parse must succeed) and then
// upserts it.
func (s *Server) handlePutSieve(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	var req sieveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := sieve.Parse(req.Source); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sc, err := s.store.SetSieveScript(acc.ID, req.Source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save sieve script")
		return
	}
	writeJSON(w, http.StatusOK, sieveResponse{Source: sc.Source, UpdatedAt: sc.UpdatedAt})
}

// handleDeleteSieve removes the script.
func (s *Server) handleDeleteSieve(w http.ResponseWriter, _ *http.Request, acc *store.Account) {
	if err := s.store.DeleteSieveScript(acc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete sieve script")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
