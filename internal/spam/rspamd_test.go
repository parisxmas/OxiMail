package spam

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerdictFromAction(t *testing.T) {
	cases := map[string]Verdict{
		"no action":       Accept,
		"add header":      Accept,
		"rewrite subject": Accept,
		"":                Accept,
		"quarantine":      Accept, // unrecognised — deliver it
		"greylist":        Greylist,
		"soft reject":     Greylist,
		"reject":          Reject,
	}
	for action, want := range cases {
		if got := verdictFromAction(action); got != want {
			t.Errorf("verdictFromAction(%q) = %v, want %v", action, got, want)
		}
	}
}

// fakeRspamd is a stand-in Rspamd daemon: it records the last request's
// envelope headers and body and replies with a fixed action.
type fakeRspamd struct {
	action string

	gotIP   string
	gotFrom string
	gotRcpt []string
	gotBody string
}

func (f *fakeRspamd) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.gotIP = r.Header.Get("IP")
		f.gotFrom = r.Header.Get("From")
		f.gotRcpt = r.Header.Values("Rcpt")
		body, _ := io.ReadAll(r.Body)
		f.gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"action": f.action, "score": 1.0})
	}
}

func TestRspamdChecker(t *testing.T) {
	for action, want := range map[string]Verdict{
		"no action":   Accept,
		"greylist":    Greylist,
		"soft reject": Greylist,
		"reject":      Reject,
	} {
		fake := &fakeRspamd{action: action}
		srv := httptest.NewServer(fake.handler())

		c := newRspamdChecker(srv.URL)
		got := c.check("1.2.3.4", "sender@x.test", []string{"a@local", "b@local"}, []byte("raw message"))

		// Close blocks until the handler has returned, so the recorded
		// fields are safe to read.
		srv.Close()

		if got != want {
			t.Errorf("action %q: verdict = %v, want %v", action, got, want)
		}
		if fake.gotIP != "1.2.3.4" || fake.gotFrom != "sender@x.test" {
			t.Errorf("action %q: envelope headers = IP %q From %q", action, fake.gotIP, fake.gotFrom)
		}
		if len(fake.gotRcpt) != 2 {
			t.Errorf("action %q: Rcpt headers = %v, want 2", action, fake.gotRcpt)
		}
		if fake.gotBody != "raw message" {
			t.Errorf("action %q: forwarded body = %q", action, fake.gotBody)
		}
	}
}

func TestRspamdFailsOpen(t *testing.T) {
	// A server that errors on every request.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if v := newRspamdChecker(errSrv.URL).check("1.2.3.4", "x@y.test", nil, []byte("m")); v != Accept {
		t.Errorf("a 500 from rspamd: verdict = %v, want Accept (fail open)", v)
	}
	errSrv.Close()

	// An endpoint with nothing listening — the connection is refused.
	if v := newRspamdChecker(errSrv.URL).check("1.2.3.4", "x@y.test", nil, []byte("m")); v != Accept {
		t.Errorf("unreachable rspamd: verdict = %v, want Accept (fail open)", v)
	}
}

func TestPipelineRspamdStage(t *testing.T) {
	fake := &fakeRspamd{action: "reject"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	clk := newFakeClock()
	p := New("", nil)                    // no rspamd from the constructor...
	p.rspamd = newRspamdChecker(srv.URL) // ...inject one directly
	p.rateLimit.now = clk.now
	p.greylist.now = clk.now
	p.dnsbl.lookup = notListed

	// First contact is greylisted at stage 1 — stage 3 is never reached.
	if v, _ := p.Check("9.9.9.9", "s@x.test", []string{"a@local"}, []byte("m")); v != Greylist {
		t.Fatalf("first contact = %v, want Greylist", v)
	}
	// Past the greylist delay, stage 1 passes and the message reaches
	// Rspamd, which rejects it.
	clk.advance(2 * time.Minute)
	if v, _ := p.Check("9.9.9.9", "s@x.test", []string{"a@local"}, []byte("m")); v != Reject {
		t.Fatalf("after greylisting = %v, want Reject (from rspamd)", v)
	}
}
