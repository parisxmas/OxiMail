package webmail

import (
	"errors"
	"net/http"

	"github.com/parisxmas/OxiMail/internal/store"
)

// vacationRequest is the body of PUT /api/vacation.
type vacationRequest struct {
	Enabled      bool   `json:"enabled"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	SuppressDays int    `json:"suppress_days,omitempty"`
}

// vacationResponse is what GET / PUT return.
type vacationResponse struct {
	Enabled      bool   `json:"enabled"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	SuppressDays int    `json:"suppress_days,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// handleGetVacation returns the account's vacation rule, or an
// all-zero "disabled" response when none is set.
func (s *Server) handleGetVacation(w http.ResponseWriter, _ *http.Request, acc *store.Account) {
	v, err := s.store.GetVacation(acc.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, vacationResponse{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read vacation rule")
		return
	}
	writeJSON(w, http.StatusOK, vacationResponse{
		Enabled: v.Enabled, Subject: v.Subject, Body: v.Body,
		SuppressDays: v.SuppressDays, UpdatedAt: v.UpdatedAt,
	})
}

// handlePutVacation upserts the rule.
func (s *Server) handlePutVacation(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	var req vacationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Enabled && req.Body == "" {
		writeError(w, http.StatusBadRequest, "vacation body must not be empty when enabled")
		return
	}
	v, err := s.store.SetVacation(acc.ID, req.Enabled, req.Subject, req.Body, req.SuppressDays)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save vacation rule")
		return
	}
	writeJSON(w, http.StatusOK, vacationResponse{
		Enabled: v.Enabled, Subject: v.Subject, Body: v.Body,
		SuppressDays: v.SuppressDays, UpdatedAt: v.UpdatedAt,
	})
}

// handleDeleteVacation turns the auto-responder off and removes the
// rule.
func (s *Server) handleDeleteVacation(w http.ResponseWriter, _ *http.Request, acc *store.Account) {
	if err := s.store.DeleteVacation(acc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete vacation rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
