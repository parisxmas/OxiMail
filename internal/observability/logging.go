// Package observability is OxiMail's instrumentation glue: structured
// logging via log/slog, Prometheus metrics, and HTTP health probes.
//
// Logging — SetupLogging installs slog as the default logger. Since
// Go 1.21 slog.SetDefault also routes the stdlib log package through
// slog, so existing log.Print calls become structured records too.
package observability

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

// SetupLogging installs slog as the default with the given output
// format and minimum level.
//
//	format: "json" for machine-readable, anything else for text.
//	level:  "debug" | "info" | "warn" | "error" (unrecognised: info).
//
// It also clears the stdlib log package's prefix and flags so legacy
// log.Print calls, which now flow through slog's handler, don't carry
// duplicate formatting.
func SetupLogging(format, level string) {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
	log.SetFlags(0)
	log.SetPrefix("")
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
