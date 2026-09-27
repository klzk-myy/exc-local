package nats

import (
	"testing"
	"time"
)

func TestSubject(t *testing.T) {
	got, err := Subject("trades", 0, "EUR-USD")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if want := "trades.0.EUR-USD"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
}

func TestSubjectRejectsBadTokens(t *testing.T) {
	cases := []struct {
		stream, symbol string
	}{
		{"", "EUR-USD"},
		{"trades", ""},
		{"tra.des", "EUR-USD"}, // dot would split into extra subject tokens
		{"trades", "EUR.USD"},  // dots in symbols break per-subject ordering
		{"trades", "EUR USD"},  // space is illegal in subjects
		{"trades", "*"},        // wildcards must never reach publish path
		{"trades.>", "EUR-USD"},
	}
	for _, tc := range cases {
		if _, err := Subject(tc.stream, 0, tc.symbol); err == nil {
			t.Errorf("Subject(%q, 0, %q) = nil error, want rejection", tc.stream, tc.symbol)
		}
	}
}

func TestStreamConfigCanonical(t *testing.T) {
	if len(Streams) != 7 {
		t.Fatalf("expected 7 canonical streams, got %d: %v", len(Streams), Streams)
	}
	for _, name := range Streams {
		cfg := streamConfig(name)
		if cfg.Name != name {
			t.Errorf("%s: Name = %q", name, cfg.Name)
		}
		if len(cfg.Subjects) != 1 || cfg.Subjects[0] != name+".>" {
			t.Errorf("%s: Subjects = %v, want [%s.>]", name, cfg.Subjects, name)
		}
		if cfg.Replicas != 3 {
			t.Errorf("%s: Replicas = %d, want 3", name, cfg.Replicas)
		}
		if cfg.MaxAge != 7*24*time.Hour {
			t.Errorf("%s: MaxAge = %v, want 168h", name, cfg.MaxAge)
		}
		if cfg.Retention.String() != "WorkQueue" {
			t.Errorf("%s: Retention = %s, want WorkQueue", name, cfg.Retention)
		}
		if cfg.Storage.String() != "File" {
			t.Errorf("%s: Storage = %s, want File", name, cfg.Storage)
		}
	}
}
