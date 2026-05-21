package av

import (
	"archive/zip"
	"bytes"
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

	u := NewUpdater(c, []Feed{{URL: feed.server.URL, Source: "TestFeed"}}, out, 10*time.Millisecond)
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
	u := NewUpdater(c, []Feed{{URL: feed.server.URL, Source: "TestFeed"}}, out, time.Hour)

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
	u := NewUpdater(c, []Feed{{URL: feed.server.URL, Source: "TestFeed"}}, out, time.Hour)
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
	u := NewUpdater(c, []Feed{{URL: srv.URL, Source: "TestFeed"}}, out, time.Hour)
	if err := u.refresh(context.Background()); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
	if got := c.SignatureCount(); got != 1 {
		t.Errorf("signatures = %d after failed refresh, want 1 (builtin intact)", got)
	}
}

func TestNewUpdaterDisabledOnEmptyArgs(t *testing.T) {
	c, _ := New()
	one := []Feed{{URL: "http://x", Source: "S"}}
	if u := NewUpdater(c, nil, "/tmp/x.sigdb", time.Hour); u != nil {
		t.Error("empty feed list should yield nil updater")
	}
	if u := NewUpdater(c, []Feed{{URL: "", Source: "S"}}, "/tmp/x.sigdb", time.Hour); u != nil {
		t.Error("feed list with only empty URLs should yield nil updater")
	}
	if u := NewUpdater(c, one, "", time.Hour); u != nil {
		t.Error("empty out path should yield nil updater")
	}
	if u := NewUpdater(nil, one, "/tmp/x.sigdb", time.Hour); u != nil {
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
	u := NewUpdater(c, []Feed{{URL: feed.server.URL, Source: "TestFeed"}}, out, 50*time.Millisecond)

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

// TestUpdaterMergesMultipleFeeds — the multi-source contract: one
// feed contributes hash A, another contributes hash B, both end up
// loaded with their respective source labels.
func TestUpdaterMergesMultipleFeeds(t *testing.T) {
	hashA := hashOf("payload-A")
	hashB := hashOf("payload-B")
	feedA := newFakeFeed(t, hashA+"\n")
	feedB := newFakeFeed(t, hashB+"\n")

	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, []Feed{
		{URL: feedA.server.URL, Source: "FeedA"},
		{URL: feedB.server.URL, Source: "FeedB"},
	}, out, time.Hour)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := c.SignatureCount(); got != 3 {
		t.Errorf("signatures = %d, want 3 (EICAR + A + B)", got)
	}
	if v, _ := c.Scan(context.Background(), []byte("payload-A")); v.OK || v.Threat != "FeedA" {
		t.Errorf("payload-A verdict = %+v, want hit on FeedA", v)
	}
	if v, _ := c.Scan(context.Background(), []byte("payload-B")); v.OK || v.Threat != "FeedB" {
		t.Errorf("payload-B verdict = %+v, want hit on FeedB", v)
	}
}

// TestUpdaterContinuesWhenOneFeedFails — a transient outage on one
// feed must not knock out the rest. Hashes from the surviving feeds
// stay live; the failed-feed line shows up in the sigdb header.
func TestUpdaterContinuesWhenOneFeedFails(t *testing.T) {
	hashGood := hashOf("good-payload")
	goodFeed := newFakeFeed(t, hashGood+"\n")
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer badSrv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, []Feed{
		{URL: badSrv.URL, Source: "BadFeed"},
		{URL: goodFeed.server.URL, Source: "GoodFeed"},
	}, out, time.Hour)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh with one bad feed should still succeed if any feed works: %v", err)
	}
	if v, _ := c.Scan(context.Background(), []byte("good-payload")); v.OK || v.Threat != "GoodFeed" {
		t.Errorf("good payload verdict = %+v, want hit on GoodFeed", v)
	}
	written, _ := os.ReadFile(out)
	if !contains(string(written), "BadFeed") || !contains(string(written), "FAILED") {
		t.Errorf("sigdb header should record the failed feed:\n%s", written)
	}
}

