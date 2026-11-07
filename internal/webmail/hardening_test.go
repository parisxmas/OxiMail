//go:build integration

// End-to-end checks of the webmail hardening: what a browser actually
// receives for hostile mail, the headers on every kind of response, and
// login timing.
package webmail_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/store"
)

func TestWebmailHardening(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	acc, err := st.CreateAccount("user@oximail.test", hash, 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("ensure mailboxes: %v", err)
	}
	inbox, err := st.GetMailboxByName(acc.ID, "INBOX")
	if err != nil {
		t.Fatalf("get INBOX: %v", err)
	}
	raw := []byte("From: Evil <evil@elsewhere.test>\r\n" +
		"To: user@oximail.test\r\n" +
		"Subject: hostile\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p onclick=\"steal()\">watermelons</p><script>steal()</script>" +
		"<img src=\"https://track.test/p.gif\">\r\n" +
		"--B\r\n" +
		"Content-Type: text/html\r\n" +
		"Content-Disposition: attachment; filename=\"page.html\"\r\n" +
		"\r\n" +
		"<script>steal()</script>\r\n" +
		"--B--\r\n")
	m, err := st.AppendMessage(acc.ID, inbox.ID, store.IncomingMessage{
		Raw: raw, Subject: "hostile", MessageID: "h-1@elsewhere.test", FromAddr: "evil@elsewhere.test",
	})
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}

	base := startWebmail(t, st)
	var login struct{ Token string }
	if status := postJSON(t, base+"/api/login", "", loginBody("user@oximail.test", "s3cret"), &login); status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}
	msgURL := base + "/api/messages/" + jsonNumber(m.ID)

	t.Run("message HTML is sanitised and remote images held back", func(t *testing.T) {
		var out struct {
			HTML         string `json:"html"`
			RemoteImages bool   `json:"remote_images"`
		}
		if status := getJSON(t, msgURL, login.Token, &out); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		for _, bad := range []string{"<script", "onclick", "steal", "track.test"} {
			if strings.Contains(out.HTML, bad) {
				t.Errorf("html %q contains %q", out.HTML, bad)
			}
		}
		if !strings.Contains(out.HTML, "watermelons") {
			t.Errorf("html %q lost the text", out.HTML)
		}
		if !out.RemoteImages {
			t.Error("remote_images = false, want true")
		}
	})

	t.Run("images=1 returns remote images but nothing else", func(t *testing.T) {
		var out struct {
			HTML string `json:"html"`
		}
		if status := getJSON(t, msgURL+"?images=1", login.Token, &out); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if !strings.Contains(out.HTML, "https://track.test/p.gif") {
			t.Errorf("html %q lacks the image", out.HTML)
		}
		if strings.Contains(out.HTML, "steal") {
			t.Errorf("html %q was not sanitised", out.HTML)
		}
	})

	t.Run("search still sees the original body", func(t *testing.T) {
		var out []struct{ ID uint64 }
		if status := getJSON(t, base+"/api/mailboxes/INBOX/messages?q=watermelons", login.Token, &out); status != http.StatusOK || len(out) != 1 {
			t.Fatalf("status = %d, hits = %d", status, len(out))
		}
	})

	t.Run("an HTML attachment is served as an inert download", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, msgURL+"/attachments/0", nil)
		req.Header.Set("Authorization", "Bearer "+login.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type = %q", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
			t.Errorf("Content-Disposition = %q", cd)
		}
		if n := resp.Header.Get("X-Content-Type-Options"); n != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q", n)
		}
	})

	t.Run("every kind of response carries the headers", func(t *testing.T) {
		for _, path := range []string{"/api/mailboxes", "/api/no-such-thing", "/nothing-here"} {
			resp, err := http.Get(base + path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.Header.Get("Content-Security-Policy") == "" ||
				resp.Header.Get("X-Frame-Options") != "DENY" ||
				resp.Header.Get("Referrer-Policy") != "no-referrer" ||
				resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s: headers missing: %v", path, resp.Header)
			}
		}
	})

	t.Run("an oversized unauthenticated login is refused", func(t *testing.T) {
		body := loginBody("user@oximail.test", strings.Repeat("x", 1<<20))
		if status := post(t, base+"/api/login", "", body); status != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", status)
		}
	})

	t.Run("sieve PUT explains what it refuses", func(t *testing.T) {
		for script, want := range map[string]string{
			`redirect "a@b.test";`:              `"redirect"`,
			`require "vacation"; keep;`:         `"vacation"`,
			`fileinto :flags "\\Seen" "X";`:     ":flags",
			`if envelope "to" "x" { discard; }`: `"envelope"`,
		} {
			body, _ := json.Marshal(map[string]string{"source": script})
			req, _ := http.NewRequest(http.MethodPut, base+"/api/sieve", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+login.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			msg, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(msg), strings.ReplaceAll(want, `"`, `\"`)) {
				t.Errorf("%s: status %d, body %s", script, resp.StatusCode, msg)
			}
		}
	})
}

// A failed login must take about as long, and read the same, whether or
// not the account exists. Without the dummy comparison an unknown
// address skips bcrypt and answers tens of times faster, so a loose
// ratio is enough and stays stable on a busy machine.
func TestWebmailLoginTimingHidesAccounts(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := st.CreateAccount("user@oximail.test", hash, 0); err != nil {
		t.Fatalf("create account: %v", err)
	}
	base := startWebmail(t, st)

	attempt := func(address string) (time.Duration, int, string) {
		start := time.Now()
		resp, err := http.Post(base+"/api/login", "application/json", bytes.NewReader(loginBody(address, "wrong")))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return time.Since(start), resp.StatusCode, string(body)
	}
	// Fastest of a few, to shed scheduling noise. Four tries stay under
	// the per-account limit; four of each stay under the per-IP one.
	fastest := func(address func(i int) string) (time.Duration, int, string) {
		var best time.Duration
		var status int
		var body string
		for i := 0; i < 4; i++ {
			d, s, b := attempt(address(i))
			if i == 0 || d < best {
				best = d
			}
			status, body = s, b
		}
		return best, status, body
	}
	known, knownStatus, knownBody := fastest(func(int) string { return "user@oximail.test" })
	unknown, unknownStatus, unknownBody := fastest(func(i int) string {
		return "nobody" + string(rune('a'+i)) + "@oximail.test"
	})

	if knownStatus != http.StatusUnauthorized || unknownStatus != knownStatus {
		t.Errorf("status known = %d, unknown = %d, want both 401", knownStatus, unknownStatus)
	}
	if knownBody != unknownBody {
		t.Errorf("bodies differ: %q vs %q", knownBody, unknownBody)
	}
	if unknown < known/3 {
		t.Errorf("unknown account answered in %v, known in %v: timing reveals the account", unknown, known)
	}
}

func jsonNumber(id uint64) string {
	b, _ := json.Marshal(id)
	return string(b)
}
