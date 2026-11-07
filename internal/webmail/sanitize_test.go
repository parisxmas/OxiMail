package webmail

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each input must lose the listed substring (matched case-insensitively
// against the output).
func TestSanitizeHTMLStrips(t *testing.T) {
	cases := []struct{ name, in, gone string }{
		{"script", `<p>a</p><script>alert(1)</script>`, "alert"},
		{"event handler", `<p onclick="alert(1)">a</p>`, "onclick"},
		{"img onerror", `<img src=x onerror=alert(1)>`, "onerror"},
		{"javascript href", `<a href="javascript:alert(1)">a</a>`, "javascript"},
		{"mixed case scheme", `<a href="JaVaScRiPt:alert(1)">a</a>`, "javascript"},
		{"tab in scheme", "<a href=\"java\tscript:alert(1)\">a</a>", "script"},
		{"entity in scheme", `<a href="java&#115;cript:alert(1)">a</a>`, "script"},
		{"leading space", `<a href="  javascript:alert(1)">a</a>`, "javascript"},
		{"vbscript href", `<a href="vbscript:x">a</a>`, "vbscript"},
		{"data href", `<a href="data:text/html,x">a</a>`, "data:"},
		{"relative href", `<a href="/api/logout">a</a>`, "/api/logout"},
		{"form", `<form action="http://e.test/"><p>a</p></form>`, "<form"},
		{"form action", `<form action="http://e.test/"><p>a</p></form>`, "e.test"},
		{"input", `<input name=pw>`, "<input"},
		{"button formaction", `<button formaction="http://e.test/">x</button>`, "e.test"},
		{"iframe", `<iframe src="http://e.test/"></iframe>`, "iframe"},
		{"object", `<object data="http://e.test/x"></object>`, "object"},
		{"embed", `<embed src="http://e.test/x">`, "embed"},
		{"svg", `<svg onload=alert(1)><script>alert(1)</script></svg>`, "alert"},
		{"math", `<math><mi xlink:href="javascript:x">a</mi></math>`, "javascript"},
		{"style element", `<style>@import "http://e.test/x.css";</style>`, "e.test"},
		{"link element", `<link rel=stylesheet href="http://e.test/x.css">`, "e.test"},
		{"meta refresh", `<meta http-equiv=refresh content="0;url=http://e.test/">`, "e.test"},
		{"base", `<base href="http://e.test/">`, "e.test"},
		{"comment", `<!--[if IE]><script>alert(1)</script><![endif]-->`, "alert"},
		{"css url", `<div style="background:url(http://e.test/p.gif)">a</div>`, "e.test"},
		{"css escape", `<div style="background:u\72l(http://e.test/p.gif)">a</div>`, "e.test"},
		{"css expression", `<div style="width:expression(alert(1))">a</div>`, "expression"},
		{"background attr", `<table background="http://e.test/p.gif"><tr><td>a</td></tr></table>`, "e.test"},
		{"srcset", `<img srcset="http://e.test/p.gif 1x">`, "e.test"},
		{"svg data image", `<img src="data:image/svg+xml;base64,AAAA">`, "svg"},
		{"relative image", `<img src="/api/mailboxes">`, "/api"},
		{"noscript breakout", `<noscript><p title="</noscript><img src=x onerror=alert(1)>">`, "onerror"},
		{"title breakout", `<title><img src=x onerror=alert(1)></title>`, "<img"},
		{"textarea breakout", `<textarea><img src=x onerror=alert(1)></textarea>`, "onerror"},
		{"unknown element attr", `<foo onmouseover="alert(1)">a</foo>`, "onmouseover"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, images := range []bool{false, true} {
				got, _ := sanitizeHTML(c.in, images)
				if strings.Contains(strings.ToLower(got), c.gone) {
					t.Errorf("showImages=%v: %q still contains %q", images, got, c.gone)
				}
			}
		})
	}
}

