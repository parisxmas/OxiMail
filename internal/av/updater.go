package av

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Updater periodically fetches a remote feed of SHA-256 malware
// hashes, writes them to a sigdb file under the format
// internal/av understands (`<sha256>:<name>`), and asks the Client
// to reload so the fresh hashes start matching immediately.
//
// The default feed is abuse.ch MalwareBazaar's recent SHA-256 list
// (about 100 entries — the most recently uploaded samples world-
// wide). On every refresh we replace the file rather than merge:
// stale entries fall off naturally, the file stays small, and a
// poisoned tick is self-healing (next tick overwrites).
//
// Failure mode: a refresh that fails (network, HTTP non-2xx, parse
// error) logs and moves on to the next tick. The Client keeps
// serving the last good map — fail-open at the updater layer.
type Updater struct {
	client   *Client
	url      string
	out      string // sigdb file to write
	source   string // human-readable label for entries (column 2 of sigdb)
	interval time.Duration
	http     *http.Client
}

// Defaults pull abuse.ch's open SHA-256 feed every 6 hours.
const (
	DefaultUpdateURL      = "https://bazaar.abuse.ch/export/txt/sha256/recent/"
	DefaultUpdateInterval = 6 * time.Hour
	DefaultUpdateSource   = "MalwareBazaar"
)

// NewUpdater builds an Updater. An empty url or sigdb path returns
// nil — the caller's `if upd != nil { go upd.Run(ctx) }` idiom
// keeps the wiring branchless.
func NewUpdater(c *Client, url, sigdbOut, source string, interval time.Duration) *Updater {
	if c == nil || url == "" || sigdbOut == "" {
		return nil
	}
	if interval <= 0 {
		interval = DefaultUpdateInterval
	}
	if source == "" {
		source = DefaultUpdateSource
	}
	return &Updater{
		client:   c,
		url:      url,
		out:      sigdbOut,
		source:   source,
		interval: interval,
		// 30s timeout — the MalwareBazaar list is ~50 KB so a
		// short cap stops a stuck connection from holding the
		// goroutine indefinitely. Total budget per tick is bounded
		// by this plus the parse + write + reload cycle (<1s).
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Run drives the refresh loop until ctx is done. Fires one refresh
// immediately on entry so a freshly-started daemon doesn't wait an
// entire interval for first hashes. Returns ctx.Err on shutdown.
func (u *Updater) Run(ctx context.Context) error {
	if err := u.refresh(ctx); err != nil {
		log.Printf("av-updater: initial refresh from %s failed: %v", u.url, err)
	}
	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := u.refresh(ctx); err != nil {
				log.Printf("av-updater: refresh from %s failed: %v", u.url, err)
			}
		}
	}
}

// refresh runs the fetch → parse → write → reload cycle once. The
// file write is atomic (write-to-tmp + rename) so the Client's
// next Reload either sees the full new content or the previous
// content; never a half-written file.
func (u *Updater) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return err
	}
	// abuse.ch is fine with anonymous fetches of these files but a
	// descriptive UA lets them rate-limit politely instead of
	// blackholing — we identify as OxiMail's updater.
	req.Header.Set("User-Agent", "OxiMail-AV-Updater (oximail; abuse-ch-recent)")
	resp, err := u.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, u.url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	// Build the sigdb file. The feed format is one SHA-256 hex
	// string per line (64 lower-case hex chars), with `#` comment
	// lines and blanks. We tolerate uppercase too — that's still
	// valid hex; loadInto in av.go normalises via hex.DecodeString.
	var sb strings.Builder
	sb.WriteString("# OxiMail av-updater — auto-managed sigdb, DO NOT EDIT.\n")
	fmt.Fprintf(&sb, "# fetched %s from %s\n", time.Now().UTC().Format(time.RFC3339), u.url)
	count := 0
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Hash + size variant: some feeds emit `<sha256>:<size>`.
		// Strip the size for our pure-hash format.
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			line = line[:idx]
		}
		if len(line) != 64 {
			continue
		}
		if _, err := hex.DecodeString(line); err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%s:%s\n", strings.ToLower(line), u.source)
		count++
	}

	if err := writeAtomic(u.out, []byte(sb.String())); err != nil {
		return fmt.Errorf("write sigdb: %w", err)
	}
	if err := u.client.Reload(); err != nil {
		return fmt.Errorf("client reload: %w", err)
	}
	log.Printf("av-updater: refreshed — %d hashes from %s, total active signatures=%d",
		count, u.url, u.client.SignatureCount())
	return nil
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
