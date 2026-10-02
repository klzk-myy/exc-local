package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn, // tolerated alias
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Fatalf("ParseLevel(%q) = %v, %v — want %v, nil", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "verbose", "0"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Fatalf("ParseLevel(%q) succeeded, want error", bad)
		}
	}
}
