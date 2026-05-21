package webmail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"

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
		stats, err := s.store.Stats(acc.ID, mb.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not count messages")
			return
		}
		out = append(out, mailboxSummary{
			Name:       mb.Name,
			Subscribed: mb.Subscribed,
			Total:      stats.Total,
			Unseen:     stats.Unseen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// messageSummary is one message in a mailbox listing — metadata only,
// no body. Snippet is populated only when the list call asks for it
// (`?snippets=1`); the default listing skips the per-message body
// fetch so a large mailbox stays fast.
type messageSummary struct {
	ID      uint64   `json:"id"`
	UID     uint32   `json:"uid"`
	Subject string   `json:"subject"`
	From    string   `json:"from"`
	Date    string   `json:"date"`
	Size    int64    `json:"size"`
	Flags   []string `json:"flags"`
	Seen    bool     `json:"seen"`
	Snippet string   `json:"snippet,omitempty"`
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
	msgs, err := s.store.ListMessages(acc.ID, mb.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list messages")
		return
	}

	// store.ListMessages is UID-ascending; a mailbox view wants newest
	// first.
	query := ParseSearchQuery(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := parseLimit(r)
	wantSnippets := r.URL.Query().Get("snippets") == "1"
	out := make([]messageSummary, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		m := &msgs[i]
		// Lazy body/header/attachment fetchers — only realised when
		// the query actually needs them, so a metadata-only search
		// (from:/subject:) stays off the blob store entirely.
		var (
			bodyText        string
			bodyTextFetched bool
			toHeader        string
			toHeaderFetched bool
			hasAtt          bool
			hasAttFetched   bool
		)
		fetchBodyText := func() string {
			if !bodyTextFetched {
				if raw, err := s.store.FetchBody(m); err == nil {
					bodyText = renderText(raw)
				}
				bodyTextFetched = true
			}
			return bodyText
		}
		fetchToHeader := func() string {
			if !toHeaderFetched {
				if raw, err := s.store.FetchBody(m); err == nil {
					toHeader = readToHeader(raw)
				}
				toHeaderFetched = true
			}
			return toHeader
		}
		fetchHasAttachment := func() bool {
			if !hasAttFetched {
				if raw, err := s.store.FetchBody(m); err == nil {
					hasAtt = messageHasAttachment(raw)
				}
				hasAttFetched = true
			}
			return hasAtt
		}
		if !query.matches(m, fetchBodyText, fetchToHeader, fetchHasAttachment) {
			continue
		}
		sum := summarize(m)
		if wantSnippets {
			sum.Snippet = s.computeSnippet(m)
		}
		out = append(out, sum)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// computeSnippet pulls a short text-only preview from the message body
// — at most snippetMax runes, with leading/trailing whitespace
// collapsed. Returns "" if the body can't be fetched (the list view
// is best-effort and a failed snippet just shows no preview).
func (s *Server) computeSnippet(m *store.Message) string {
	raw, err := s.store.FetchBody(m)
	if err != nil {
		return ""
	}
	t := strings.Join(strings.Fields(renderText(raw)), " ")
	if len([]rune(t)) <= snippetMax {
		return t
	}
	r := []rune(t)
	return string(r[:snippetMax]) + "…"
}

// snippetMax bounds the preview returned by computeSnippet. Picked
// to fit on a single line of the mailbox-list row at the SPA's
// default font size without forcing the layout to scroll.
const snippetMax = 140

// readToHeader returns the raw "To:" header value (comma-joined
// addresses, no parsing) from a stored RFC 5322 message. Used by the
// `to:` search operator. An unparseable message yields "" — the
// search operator then sees no match and reports a clean "no
// results" rather than a 500.
func readToHeader(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return msg.Header.Get("To")
}

// messageHasAttachment reports whether the message has at least one
// MIME part with Content-Disposition: attachment (or with a
// filename parameter on Content-Type, which some legacy senders use
// in place of disposition). Used by the `has:attachment` operator.
func messageHasAttachment(raw []byte) bool {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	ct := msg.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/") {
		return false
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	boundary := params["boundary"]
	if boundary == "" {
		return false
	}
	mr := multipart.NewReader(msg.Body, boundary)
	for {
		p, err := mr.NextPart()
		if err != nil {
			return false
		}
		if disp := p.Header.Get("Content-Disposition"); disp != "" {
			if d, _, err := mime.ParseMediaType(disp); err == nil && strings.EqualFold(d, "attachment") {
				return true
			}
		}
		if pct := p.Header.Get("Content-Type"); pct != "" {
			if _, p2, err := mime.ParseMediaType(pct); err == nil && p2["name"] != "" {
				return true
			}
		}
	}
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
	m, ok := s.loadOwnedMessage(w, r, acc)
	if !ok {
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

// flagsRequest is the body of PATCH /api/messages/{id}/flags.
type flagsRequest struct {
	Op    string   `json:"op"` // "add" | "remove" | "set"
	Flags []string `json:"flags"`
}

// handleFlags changes a message's IMAP flags and returns its updated
// summary.
func (s *Server) handleFlags(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	m, ok := s.loadOwnedMessage(w, r, acc)
	if !ok {
		return
	}
	var req flagsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var err error
	switch req.Op {
	case "add":
		err = s.store.AddFlags(acc.ID, m.ID, req.Flags...)
	case "remove":
		err = s.store.RemoveFlags(acc.ID, m.ID, req.Flags...)
	case "set":
		err = s.store.SetFlags(acc.ID, m.ID, req.Flags)
	default:
		writeError(w, http.StatusBadRequest, `op must be "add", "remove", or "set"`)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update flags")
		return
	}
	updated, err := s.store.GetMessage(acc.ID, m.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not reload message")
		return
	}
	writeJSON(w, http.StatusOK, summarize(updated))
}

// moveRequest is the body of POST /api/messages/{id}/move.
type moveRequest struct {
	Mailbox string `json:"mailbox"`
}

// handleMove moves a message into another of the account's mailboxes.
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	m, ok := s.loadOwnedMessage(w, r, acc)
	if !ok {
		return
	}
	var req moveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	dest, err := s.store.GetMailboxByName(acc.ID, req.Mailbox)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such mailbox")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not open destination mailbox")
		return
	}
	moved, err := s.store.MoveMessage(acc.ID, m.ID, dest.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not move message")
		return
	}
	writeJSON(w, http.StatusOK, summarize(moved))
}

// handleDelete permanently removes a message.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	m, ok := s.loadOwnedMessage(w, r, acc)
	if !ok {
		return
	}
	if err := s.store.DeleteMessage(acc.ID, m.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete message")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAttachment streams the n-th attachment of a message back to
// the client. n is 0-based and matches the order in which the parsed
// message exposes attachments via GET /api/messages/{id}.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	m, ok := s.loadOwnedMessage(w, r, acc)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || idx < 0 {
		writeError(w, http.StatusBadRequest, "invalid attachment index")
		return
	}
	raw, err := s.store.FetchBody(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load message body")
		return
	}
	content, filename, contentType, ok := extractAttachment(raw, idx)
	if !ok {
		writeError(w, http.StatusNotFound, "no such attachment")
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if filename == "" {
		filename = "attachment"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	_, _ = w.Write(content)
}

// sendRequest is the body of POST /api/messages.
type sendRequest struct {
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	// Optional HTML body. When non-empty, the outbound message is
	// multipart/alternative carrying both Text and HTML.
	HTML string `json:"html"`
	// Optional threading metadata for reply / forward composes. Both
	// values are Message-IDs without angle brackets; the server adds
	// them. References should already include in_reply_to as its
	// last entry (the client builds it from the original message's
	// References + the original's own Message-Id, per RFC 5322 §3.6.4).
	InReplyTo  string   `json:"in_reply_to,omitempty"`
	References []string `json:"references,omitempty"`
	// Optional file attachments. Each entry carries the filename,
	// content type, and the file bytes base64-encoded. The server
	// decodes, enforces maxAttachmentBytes total, and wraps the
	// message in multipart/mixed. Empty / nil means a plain message.
	Attachments []attachmentInput `json:"attachments,omitempty"`
}

// attachmentInput is one file attached to an outbound message. Sent
// over JSON, with Data base64-encoded — gross on the wire but works
// with the existing JSON pipeline and keeps the API single-shot
// (no separate multipart upload flow). For a personal mail server's
// typical attachment sizes (a few MB) the ~33% base64 overhead is
// fine; the maxAttachmentBytes cap below is what keeps it honest.
type attachmentInput struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Data        string `json:"data"` // base64-encoded bytes
}

// maxAttachmentBytes is the hard cap on the total decoded attachment
// payload for one outbound message. 25 MB matches Gmail's outbound
// limit and is what most receiving MTAs accept without splitting
// messages or rejecting them outright. Per-attachment count is not
// capped — one big file or twenty small files both have to fit
// under the same byte budget.
const maxAttachmentBytes = 25 * 1024 * 1024

// sendResponse reports how a sent message was dispatched.
type sendResponse struct {
	Delivered int `json:"delivered"` // copies filed into local mailboxes
	Queued    int `json:"queued"`    // recipients handed to the outbound queue
}

// handleSend composes a message from the request, files a copy into the
// sender's Sent mailbox, and routes it: local recipients are delivered
// directly, remote ones are queued for relay.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	var req sendRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	recipients, err := collectRecipients(req.To, req.Cc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	attachments, err := decodeAttachments(req.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	messageID := randomID() + "@" + addressDomain(acc.Address)
	raw := buildMessage(composeFields{
		from:        formatFromHeader(acc.DisplayName, acc.Address),
		to:          req.To,
		cc:          req.Cc,
		subject:     req.Subject,
		text:        req.Text,
		html:        req.HTML,
		messageID:   messageID,
		inReplyTo:   req.InReplyTo,
		references:  req.References,
		attachments: attachments,
	})
	in := store.IncomingMessage{
		Raw:       raw,
		Subject:   req.Subject,
		FromAddr:  formatFromAddr(acc.DisplayName, acc.Address),
		MessageID: messageID,
	}

	routed, err := s.store.Route(acc.Address, recipients, in)
	if err != nil {
		log.Printf("webmail: send from %s: %v", acc.Address, err)
		writeError(w, http.StatusBadGateway, "could not send message")
		return
	}

	// File a copy into Sent — best-effort; the message is already on its
	// way, so a Sent-folder hiccup must not fail the send.
	if sent, err := s.store.GetMailboxByName(acc.ID, "Sent"); err == nil {
		if _, err := s.store.AppendMessage(acc.ID, sent.ID, in); err != nil {
			log.Printf("webmail: save to Sent for %s: %v", acc.Address, err)
		}
	}

	writeJSON(w, http.StatusOK, sendResponse{Delivered: routed.LocalCount, Queued: routed.QueuedCount})
}

// draftRequest is the body of POST /api/drafts. The payload is the
// same compose-form data send accepts, plus an optional id of an
// existing draft to overwrite (so auto-save keeps a stable spot in
// the Drafts folder instead of piling up versions).
type draftRequest struct {
	sendRequest
	ID uint64 `json:"id,omitempty"`
}

// handleSaveDraft files a compose-form payload into the account's
// Drafts folder. The response is the resulting message summary so the
// SPA can pick up its id (and keep submitting that id back on
// subsequent auto-saves to overwrite in place).
func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request, acc *store.Account) {
	var req draftRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	drafts, err := s.store.GetMailboxByName(acc.ID, "Drafts")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Drafts folder not available")
		return
	}
	attachments, err := decodeAttachments(req.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	messageID := randomID() + "@" + addressDomain(acc.Address)
	raw := buildMessage(composeFields{
		from:        formatFromHeader(acc.DisplayName, acc.Address),
		to:          req.To,
		cc:          req.Cc,
		subject:     req.Subject,
		text:        req.Text,
		html:        req.HTML,
		messageID:   messageID,
		inReplyTo:   req.InReplyTo,
		references:  req.References,
		attachments: attachments,
	})
	in := store.IncomingMessage{
		Raw:       raw,
		Subject:   req.Subject,
		FromAddr:  formatFromAddr(acc.DisplayName, acc.Address),
		MessageID: messageID,
		Flags:     []string{`\Draft`},
	}
	// Overwriting an existing draft: delete the old document first so
	// the user sees one (newer) entry in Drafts rather than many.
	if req.ID != 0 {
		if prev, err := s.store.GetMessage(acc.ID, req.ID); err == nil && prev.AccountID == acc.ID && prev.MailboxID == drafts.ID {
			_ = s.store.DeleteMessage(acc.ID, prev.ID)
		}
	}
	saved, err := s.store.AppendMessage(acc.ID, drafts.ID, in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save draft")
		return
	}
	writeJSON(w, http.StatusOK, summarize(saved))
}

// loadOwnedMessage parses the {id} path value and loads the message,
// requiring it to belong to acc. A message that is missing — or that
// belongs to another account — yields the same 404, so the API does not
// leak which ids are in use. It writes the error response itself and
// returns ok=false when the caller should stop.
func (s *Server) loadOwnedMessage(w http.ResponseWriter, r *http.Request, acc *store.Account) (*store.Message, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return nil, false
	}
	m, err := s.store.GetMessage(acc.ID, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.AccountID != acc.ID) {
		writeError(w, http.StatusNotFound, "no such message")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load message")
		return nil, false
	}
	return m, true
}

// decodeAttachments base64-decodes the SPA-side attachment payload
// and enforces maxAttachmentBytes across the whole set. Returns the
// per-attachment data ready for buildMessage. An empty / nil input
// returns nil, nil — no error.
//
// Validation is strict on purpose: a bad payload from the SPA is more
// likely to mean a bug than user fat-fingering, so we surface it as a
// 400 rather than silently dropping the file.
func decodeAttachments(in []attachmentInput) ([]attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	total := 0
	out := make([]attachment, 0, len(in))
	for i, a := range in {
		if ext, blocked := blockedAttachmentExtension(a.Filename); blocked {
			return nil, fmt.Errorf(
				"attachment %d (%q): %s files are blocked — common malware vector. "+
					"Wrap the file in a zip if you really need to send it.",
				i, a.Filename, ext)
		}
		bytes, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, fmt.Errorf("attachment %d (%q): not valid base64: %w", i, a.Filename, err)
		}
		total += len(bytes)
		if total > maxAttachmentBytes {
			return nil, fmt.Errorf("attachments exceed %d bytes (Gmail-style limit) — split into multiple messages",
				maxAttachmentBytes)
		}
		out = append(out, attachment{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Content:     bytes,
		})
	}
	return out, nil
}

