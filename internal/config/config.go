// Package config holds OxiMail's runtime configuration, loaded from the
// environment. Every field has a working default so the server boots
// with zero configuration for local development.
package config

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Config is the fully-resolved configuration for one server process.
type Config struct {
	// Hostname announced in SMTP greetings / EHLO and used for loops checks.
	Hostname string

	// SMTPAddr — inbound mail (MX), conventionally port 25.
	SMTPAddr string
	// SubmissionAddr — authenticated outbound submission, conventionally 587.
	SubmissionAddr string
	// IMAPAddr — mailbox access, conventionally port 143.
	IMAPAddr string
	// SMTPSAddr / IMAPSAddr — implicit-TLS listeners, conventionally
	// ports 465 and 993. Started only when a TLS certificate is set.
	SMTPSAddr string
	IMAPSAddr string
	// WebmailAddr — the webmail HTTP+JSON API. Serves HTTPS when a TLS
	// certificate is configured, plain HTTP otherwise.
	WebmailAddr string
	// WebmailStatic, if set, is a directory of built frontend assets
	// (the Angular SPA) that the webmail server also serves, with an
	// index.html fallback for client-side routing. Empty = API only.
	WebmailStatic string

	// MetricsAddr — the observability HTTP port. It exposes Prometheus
	// metrics at /metrics plus /healthz (liveness) and /readyz
	// (readiness — checks the store).
	MetricsAddr string

	// LogFormat — slog output: "text" (default) or "json".
	LogFormat string
	// LogLevel — slog minimum level: debug | info | warn | error.
	LogLevel string

	// TLSCert / TLSKey — PEM file paths for STARTTLS and implicit TLS.
	// When unset, TLS is disabled: STARTTLS is not advertised and the
	// implicit-TLS listeners are not started. Ignored when ACMEHosts
	// is configured.
	TLSCert string
	TLSKey  string

	// ACMEHosts is a comma-separated list of hostnames to obtain Let's
	// Encrypt certificates for. When non-empty, the static TLSCert /
	// TLSKey are ignored and TLSConfig instead returns an autocert-
	// backed configuration. ACMEChallengeAddr must then be reachable
	// on the public internet for HTTP-01 validation.
	ACMEHosts []string
	// ACMECache is the on-disk directory where autocert stores the
	// fetched certificate, private key, and ACME account material so
	// they survive a restart. Defaults to ./acme-cache.
	ACMECache string
	// ACMEEmail is the contact address for the ACME account. Optional;
	// Let's Encrypt sends expiry warnings here.
	ACMEEmail string
	// ACMEDirectoryURL overrides the ACME endpoint. Useful for
	// pointing at the Let's Encrypt staging environment during testing
	// (`https://acme-staging-v02.api.letsencrypt.org/directory`); when
	// empty, autocert uses the production directory.
	ACMEDirectoryURL string
	// ACMEChallengeAddr is the HTTP-01 listener address — Let's
	// Encrypt's validator dials it on port 80 to fetch the challenge
	// token. Defaults to ":80".
	ACMEChallengeAddr string

	// OxiDB — the backing store (collections + blob store + OxiMem).
	OxiDBHost string
	OxiDBPort int

	// RspamdURL — content spam scanning over HTTP. Empty disables it.
	RspamdURL string

	// AVSocket is the Unix-socket path to a clamd-protocol antivirus
	// daemon. Webmail compose dials it to scan outbound attachments
	// before relay (Phase B in the AV plan). Empty disables the
	// integration; rspamd's antivirus module (Phase A) is the
	// inbound counterpart and lives at a different control surface.
	AVSocket string
	// AVRequired, when true, treats an unreachable AV daemon as a
	// send-blocking failure: the webmail handler returns 502 and
	// the attachment doesn't go out. When false (the default) an
	// AV outage logs a warning but mail still ships — the
	// receiver's AV catches anything we miss.
	AVRequired bool

	// DNSBLZones is the comma-separated list of DNS blocklist zones
	// queried at connection-time (see internal/spam/dnsbl.go). The
	// default — "zen.spamhaus.org" — only works when OxiMail resolves
	// it through a private/registered DNS path; Spamhaus refuses
	// queries that go via public resolvers (1.1.1.1, 8.8.8.8, …) and
	// returns a diagnostic 127.255.255.x reply that the checker treats
	// as "unable to classify". Set to empty to disable DNSBL entirely.
	DNSBLZones []string

	// GreylistDelay is how long a (sender-IP-/24, MAIL FROM) tuple
	// must wait between its first delivery attempt and the retry that
	// gets accepted. Default 1 minute. Set to 0 to disable greylisting
	// entirely — a reasonable choice for low-volume personal servers
	// where the spam-mitigation benefit is small compared to the
	// rejection cost when a sender (e.g. Gmail) rotates outbound IPs
	// across /24s and each retry triggers a fresh greylist.
	GreylistDelay time.Duration

	// MTASTSMode publishes an MTA-STS policy (RFC 8461) when set.
	// Valid values: "enforce", "testing", "none". An empty value
	// disables the /.well-known/mta-sts.txt handler.
	MTASTSMode string
	// MTASTSMX is the comma-separated list of MX hostname patterns
	// included in the published policy. Defaults to Hostname when
	// empty. Wildcards like "*.example.com" are honored.
	MTASTSMX []string
	// MTASTSMaxAge is how long remote senders may cache the policy.
	// Defaults to 86400 (24h) per the spec's recommended minimum.
	MTASTSMaxAge time.Duration

	// SRSSecret is the hex-encoded HMAC key used to sign rewritten
	// envelope senders for alias forwarding to remote addresses. When
	// unset, aliases that point off-server are not relayed (the MX
	// returns 550 for them). Must be at least 16 raw bytes (32 hex
	// characters); regenerating it invalidates every outstanding
	// bounce address.
	SRSSecret string
	// SRSMaxAge is how long an SRS-rewritten address stays valid for
	// inbound bounce delivery. Default 21 days; set to 0 to skip the
	// age check entirely.
	SRSMaxAge time.Duration
}

