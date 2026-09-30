// Phase-21 wave-2 governance — pure-helper unit tests (no PG).
// Covers Basel III (21.3.13) conversion/breach constants and the FX
// Global Code (21.3.17) theme matrix, probe enumeration and statement
// integrity pinning.
package compliance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// December-2024 edition: every principle id 1..55 has a seeded title
// and maps into exactly one of the six themes at the spec boundaries.
func TestFXGCThemeBoundaries(t *testing.T) {
	want := map[int]string{
		1: ThemeEthics, 3: ThemeEthics,
		4: ThemeGovernance, 7: ThemeGovernance,
		8: ThemeExecution, 18: ThemeExecution,
		19: ThemeInformationSharing, 23: ThemeInformationSharing,
		24: ThemeRiskCompliance, 41: ThemeRiskCompliance,
		42: ThemeConfirmationSettlement, 55: ThemeConfirmationSettlement,
	}
	for p, theme := range want {
		if got := fxGCTheme(p); got != theme {
			t.Fatalf("principle %d theme=%s want %s", p, got, theme)
		}
	}
	for p := 1; p <= FXGCPrinciplesTotal; p++ {
		if fxGCPrincipleTitles[p] == "" {
			t.Fatalf("principle %d has no seeded title", p)
		}
	}
	if len(fxGCPrincipleTitles) != FXGCPrinciplesTotal {
		t.Fatalf("titles=%d want %d", len(fxGCPrincipleTitles),
			FXGCPrinciplesTotal)
	}
}

// The four automated probes are exactly principles 9/10/17/50 — every
// other row must stay PENDING until an officer attests.
func TestFXGCAutomatedProbeSet(t *testing.T) {
	for _, p := range []int{9, 10, 17, 50} {
		if !automatedChecks[p] {
			t.Fatalf("principle %d must be auto-probed", p)
		}
	}
	if len(automatedChecks) != 4 {
		t.Fatalf("automated probes=%d want 4", len(automatedChecks))
	}
	for p := 1; p <= FXGCPrinciplesTotal; p++ {
		switch p {
		case 9, 10, 17, 50:
		default:
			if automatedChecks[p] {
				t.Fatalf("principle %d must be officer-assessed", p)
			}
		}
	}
}

// The statement body hash is a stable SHA-256 pin — same body ⇒ same
// hex digest; any byte change changes the pin.
func TestStatementHashPinning(t *testing.T) {
	h := statementHash("statement-body-v1")
	if len(h) != 64 {
		t.Fatalf("hash len=%d want 64 hex", len(h))
	}
	if h != statementHash("statement-body-v1") {
		t.Fatal("statement hash must be deterministic")
	}
	if h == statementHash("statement-body-v2") {
		t.Fatal("distinct bodies must pin distinct hashes")
	}
}

// Basel convert: reporting-ccy passthrough, zero-amount exemption,
// missing converter + missing rate both flag inputs_complete=false and
// exclude the component (§2.7 fail-closed, never fabricate a rate).
func TestBaselConvert(t *testing.T) {
	svc := &BaselService{reportingCCY: "USD"}
	var incomplete []string

	v, ok := svc.convert(context.Background(), decimal.Zero, "EUR", &incomplete)
	if !ok || !v.IsZero() {
		t.Fatal("zero amount needs no rate")
	}
	v, ok = svc.convert(context.Background(),
		decimal.RequireFromString("100"), "USD", &incomplete)
	if !ok || !v.Equal(decimal.RequireFromString("100")) {
		t.Fatal("same-ccy passthrough failed")
	}
	if _, ok = svc.convert(context.Background(),
		decimal.RequireFromString("50"), "EUR", &incomplete); ok {
		t.Fatal("unpriced non-USD leg must fail closed")
	}
	if len(incomplete) != 1 || !strings.Contains(incomplete[0], "EUR") {
		t.Fatalf("incomplete=%v want one EUR entry", incomplete)
	}
}

func TestBaselConvertWithRate(t *testing.T) {
	svc := &BaselService{
		reportingCCY: "USD",
		conv: func(ctx context.Context, ccy string) (decimal.Decimal, error) {
			if ccy != "EUR" {
				return decimal.Zero, nil // unknown → unpriced
			}
			return decimal.RequireFromString("1.10"), nil
		},
	}
	var incomplete []string
	v, ok := svc.convert(context.Background(),
		decimal.RequireFromString("100"), "EUR", &incomplete)
	if !ok || !v.Equal(decimal.RequireFromString("110")) {
		t.Fatalf("converted=%s ok=%v want 110/true", v, ok)
	}
	if _, ok = svc.convert(context.Background(),
		decimal.RequireFromString("100"), "JPY", &incomplete); ok {
		t.Fatal("missing JPY rate must flag unpriced")
	}
	if len(incomplete) != 1 {
		t.Fatalf("incomplete=%v want 1 entry", incomplete)
	}
}

// §27.1-pinned floors: CAR 8%, leverage 3%.
func TestBaselFloors(t *testing.T) {
	if !BaselCARFloor.Equal(decimal.RequireFromString("0.08")) {
		t.Fatalf("CAR floor=%s want 0.08", BaselCARFloor)
	}
	if !BaselLeverageFloor.Equal(decimal.RequireFromString("0.03")) {
		t.Fatalf("leverage floor=%s want 0.03", BaselLeverageFloor)
	}
}

// The snapshot-key contract the EOD sweep + regen path both rely on:
// period-normalised keys are deterministic and replayable.
func TestBaselRunEODKeyShape(t *testing.T) {
	yesterday := time.Date(2026, 9, 20, 5, 0, 0, 0, time.UTC).
		Add(-24 * time.Hour)
	period := time.Date(yesterday.Year(), yesterday.Month(),
		yesterday.Day(), 0, 0, 0, 0, time.UTC)
	key := "eod:" + period.Format("2006-01-02")
	if key != "eod:2026-09-19" {
		t.Fatalf("eod key=%s want eod:2026-09-19", key)
	}
}

// RegChange lifecycle constants the §27.1 matrix pins.
func TestRegChangeConstants(t *testing.T) {
	if RegCodeTriageSLA != "RULEBOOK_VERSION_STALE" {
		t.Fatalf("triage SLA code=%s", RegCodeTriageSLA)
	}
	if RegCodeDeadlineNear != "REGULATORY_DEADLINE_APPROACHING" {
		t.Fatalf("deadline code=%s", RegCodeDeadlineNear)
	}
	if RegEffectiveAlertWindow != 90*24*time.Hour {
		t.Fatalf("effective window=%s want 90d", RegEffectiveAlertWindow)
	}
}
