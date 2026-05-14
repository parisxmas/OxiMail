// Package config holds OxiMail's runtime configuration, loaded from the
// environment. Every field has a working default so the server boots
// with zero configuration for local development.
package config

import (
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
		OxiDBHost:      env("OXIMAIL_OXIDB_HOST", "127.0.0.1"),
		OxiDBPort:      envInt("OXIMAIL_OXIDB_PORT", 4444),
		RspamdURL:      env("OXIMAIL_RSPAMD_URL", ""),
	}
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
