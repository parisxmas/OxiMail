// Package config holds OxiMail's runtime configuration, loaded from the
// environment. Every field has a working default so the server boots
// with zero configuration for local development.
package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"strconv"
	"strings"

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
//   - Neither: TLS is disabled. STARTTLS is not advertised and the
//     implicit-TLS listeners are not started.
//
// On error, both returned values are nil. On the static-cert and
// disabled paths the autocert.Manager is also nil; callers can check
// for nil to decide whether to start the challenge listener.
func (c Config) TLSConfig() (*tls.Config, *autocert.Manager, error) {
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
		return cfg, m, nil
	}
	if c.TLSCert == "" && c.TLSKey == "" {
		return nil, nil, nil
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		return nil, nil, fmt.Errorf("config: OXIMAIL_TLS_CERT and OXIMAIL_TLS_KEY must be set together")
	}
	cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
	if err != nil {
		return nil, nil, fmt.Errorf("config: load TLS keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil, nil
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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