// blockedAttachmentExts is the set of file extensions we refuse to send
// from the webmail composer. The list is the consensus "obvious
// malware vector" set used by Gmail / Outlook / most ISP submission
// servers: native Windows executables and shortcuts, Windows / Mac
// scripts that run on double-click, disk-image containers commonly
// used to evade Mark-of-the-Web, and macro-enabled Office documents.
// We deliberately do NOT block archives (.zip, .7z, .rar) — they have
// legitimate uses and the dangerous payload still needs a user step
// (extract + run) that OS protections cover.
//
// All keys are lowercase, leading dot included, so the lookup is just
// `blockedAttachmentExts[strings.ToLower(ext)]`.
var blockedAttachmentExts = map[string]bool{
	".exe": true, ".bat": true, ".cmd": true, ".com": true,
	".scr": true, ".pif": true, ".lnk": true,
	".vbs": true, ".vbe": true, ".js": true, ".jse": true,
	".wsf": true, ".wsh": true, ".hta": true,
	".jar": true,
	".ps1": true, ".ps2": true,
	".msi": true, ".msp": true,
	".iso": true, ".img": true, ".vhd": true, ".vhdx": true,
	".docm": true, ".dotm": true,
	".xlsm": true, ".xltm": true, ".xlsb": true,
	".pptm": true, ".potm": true, ".ppam": true,
}

// blockedAttachmentExtension reports whether the filename's last
// extension is in the blocklist. The second return is the offending
// extension (with the dot, lowercased) — useful for the error
// message so the user knows *why* their attachment was refused.
// Filenames without an extension are always allowed.
//
// Double-extension files (e.g. "report.pdf.exe") block on the LAST
// extension only — that's the one Windows uses to pick the handler.
func blockedAttachmentExtension(filename string) (string, bool) {
	dot := strings.LastIndexByte(filename, '.')
	if dot < 0 || dot == len(filename)-1 {
		return "", false
	}
	ext := strings.ToLower(filename[dot:])
	return ext, blockedAttachmentExts[ext]
}

// collectRecipients merges, trims, and de-duplicates the To and Cc
// lists, requiring each address to look like an address and the result
// to be non-empty.
func collectRecipients(to, cc []string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	for _, addr := range append(append([]string{}, to...), cc...) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if !strings.Contains(addr, "@") {
			return nil, fmt.Errorf("invalid recipient address %q", addr)
		}
		if !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one recipient is required")
	}
	return out, nil
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
