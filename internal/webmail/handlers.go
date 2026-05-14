package webmail

import (
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/parisxmas/OxiMail/internal/store"
)

// mailboxSummary is one mailbox in the GET /api/mailboxes response.
type mailboxSummary struct {
	Name       string `json:"name"`
	Subscribed bool   `json:"subscribed"`
	Total      int    `json:"total"`
	Unseen     int    `json:"unseen"`
}

// handleMailboxes lists the account's mailboxes with message counts.
func (s *Server) handleMailboxes(w http.ResponseWriter, _ *http.Request, acc *store.Account) {
	boxes, err := s.store.ListMailboxes(acc.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list mailboxes")
		return
	}
	out := make([]mailboxSummary, 0, len(boxes))
	for _, mb := range boxes {
		// TODO: a per-mailbox count is one query each — fine for a
		// scaffold, but a count aggregate would scale better.
		msgs, err := s.store.ListMessages(mb.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not count messages")
			return
		}
		unseen := 0
		for i := range msgs {
			if !hasFlag(msgs[i].Flags, `\Seen`) {
				unseen++
			}
		}
		out = append(out, mailboxSummary{
			Name:       mb.Name,
			Subscribed: mb.Subscribed,
			Total:      len(msgs),
			Unseen:     unseen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// messageSummary is one message in a mailbox listing — metadata only,
// no body.
type messageSummary struct {
	ID      uint64   `json:"id"`
	UID     uint32   `json:"uid"`
	Subject string   `json:"subject"`
	From    string   `json:"from"`
	Date    string   `json:"date"`
	Size    int64    `json:"size"`
	Flags   []string `json:"flags"`
	Seen    bool     `json:"seen"`
}

// handleListMessages lists a mailbox's messages, newest first.
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	mb, err := s.store.GetMailboxByName(acc.ID, r.PathValue("mailbox"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such mailbox")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not open mailbox")
		return
	}
	msgs, err := s.store.ListMessages(mb.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list messages")
		return
	}

	// store.ListMessages is UID-ascending; a mailbox view wants newest
	// first.
	out := make([]messageSummary, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		out = append(out, summarize(&msgs[i]))
	}
	if limit := parseLimit(r); limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, out)
}

// messageDetail is the GET /api/messages/{id} response — metadata plus
// the parsed body.
type messageDetail struct {
	messageSummary
	To          []string         `json:"to"`
	Cc          []string         `json:"cc"`
	MessageID   string           `json:"message_id"`
	Text        string           `json:"text"`
	HTML        string           `json:"html"`
	Attachments []attachmentInfo `json:"attachments"`
}

// handleGetMessage returns one message with its body parsed into text,
// HTML, and an attachment listing.
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}
	m, err := s.store.GetMessage(id)
	// Hide other accounts' messages behind the same 404 as ones that do
	// not exist, so the API does not leak which ids are in use.
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.AccountID != acc.ID) {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load message")
		return
	}
	raw, err := s.store.FetchBody(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load message body")
		return
	}
	body := renderBody(raw)

	writeJSON(w, http.StatusOK, messageDetail{
		messageSummary: summarize(m),
		To:             body.To,
		Cc:             body.Cc,
		MessageID:      m.MessageID,
		Text:           body.Text,
		HTML:           body.HTML,
		Attachments:    body.Attachments,
	})
}

// summarize projects a store.Message into its API summary.
func summarize(m *store.Message) messageSummary {
	return messageSummary{
		ID:      m.ID,
		UID:     m.UID,
		Subject: m.Subject,
		From:    m.FromAddr,
		Date:    m.InternalDate,
		Size:    m.SizeBytes,
		Flags:   m.Flags,
		Seen:    hasFlag(m.Flags, `\Seen`),
	}
}

// hasFlag reports whether flag is present in flags.
func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// parseLimit reads an optional ?limit= query parameter; 0 means "no
// limit".
func parseLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
