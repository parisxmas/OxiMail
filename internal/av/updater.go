package av

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"time"
)

// Feed identifies one upstream hash source. The updater fetches `URL`
// and labels every imported hash with `Source` — that label shows up
// as the signature name in scan verdicts (so a webmail attachment
// blocked by a ThreatFox match logs "Threat: ThreatFox" rather than
// the bare hash).
type Feed struct {
	URL    string
	Source string
}

// Updater periodically fetches a list of remote SHA-256 hash feeds,
// merges them into a single sigdb file under the format
// internal/av understands (`<sha256>:<name>` per line), and triggers a
// hot reload so fresh hashes start matching immediately.
//
// Multiple feeds let one deploy combine breadth (MalwareBazaar's full
// ~1M-entry dump) with curated IoC lists (ThreatFox SHA-256 IoCs).
// Each feed is fetched independently; one feed's failure is logged but
// does not abort the others — the sigdb is rewritten from whichever
// feeds succeeded this tick. A tick that fetches zero hashes total
// leaves the previous sigdb untouched (fail-open at the file layer).
//
// Both plain-text feeds (one hash per line) and ZIP archives are
// supported; ZIPs are decoded in memory and every entry inside is
// parsed with the same line-by-line regex that extracts the first
// 64-char hex token. The two-shape detection plus the regex covers
// the formats abuse.ch publishes today (plain `recent/` lists, ZIPped
// `full/` dumps, ThreatFox's CSV-in-ZIP), so adding a new feed URL
// usually needs no parser change.
type Updater struct {
	client   *Client
	feeds    []Feed
	out      string // sigdb file written atomically on every refresh
	interval time.Duration
	http     *http.Client
}

const (
	// DefaultUpdateInterval is the cadence between refresh ticks when
	// the operator hasn't pinned one. Six hours is well inside abuse.ch's
	// polite-fetch guidance and keeps the recent-uploads window fresh.
	DefaultUpdateInterval = 6 * time.Hour

	// updateBodyCap is the upper bound on a single feed body. The
	// MalwareBazaar full SHA-256 ZIP sits around 40 MB; we leave
	// generous headroom so a feed that grows organically doesn't
	// start silently truncating.
	updateBodyCap = 256 * 1024 * 1024
)

// NewUpdater builds an Updater. Returns nil when there's nothing to do
// — no client, no output path, or no non-empty feed — so the caller's
// `if upd != nil { go upd.Run(ctx) }` idiom keeps the wiring branchless.
//
// A feed with an empty URL is skipped; an empty Source falls back to
// the URL string itself so the sigdb still parses (loadInto requires
// a non-empty name after the colon).
func NewUpdater(c *Client, feeds []Feed, sigdbOut string, interval time.Duration) *Updater {
	if c == nil || sigdbOut == "" {
		return nil
	}
	feeds = pruneFeeds(feeds)
	if len(feeds) == 0 {
		return nil
	}
	if interval <= 0 {
		interval = DefaultUpdateInterval
	}
	return &Updater{
		client:   c,
		feeds:    feeds,
		out:      sigdbOut,
		interval: interval,
		// 2-minute per-feed timeout — the full MalwareBazaar ZIP is
		// ~40 MB and can take 20-30s on a slow connection. Aborting
		// at 30s would flap on any non-trivial backbone latency.
		http: &http.Client{Timeout: 2 * time.Minute},
	}
}

// Feeds returns a copy of the configured feed list — handed to the
// startup log so operators see exactly which sources are wired up.
func (u *Updater) Feeds() []Feed {
	out := make([]Feed, len(u.feeds))
	copy(out, u.feeds)
	return out
}

func pruneFeeds(in []Feed) []Feed {
	out := make([]Feed, 0, len(in))
	for _, f := range in {
		if f.URL == "" {
			continue
		}
		if f.Source == "" {
			f.Source = f.URL
		}
		out = append(out, f)
	}
	return out
}

// Run drives the refresh loop until ctx is done. Fires once immediately
// so a freshly-started daemon doesn't wait an entire interval for
// first hashes.
func (u *Updater) Run(ctx context.Context) error {
	if err := u.refresh(ctx); err != nil {
		log.Printf("av-updater: initial refresh failed: %v", err)
	}
	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := u.refresh(ctx); err != nil {
				log.Printf("av-updater: refresh failed: %v", err)
			}
		}
	}
}