// TestUpdaterRefreshFailsWhenAllFeedsFail — if every feed fails the
// refresh aborts and leaves the previous sigdb intact.
func TestUpdaterRefreshFailsWhenAllFeedsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, []Feed{
		{URL: srv.URL, Source: "A"},
		{URL: srv.URL, Source: "B"},
	}, out, time.Hour)
	if err := u.refresh(context.Background()); err == nil {
		t.Fatal("expected error when every feed fails")
	}
	if got := c.SignatureCount(); got != 1 {
		t.Errorf("signatures = %d, want 1 (builtin EICAR intact)", got)
	}
}

// TestUpdaterParsesZipFeed — the MalwareBazaar full export ships as
// a ZIP containing a single plain-text hash file. The parser must
// detect the PK magic, unzip in memory, and extract hashes from
// every entry inside.
func TestUpdaterParsesZipFeed(t *testing.T) {
	hash := hashOf("zipped-payload")
	zipped := makeZip(t, "full_sha256.txt", "# header\n"+hash+"\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipped)
	}))
	defer srv.Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, []Feed{{URL: srv.URL, Source: "ZipFeed"}}, out, time.Hour)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if v, _ := c.Scan(context.Background(), []byte("zipped-payload")); v.OK || v.Threat != "ZipFeed" {
		t.Errorf("zipped payload verdict = %+v, want hit on ZipFeed", v)
	}
}

// TestUpdaterParsesCSVFeed — ThreatFox's full export wraps a CSV in a
// ZIP. Each data row is quoted-comma columns with the SHA-256 hash
// sitting in the third column. The line parser extracts the hash
// via the 64-hex-token regex regardless of surrounding columns.
func TestUpdaterParsesCSVFeed(t *testing.T) {
	hash := hashOf("csv-payload")
	csv := fmt.Sprintf(`################################################################
# ThreatFox IOCs: SHA256 hashes - CSV format (full dump)       #
################################################################
"2021-08-20 12:00:30", "192447", "%s", "sha256_hash", "payload", "win.cryptbot"
# Number of entries: 1
`, hash)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(csv))
	}))
	defer srv.Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, []Feed{{URL: srv.URL, Source: "ThreatFox"}}, out, time.Hour)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if v, _ := c.Scan(context.Background(), []byte("csv-payload")); v.OK || v.Threat != "ThreatFox" {
		t.Errorf("csv payload verdict = %+v, want hit on ThreatFox", v)
	}
}

// TestUpdaterDeterministicSigdb — back-to-back refreshes that pull
// the same upstream contents must produce a byte-identical sigdb
// file. Determinism keeps the volume diff small for rsync/backup
// and lets operators compare files across hosts.
func TestUpdaterDeterministicSigdb(t *testing.T) {
	body := hashOf("a") + "\n" + hashOf("b") + "\n" + hashOf("c") + "\n"
	feed := newFakeFeed(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "auto.sigdb")
	c, _ := New(out)
	u := NewUpdater(c, []Feed{{URL: feed.server.URL, Source: "F"}}, out, time.Hour)

	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	first, _ := os.ReadFile(out)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	second, _ := os.ReadFile(out)
	// Strip the `# fetched <timestamp>` line which legitimately changes.
	if stripFetchedHeader(string(first)) != stripFetchedHeader(string(second)) {
		t.Errorf("sigdb non-deterministic across refreshes:\nFIRST:\n%s\nSECOND:\n%s", first, second)
	}
}

func makeZip(t *testing.T, entryName, entryBody string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entryName)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte(entryBody)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func stripFetchedHeader(s string) string {
	out := ""
	for _, line := range bytesSplit(s, "\n") {
		if len(line) >= len("# fetched") && line[:len("# fetched")] == "# fetched" {
			continue
		}
		out += line + "\n"
	}
	return out
}

func bytesSplit(s, sep string) []string {
	var out []string
	for {
		i := -1
		for j := 0; j+len(sep) <= len(s); j++ {
			if s[j:j+len(sep)] == sep {
				i = j
				break
			}
		}
		if i < 0 {
			out = append(out, s)
			return out
		}
		out = append(out, s[:i])
		s = s[i+len(sep):]
	}
}
