// Package config holds OxiMail's runtime configuration, loaded from the
// environment. Every field has a working default so the server boots
// with zero configuration for local development.
package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"strconv"
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
	// implicit-TLS listeners are not started.
	TLSCert string
	TLSKey  string

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
		OxiDBHost:      env("OXIMAIL_OXIDB_HOST", "127.0.0.1"),
		OxiDBPort:      envInt("OXIMAIL_OXIDB_PORT", 4444),
		RspamdURL:      env("OXIMAIL_RSPAMD_URL", ""),
	}
}

// TLSConfig builds the server TLS configuration from the configured
// certificate and key. It returns (nil, nil) when neither is set — TLS
// is simply disabled — and an error only when one is set without the
// other, or the files will not load.
func (c Config) TLSConfig() (*tls.Config, error) {
	if c.TLSCert == "" && c.TLSKey == "" {
		return nil, nil
	}
	if c.TLSCert == "" || c.TLSKey == "" {
		return nil, fmt.Errorf("config: OXIMAIL_TLS_CERT and OXIMAIL_TLS_KEY must be set together")
	}
	cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("config: load TLS keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
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