// refresh runs one fetch → parse → write → reload cycle across every
// configured feed. Feeds run sequentially (the abuse.ch endpoints
// share a host, so a parallel fan-out wouldn't help and would risk
// being rate-limited); per-feed failures are logged and recorded in
// the sigdb header but don't abort the run as long as at least one
// feed contributed hashes.
func (u *Updater) refresh(ctx context.Context) error {
	results := make([]feedResult, 0, len(u.feeds))
	totalUnique := make(map[[32]byte]string) // first-seen source wins on duplicates
	for _, f := range u.feeds {
		hashes, err := u.fetchAndParse(ctx, f.URL)
		if err != nil {
			log.Printf("av-updater: feed %q (%s) failed: %v", f.Source, f.URL, err)
			results = append(results, feedResult{feed: f, err: err})
			continue
		}
		log.Printf("av-updater: feed %q (%s) → %d hashes", f.Source, f.URL, len(hashes))
		results = append(results, feedResult{feed: f, hashes: hashes})
		for h := range hashes {
			if _, seen := totalUnique[h]; !seen {
				totalUnique[h] = f.Source
			}
		}
	}
	if len(totalUnique) == 0 {
		return fmt.Errorf("no hashes fetched (all %d feeds failed or empty)", len(u.feeds))
	}

	body := buildSigdb(totalUnique, results)
	if err := writeAtomic(u.out, body); err != nil {
		return fmt.Errorf("write sigdb: %w", err)
	}
	if err := u.client.Reload(); err != nil {
		return fmt.Errorf("client reload: %w", err)
	}

	// The refresh + Reload cycle peaks heap with several large
	// transients (per-feed hash sets, the merged unique map, the
	// serialised sigdb buffer, the file-read buffer that loadInto
	// then parses). After Reload returns, none of those are
	// referenced — but Go's heap retains the arenas it grew, and
	// MADV_FREE on Linux leaves the pages mapped as RSS until
	// memory pressure. A one-off GC + scavenger pass right here
	// trims the heap back to the live AV map (~100 MB for ~1M
	// entries) instead of leaving the daemon at the parse-peak
	// 600+ MB for hours between refreshes.
	runtime.GC()
	debug.FreeOSMemory()

	log.Printf("av-updater: refreshed — %d unique hashes across %d feed(s), total active signatures=%d",
		len(totalUnique), len(u.feeds), u.client.SignatureCount())
	return nil
}

// buildSigdb serializes the merged feed contents into the
// `<sha256>:<name>` line format that internal/av.loadInto reads. A
// short header captures the fetch timestamp + per-feed counts (or
// per-feed failures) so an operator opening the file sees provenance
// at a glance without trawling logs.
//
// We sort the hash lines for determinism: a refresh that pulled the
// same upstream contents twice in a row produces a byte-identical
// file, which makes diffs (and rsync of the volume) cheap.
func buildSigdb(totalUnique map[[32]byte]string, results []feedResult) []byte {
	var buf bytes.Buffer
	buf.Grow(80 * len(totalUnique))
	buf.WriteString("# OxiMail av-updater — auto-managed sigdb, DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "# fetched %s\n", time.Now().UTC().Format(time.RFC3339))
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(&buf, "# feed %q (%s) FAILED: %v\n", r.feed.Source, r.feed.URL, r.err)
			continue
		}
		fmt.Fprintf(&buf, "# feed %q (%s) — %d hashes\n", r.feed.Source, r.feed.URL, len(r.hashes))
	}
	fmt.Fprintf(&buf, "# total unique hashes: %d\n", len(totalUnique))

	hashes := make([][32]byte, 0, len(totalUnique))
	for h := range totalUnique {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })

	var hexbuf [64]byte
	for _, h := range hashes {
		hex.Encode(hexbuf[:], h[:])
		buf.Write(hexbuf[:])
		buf.WriteByte(':')
		buf.WriteString(totalUnique[h])
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// feedResult is the per-feed outcome bundle that drives the sigdb
// header lines. Hoisted out of refresh() so buildSigdb's signature
// reads as straightforward data-in, bytes-out.
type feedResult struct {
	feed   Feed
	hashes map[[32]byte]struct{}
	err    error
}

func (u *Updater) fetchAndParse(ctx context.Context, url string) (map[[32]byte]struct{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// abuse.ch accepts anonymous fetches but a descriptive UA lets
	// them rate-limit politely instead of blackholing.
	req.Header.Set("User-Agent", "OxiMail-AV-Updater (oximail)")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, updateBodyCap))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return parseFeedBody(body)
}

// parseFeedBody picks the right parser by sniffing the leading magic:
// `PK\x03\x04` is the ZIP local-file header, anything else is treated
// as plain text. ZIPs are walked entry-by-entry and each entry is
// reduced to a hash set via the same line parser.
func parseFeedBody(body []byte) (map[[32]byte]struct{}, error) {
	if bytes.HasPrefix(body, []byte("PK\x03\x04")) {
		return parseZipFeed(body)
	}
	return parsePlainFeed(body), nil
}

func parseZipFeed(body []byte) (map[[32]byte]struct{}, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	out := make(map[[32]byte]struct{})
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("zip entry %s: %w", zf.Name, err)
		}
		// Per-entry cap matches the outer body cap — the full
		// MalwareBazaar list expands to ~72 MB plain text today,
		// 256 MiB leaves room to grow without ever truncating.
		contents, readErr := io.ReadAll(io.LimitReader(rc, updateBodyCap))
		_ = rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("zip entry %s read: %w", zf.Name, readErr)
		}
		for h := range parsePlainFeed(contents) {
			out[h] = struct{}{}
		}
	}
	return out, nil
}

// hash64re matches any 64-character hex token. Run as `Find` against
// each non-comment line; the first match wins. This single rule
// handles both plain-line feeds (the token IS the line) and CSV
// feeds (the token is one of several quoted columns), so adding a new
// abuse.ch endpoint usually needs no parser update.
var hash64re = regexp.MustCompile(`[0-9a-fA-F]{64}`)

func parsePlainFeed(body []byte) map[[32]byte]struct{} {
	out := make(map[[32]byte]struct{}, 1024)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		m := hash64re.Find(line)
		if m == nil {
			continue
		}
		var key [32]byte
		if _, err := hex.Decode(key[:], bytes.ToLower(m)); err != nil {
			continue
		}
		out[key] = struct{}{}
	}
	return out
}

// writeAtomic writes contents to path via a sibling temp file + rename.
// Same-filesystem rename is atomic on POSIX, so a concurrent reader
// of `path` (the Client's Reload) sees either the old contents or
// the new contents — never a half-written file.
func writeAtomic(path string, contents []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, contents, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
