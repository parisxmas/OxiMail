//go:build integration

// Integration test for the observability HTTP server: /healthz,
// /readyz, and the Prometheus /metrics endpoint.
package observability_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/store"
)

func TestObservability(t *testing.T) {
	var stopDB func()
	host, port := itest.StartOxiDB(t, itest.LazySync(), itest.WithStop(&stopDB))

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	base := startObservability(t, st)

	t.Run("/healthz", func(t *testing.T) {
		body, code := httpGet(t, base+"/healthz")
		if code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
		if !strings.Contains(body, "ok") {
			t.Errorf("body = %q, want it to contain \"ok\"", body)
		}
	})

	t.Run("/readyz with a working store", func(t *testing.T) {
		if _, code := httpGet(t, base+"/readyz"); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("/metrics exposes the registered counters", func(t *testing.T) {
		body, code := httpGet(t, base+"/metrics")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		for _, name := range []string{
			"oximail_smtp_messages_total",
			"oximail_queue_deliveries_total",
			"oximail_queue_due_messages",
			"oximail_logins_total",
		} {
			if !strings.Contains(body, name) {
				t.Errorf("metrics output is missing %q", name)
			}
		}
	})

	t.Run("an increment is reflected in /metrics", func(t *testing.T) {
		observability.SMTPMessages.WithLabelValues("accept").Inc()
		observability.SMTPMessages.WithLabelValues("accept").Inc()
		body, _ := httpGet(t, base+"/metrics")
		if !strings.Contains(body, `oximail_smtp_messages_total{verdict="accept"} 2`) {
			t.Errorf("counter not reflected in /metrics:\n%s", body)
		}
	})

	t.Run("/readyz with a dead store returns 503", func(t *testing.T) {
		// Killing the backend simulates a failed store; readiness must
		// turn red. (Merely closing the client is not enough — the
		// store redials a live server transparently.)
		stopDB()
		_, code := httpGet(t, base+"/readyz")
		if code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", code)
		}
	})
}

// startObservability launches the observability server on a free port,
// wired to st, and returns its base URL. It is shut down when the test
// ends.
func startObservability(t *testing.T, st *store.Store) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := observability.New(addr, st)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("observability server exited with: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("observability server did not stop within 5s")
		}
	})

	itest.WaitTCP(t, addr)
	return "http://" + addr
}

func httpGet(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
