package webmail

import (
	"errors"
	"net/http"
	"strings"
)

// contentSecurityPolicy applies to every response. Scripts come from
// the origin only. Styles need 'unsafe-inline' because Angular and
// Quill inject style elements and attributes at run time. img-src
// allows https: only so that "show images" can work: a srcdoc frame
// inherits this policy and can narrow it but not widen it, and the
// frame's own policy (web/src/app/mailbox/mail-frame.component.ts)
// blocks remote images until the reader asks for them.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// securityHeaders sets the browser hardening headers on every response,
// whichever handler (or the mux's own 404 / 405) produces it.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// Request body limits. Login is unauthenticated, so it gets the least.
// Compose carries attachments as base64 inside JSON: the attachment cap
// grown by 4/3, plus room for the text and HTML bodies.
const (
	maxLoginBody   = 8 << 10
	maxJSONBody    = 256 << 10
	maxComposeBody = maxAttachmentBytes/3*4 + 8<<20
)

// limitBody caps every request body. Doing it here rather than per
// handler means a new route gets the small default without opting in;
// only the routes named below get anything else.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxJSONBody)
		switch r.URL.Path {
		case "/api/login":
			limit = maxLoginBody
		case "/api/messages", "/api/drafts":
			limit = maxComposeBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// writeBodyError reports a decodeJSON failure: 413 when the body went
// over its limit, 400 otherwise.
func writeBodyError(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid request body")
}

// safeAttachmentTypes are the only sender-declared types echoed back.
// None of them is a document a browser would run script in; the rest,
// text/html and image/svg+xml above all, go out as octet-stream.
var safeAttachmentTypes = map[string]bool{
	"application/pdf": true,
	"image/gif":       true,
	"image/jpeg":      true,
	"image/png":       true,
	"image/webp":      true,
	"text/plain":      true,
}

// attachmentContentType maps the sender's Content-Type to the one we
// serve.
func attachmentContentType(declared string) string {
	ct := strings.ToLower(strings.TrimSpace(declared))
	if safeAttachmentTypes[ct] {
		return ct
	}
	return "application/octet-stream"
}