// Load reads the configuration from OXIMAIL_* environment variables,
// applying defaults for anything unset.
func Load() Config {
	return Config{
		Hostname:       env("OXIMAIL_HOSTNAME", "localhost"),
		SMTPAddr:       env("OXIMAIL_SMTP_ADDR", ":25"),
		SubmissionAddr: env("OXIMAIL_SUBMISSION_ADDR", ":587"),
		IMAPAddr:       env("OXIMAIL_IMAP_ADDR", ":143"),
		SMTPSAddr:      env("OXIMAIL_SMTPS_ADDR", ":465"),
		IMAPSAddr:      env("OXIMAIL_IMAPS_ADDR", ":993"),
		WebmailAddr:    env("OXIMAIL_WEBMAIL_ADDR", ":8080"),
		WebmailStatic:  env("OXIMAIL_WEBMAIL_STATIC", ""),
		MetricsAddr:    env("OXIMAIL_METRICS_ADDR", ":9090"),
		LogFormat:      env("OXIMAIL_LOG_FORMAT", "text"),
		LogLevel:       env("OXIMAIL_LOG_LEVEL", "info"),
		TLSCert:        env("OXIMAIL_TLS_CERT", ""),
		TLSKey:         env("OXIMAIL_TLS_KEY", ""),
		ACMEHosts:      splitCSV(env("OXIMAIL_ACME_HOSTS", "")),
		ACMECache:      env("OXIMAIL_ACME_CACHE", "./acme-cache"),
		ACMEEmail:      env("OXIMAIL_ACME_EMAIL", ""),
		ACMEDirectoryURL: env("OXIMAIL_ACME_DIRECTORY_URL", ""),
		ACMEChallengeAddr: env("OXIMAIL_ACME_CHALLENGE_ADDR", ":80"),
		OxiDBHost:      env("OXIMAIL_OXIDB_HOST", "127.0.0.1"),
		OxiDBPort:      envInt("OXIMAIL_OXIDB_PORT", 4444),
		RspamdURL:      env("OXIMAIL_RSPAMD_URL", ""),
		AVSocket:       env("OXIMAIL_AV_SOCKET", ""),
		AVRequired:     envBool("OXIMAIL_AV_REQUIRED"),
		DNSBLZones:     splitCSV(envOrDefault("OXIMAIL_DNSBL_ZONES", "zen.spamhaus.org")),
		GreylistDelay:  envDuration("OXIMAIL_GREYLIST_DELAY", time.Minute),
		SRSSecret:      env("OXIMAIL_SRS_SECRET", ""),
		SRSMaxAge:      envDuration("OXIMAIL_SRS_MAX_AGE", 21*24*time.Hour),
		MTASTSMode:     env("OXIMAIL_MTASTS_MODE", ""),
		MTASTSMX:       splitCSV(env("OXIMAIL_MTASTS_MX", "")),
		MTASTSMaxAge:   envDuration("OXIMAIL_MTASTS_MAX_AGE", 86400*time.Second),
	}
}

