// Task 5.3.15 — promo-window unit tests. Create validates rates and the
// window BEFORE touching the store, so those paths run against a nil
// pool; the full lifecycle lives in promos_integration_test.go.
package promos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestNewStoreNil(t *testing.T) {
	if _, err := NewStore(nil); err == nil {
		t.Fatal("nil pool accepted")
	}
}

func decN(t *testing.T, v int64) *decimal.Decimal {
	t.Helper()
	d := decimal.NewFromInt(v)
	return &d
}

func TestCreateValidation(t *testing.T) {
	// NewStore refuses a nil pool — build the store in-package; every
	// path below returns before the pool is dereferenced.
	s := &Store{now: time.Now}
	ctx := context.Background()
	future := time.Now().Add(time.Hour)

	if _, err := s.Create(ctx, 0, 1, decN(t, 10), nil, future, nil); err == nil {
		t.Fatal("fee_tier_id=0 accepted")
	}
	if _, err := s.Create(ctx, 1, 0, decN(t, 10), nil, future, nil); err == nil {
		t.Fatal("created_by=0 accepted")
	}
	if _, err := s.Create(ctx, 1, 1, nil, nil, future, nil); !errors.Is(err, ErrNoRates) {
		t.Fatalf("no rates err=%v want ErrNoRates", err)
	}
	neg := decimal.NewFromInt(-1)
	if _, err := s.Create(ctx, 1, 1, &neg, nil, future, nil); !errors.Is(err, ErrRateRange) {
		t.Fatalf("negative rate err=%v want ErrRateRange", err)
	}
	over := decimal.NewFromInt(10001)
	if _, err := s.Create(ctx, 1, 1, nil, &over, future, nil); !errors.Is(err, ErrRateRange) {
		t.Fatalf("rate>10000 err=%v want ErrRateRange", err)
	}
	if _, err := s.Create(ctx, 1, 1, decN(t, 10), nil,
		time.Now().Add(-time.Minute), nil); !errors.Is(err, ErrBadWindow) {
		t.Fatalf("past ends_at err=%v want ErrBadWindow", err)
	}
}

// Rate boundary: 0 and 10 000 bps are legal — the promo can zero out or
// saturate a fee within the promotion's cap.
func TestValidRateBounds(t *testing.T) {
	if !validRate(decimal.Zero) || !validRate(decimal.NewFromInt(10000)) {
		t.Fatal("boundary rates rejected")
	}
	if validRate(decimal.NewFromInt(-1)) || validRate(decimal.NewFromInt(10001)) {
		t.Fatal("out-of-range rate accepted")
	}
}
