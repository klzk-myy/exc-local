// Package logging provides the exchange-wide structured logger.
//
// All services emit log/slog records to stdout — JSON in normal operation,
// text for local debugging — with the level set from config.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// ParseLevel converts a config string to a slog.Level.
// Accepts debug|info|warn|error (case-insensitive; "warning" tolerated).
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level %q (debug|info|warn|error)", s)
	}
}

// New returns a slog.Logger writing to stdout in the given format
// ("json" or "text") at or above level. Config validation (config.Validate)
// already guarantees both arguments, so callers may treat an error here as
// a programming error.
func New(level slog.Level, format string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (json|text)", format)
	}
}