// TLSConfig builds the server TLS configuration. There are three
// modes:
//
//   - ACMEHosts non-empty: TLS comes from Let's Encrypt (or the
//     configured ACME directory) via autocert. The returned *tls.Config
//     carries a GetCertificate callback; renewals are automatic. The
//     returned autocert.Manager owns the HTTP-01 challenge handler the
//     caller must serve on ACMEChallengeAddr.
//   - TLSCert + TLSKey set: TLS comes from those static PEM files.
//     The returned *TLSReloader can be used to hot-reload the cert on
//     SIGHUP (or after `certbot renew`) without restarting the
//     process — listeners obtain certs through GetCertificate, so a
//     reload takes effect on the next handshake.
//   - Neither: TLS is disabled. STARTTLS is not advertised and the
//     implicit-TLS listeners are not started.
//
// On error, all three returned values are nil. ACME mode and the
// disabled mode return a nil *TLSReloader; static-cert mode returns a
// nil *autocert.Manager.
func (c Config) TLSConfig() (*tls.Config, *autocert.Manager, *TLSReloader, error) {
	if len(c.ACMEHosts) > 0 {
		m := &autocert.Manager{
			Cache:      autocert.DirCache(c.ACMECache),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(c.ACMEHosts...),
			Email:      c.ACMEEmail,
		}
		if c.ACMEDirectoryURL != "" {
			m.Client = &acme.Client{DirectoryURL: c.ACMEDirectoryURL}
		}
		cfg := m.TLSConfig()
		cfg.MinVersion = tls.VersionTLS12
		return cfg, m, nil, nil
	}
	if c.TLSCert == "" && c.TLSKey == "" {
		return nil, nil, nil, nil
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		return nil, nil, nil, fmt.Errorf("config: OXIMAIL_TLS_CERT and OXIMAIL_TLS_KEY must be set together")
	}
	reloader, err := NewTLSReloader(c.TLSCert, c.TLSKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("config: load TLS keypair: %w", err)
	}
	return &tls.Config{
		GetCertificate: reloader.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}, nil, reloader, nil
}

// SRSSecretBytes returns the SRS HMAC key as raw bytes, or nil + nil
// when no secret is configured. A configured-but-malformed secret
// (non-hex or < 16 bytes) is an error so the operator is told
// loudly.
func (c Config) SRSSecretBytes() ([]byte, error) {
	if c.SRSSecret == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(c.SRSSecret)
	if err != nil {
		return nil, fmt.Errorf("config: OXIMAIL_SRS_SECRET is not valid hex: %w", err)
	}
	if len(b) < 16 {
		return nil, fmt.Errorf("config: OXIMAIL_SRS_SECRET must be at least 16 bytes (32 hex chars); got %d", len(b))
	}
	return b, nil
}

// envDuration reads a time.Duration env var, applying def when unset
// or malformed.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// splitCSV parses a comma-separated env value into a trimmed,
// non-empty slice. An empty input yields a nil slice.
func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reads an env var as a boolean. "true" / "1" / "yes" / "on"
// (case-insensitive) → true; anything else, including unset, → false.
// We deliberately don't error on a typo: a stray "treu" defaulting to
// false is the correct conservative reading.
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// envOrDefault is like env, but distinguishes "unset" from "set to the
// empty string". An explicit empty value wins over def — used for
// settings where setting the env var to "" is the operator's way of
// disabling a feature whose default is non-empty.
func envOrDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
