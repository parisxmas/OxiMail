package webmail

import (
	"net/http"
	"strings"

	"github.com/parisxmas/OxiMail/internal/store"
)

// MaxDisplayNameLength caps the display name an account can set for
// its outbound From header. 80 fits inside RFC 5322's recommended
// 78-character header line plus the cost of `"" <addr@domain>`. We
// could go higher, but receivers display roughly this much anyway and
// the cap keeps obvious "fill the header" abuse off the API.
const MaxDisplayNameLength = 80

// profileResponse is the body of GET /api/account/profile. Address is
// returned for convenience even though the SPA already knows it from
// the login response — that way the profile page is self-contained.
type profileResponse struct {
	Address     string `json:"address"`
	DisplayName string `json:"display_name"`
}

// profileRequest is the body of PATCH /api/account/profile. Only the
// display name is mutable — the address is set at account-creation
// time and is the login identity, not a profile attribute.
type profileRequest struct {
	DisplayName string `json:"display_name"`
}

// handleGetProfile returns the account's editable profile fields.
func (s *Server) handleGetProfile(w http.ResponseWriter, _ *http.Request, acc *store.Account) {
	writeJSON(w, http.StatusOK, profileResponse{
		Address:     acc.Address,
		DisplayName: acc.DisplayName,
	})
}

// handleUpdateProfile sets the account's display name. An empty value
// clears it (the next outbound message reverts to the bare address).
// The value is normalised (trimmed, single-line, length-capped) here
// so the store stays a dumb key/value sink.
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	var req profileRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, err := normaliseDisplayName(req.DisplayName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetAccountDisplayName(acc.ID, name); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save profile")
		return
	}
	writeJSON(w, http.StatusOK, profileResponse{
		Address:     acc.Address,
		DisplayName: name,
	})
}

// normaliseDisplayName trims leading/trailing whitespace, rejects
// CR/LF/NUL (those would let a caller smuggle extra headers into the
// outbound From line), and enforces the length cap. Empty after
// trimming is fine — it clears the display name.
func normaliseDisplayName(s string) (string, error) {
	// Check for header-injection chars BEFORE TrimSpace — \r and \n
	// are themselves whitespace, so trimming first would silently
	// accept a bare "\n" (an obviously malicious payload) as the
	// empty-string clear-display-name case.
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", errInvalidDisplayName
	}
	s = strings.TrimSpace(s)
	if len(s) > MaxDisplayNameLength {
		return "", errDisplayNameTooLong
	}
	return s, nil
}

type displayNameError string

func (e displayNameError) Error() string { return string(e) }

const (
	errInvalidDisplayName = displayNameError("display name must not contain control characters")
	errDisplayNameTooLong = displayNameError("display name is too long")
)
