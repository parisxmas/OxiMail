package av

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFeed serves a configurable list of SHA-256 hex strings, one
// per line, in the same shape abuse.ch's /export/txt/sha256/recent/
// returns. Two atomics drive the test surface: hits counts calls
// (so we can assert "the updater fired N times") and body holds
// the current response body (so we can verify refresh on change).
type fakeFeed struct {
	t      *testing.T
	server *httptest.Server
	hits   atomic.Int64
	mu     sync.Mutex
	body   string
}

func newFakeFeed(t *testing.T, initial string) *fakeFeed {
	t.Helper()
	f := &fakeFeed{t: t, body: initial}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		body := f.body
		f.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeFeed) setBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body = body
}

// hashOf returns the lower-case-hex sha256 of s. Mirrored from
// av_test.go so updater tests don't reach across files.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestUpdaterFetchesAndPopulatesSigdb(t *testing.T) {
	hash1 := hashOf("payload-A")
	hash2 := hashOf("payload-B")
	feed := newFakeFeed(t, fmt.Sprintf(
		"# header\n%s\n%s\n# trailer\n", hash1, hash2,
	))

	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")

	c, err := New(out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Before the updater runs, only the builtin EICAR is loaded.
	if got := c.SignatureCount(); got != 1 {
		t.Fatalf("pre-refresh signatures = %d, want 1 (EICAR only)", got)
	}

	u := NewUpdater(c, feed.server.URL, out, "TestFeed", 10*time.Millisecond)
	if u == nil {
		t.Fatal("NewUpdater returned nil — bad arg interpretation")
	}
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// EICAR + 2 fed hashes.
	if got := c.SignatureCount(); got != 3 {
		t.Errorf("post-refresh signatures = %d, want 3 (EICAR + 2 feed)", got)
	}
	v, _ := c.Scan(context.Background(), []byte("payload-A"))
	if v.OK || v.Threat != "TestFeed" {
		t.Errorf("payload-A verdict = %+v, want hit on TestFeed", v)
	}

	// File on disk reflects the feed.
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read sigdb: %v", err)
	}
	if got := string(written); !contains(got, hash1) || !contains(got, hash2) {
		t.Errorf("sigdb file missing fetched hashes:\n%s", got)
	}
}

func TestUpdaterHotReloadOnNewBody(t *testing.T) {
	hash1 := hashOf("first-payload")
	hash2 := hashOf("second-payload")
	feed := newFakeFeed(t, hash1+"\n")

	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, feed.server.URL, out, "TestFeed", time.Hour)

	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	if v, _ := c.Scan(context.Background(), []byte("first-payload")); v.OK {
		t.Error("first refresh: payload-1 missed")
	}
	if v, _ := c.Scan(context.Background(), []byte("second-payload")); !v.OK {
		t.Error("first refresh: payload-2 hit before being fed")
	}

	// Feed flips. Next refresh should hot-swap to the new map.
	feed.setBody(hash2 + "\n")
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	if v, _ := c.Scan(context.Background(), []byte("first-payload")); !v.OK {
		t.Error("second refresh: payload-1 should be gone after replace")
	}
	if v, _ := c.Scan(context.Background(), []byte("second-payload")); v.OK {
		t.Error("second refresh: payload-2 missed")
	}
}

func TestUpdaterRejectsGarbageLines(t *testing.T) {
	// Mixed body — comments, blanks, garbage, valid hashes, and a
	// line with a trailing :size suffix that some feeds emit.
	hash := hashOf("valid-payload")
	feed := newFakeFeed(t, fmt.Sprintf(
		"# comment\n\nnothex\n%s\nshort\n%s:1234\n", hash, hashOf("with-size"),
	))

	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, feed.server.URL, out, "TestFeed", time.Hour)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if v, _ := c.Scan(context.Background(), []byte("valid-payload")); v.OK {
		t.Error("valid payload missed")
	}
	if v, _ := c.Scan(context.Background(), []byte("with-size")); v.OK {
		t.Error(":size-suffixed payload missed (suffix should be stripped)")
	}
}

func TestUpdaterHandlesHTTPError(t *testing.T) {
	// Server returns 500 — refresh should report an error but the
	// Client's previous (builtin-only) state stays intact.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, srv.URL, out, "TestFeed", time.Hour)
	if err := u.refresh(context.Background()); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
	if got := c.SignatureCount(); got != 1 {
		t.Errorf("signatures = %d after failed refresh, want 1 (builtin intact)", got)
	}
}

func TestNewUpdaterDisabledOnEmptyArgs(t *testing.T) {
	c, _ := New()
	if u := NewUpdater(c, "", "/tmp/x.sigdb", "S", time.Hour); u != nil {
		t.Error("empty url should yield nil updater")
	}
	if u := NewUpdater(c, "http://x", "", "S", time.Hour); u != nil {
		t.Error("empty out path should yield nil updater")
	}
	if u := NewUpdater(nil, "http://x", "/tmp/x.sigdb", "S", time.Hour); u != nil {
		t.Error("nil client should yield nil updater")
	}
}

func TestUpdaterRunRespectsContext(t *testing.T) {
	// Start an updater with a very short interval, cancel context,
	// confirm Run returns promptly with the cancel error.
	feed := newFakeFeed(t, "")
	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, feed.server.URL, out, "TestFeed", 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil; expected ctx error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run didn't return within 500ms of cancel")
	}
	// At least the initial-tick refresh fired before cancel.
	if feed.hits.Load() < 1 {
		t.Errorf("feed hits = %d, want at least 1 (initial tick)", feed.hits.Load())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