func TestSanitizeHTMLKeeps(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<p>hello <b>world</b></p>`, `<p>hello <b>world</b></p>`},
		{`<table width="100%"><tr><td colspan="2">a</td></tr></table>`, `<td colspan="2">a</td>`},
		{`<div style="color: red; background:url(x); font-size: 12px">a</div>`, `style="color: red; font-size: 12px"`},
		{`<form><p>kept text</p></form>`, `<p>kept text</p>`},
		{`<foo><b>kept</b></foo>`, `<b>kept</b>`},
		{`<a href="mailto:a@x.test">m</a>`, `href="mailto:a@x.test"`},
		{`<img src="data:image/png;base64,AAAA" alt="logo">`, `src="data:image/png;base64,AAAA"`},
	}
	for _, c := range cases {
		if got, _ := sanitizeHTML(c.in, false); !strings.Contains(got, c.want) {
			t.Errorf("sanitizeHTML(%q) = %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

// A link must open outside the sandboxed frame.
func TestSanitizeHTMLLinksLeaveTheFrame(t *testing.T) {
	got, _ := sanitizeHTML(`<a href="https://x.test/" target="_top">a</a>`, false)
	want := `<a href="https://x.test/" target="_blank" rel="noopener noreferrer">a</a>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeHTMLRemoteImages(t *testing.T) {
	for _, src := range []string{
		"http://t.test/p.gif", "https://t.test/p.gif", "//t.test/p.gif",
		"HTTPS://t.test/p.gif", " https://t.test/p.gif",
	} {
		in := `<img src="` + src + `" alt="pixel" width="1">`
		blocked, remote := sanitizeHTML(in, false)
		if !remote {
			t.Errorf("%s: hasRemote = false", src)
		}
		if strings.Contains(blocked, "t.test") || strings.Contains(blocked, "src=") {
			t.Errorf("%s: blocked output %q still loads it", src, blocked)
		}
		if !strings.Contains(blocked, `alt="pixel"`) {
			t.Errorf("%s: blocked output %q lost the alt text", src, blocked)
		}
		shown, remote := sanitizeHTML(in, true)
		if !remote || !strings.Contains(shown, "t.test/p.gif") {
			t.Errorf("%s: shown = %q, hasRemote = %v", src, shown, remote)
		}
	}
	if _, remote := sanitizeHTML(`<img src="data:image/gif;base64,AAAA"><p>x</p>`, false); remote {
		t.Error("a data: image must not count as remote")
	}
}

func TestSanitizeHTMLDeepNestingIsContained(t *testing.T) {
	in := strings.Repeat("<div>", 100000) + "<script>alert(1)</script>"
	if got, _ := sanitizeHTML(in, false); strings.Contains(got, "<script") {
		t.Errorf("script survived in %d bytes of output", len(got))
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for k, v := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self';", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	for _, part := range strings.Split(csp, ";") {
		if strings.Contains(part, "script-src") && strings.Contains(part, "unsafe") {
			t.Errorf("script-src must not be relaxed: %q", part)
		}
	}
}

func TestAttachmentContentType(t *testing.T) {
	for in, want := range map[string]string{
		"text/html":                 "application/octet-stream",
		"TEXT/HTML":                 "application/octet-stream",
		"image/svg+xml":             "application/octet-stream",
		"application/xhtml+xml":     "application/octet-stream",
		"application/javascript":    "application/octet-stream",
		"multipart/x-mixed-replace": "application/octet-stream",
		"":                          "application/octet-stream",
		"application/pdf":           "application/pdf",
		"IMAGE/PNG":                 "image/png",
		"text/plain":                "text/plain",
	} {
		if got := attachmentContentType(in); got != want {
			t.Errorf("attachmentContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLimitBody(t *testing.T) {
	var got error
	h := limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if got = decodeJSON(r, &v); got != nil {
			writeBodyError(w, got)
		}
	}))
	send := func(path string, size int) int {
		body := `{"a":"` + strings.Repeat("x", size) + `"}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec.Code
	}
	cases := []struct {
		path string
		size int
		want int
	}{
		{"/api/login", maxLoginBody / 2, http.StatusOK},
		{"/api/login", maxLoginBody, http.StatusRequestEntityTooLarge},
		{"/api/sieve", maxJSONBody / 2, http.StatusOK},
		{"/api/sieve", maxJSONBody, http.StatusRequestEntityTooLarge},
		{"/api/messages/1/move", maxJSONBody, http.StatusRequestEntityTooLarge},
		{"/api/messages", maxAttachmentBytes / 3 * 4, http.StatusOK},
		{"/api/drafts", maxAttachmentBytes / 3 * 4, http.StatusOK},
		{"/api/messages", maxComposeBody, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		if code := send(c.path, c.size); code != c.want {
			t.Errorf("%s with %d bytes: status %d, want %d", c.path, c.size, code, c.want)
		}
	}
}

func TestWriteBodyErrorBadJSONIs400(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader("{"))
	rec := httptest.NewRecorder()
	var v map[string]any
	writeBodyError(rec, decodeJSON(r, &v))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
