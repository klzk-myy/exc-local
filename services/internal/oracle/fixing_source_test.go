package oracle

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"exchange/pkg/decimal"
)

// fakeMarkGet answers the two keys the source reads; absent entries
// report goredis.Nil like a real miss.
func fakeMarkGet(m map[string]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, k string) (string, error) {
		if v, ok := m[k]; ok {
			return v, nil
		}
		return "", goredis.Nil
	}
}

func TestFixingMarkSource_ServesFreshMark(t *testing.T) {
	now := time.Now().UTC()
	src := &FixingMarkSource{
		Now: func() time.Time { return now },
		Get: fakeMarkGet(map[string]string{
			MarkKey("EUR/USD"):         "1.08525000",
			OracleMarkTsKey("EUR/USD"): strconv.FormatInt(now.UnixNano(), 10),
		}),
	}
	fr, err := src.FixingRate(context.Background(), "EUR/USD", "ECB_REF_1415", now)
	if err != nil {
		t.Fatalf("fixing rate: %v", err)
	}
	if !fr.Rate.Equal(decimal.RequireFromString("1.08525000")) {
		t.Fatalf("rate %s", fr.Rate)
	}
	if fr.Source != "oracle-mark:ECB_REF_1415" {
		t.Fatalf("source %q", fr.Source)
	}
}

func TestFixingMarkSource_StaleMarkRefused(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-10 * time.Second) // outside the 5s staleness gate
	src := &FixingMarkSource{
		Now: func() time.Time { return now },
		Get: fakeMarkGet(map[string]string{
			MarkKey("EUR/USD"):         "1.08525000",
			OracleMarkTsKey("EUR/USD"): strconv.FormatInt(old.UnixNano(), 10),
		}),
	}
	if _, err := src.FixingRate(context.Background(), "EUR/USD", "ECB_REF_1415", now); err == nil {
		t.Fatal("stale mark must be refused — scheduler records SKIPPED")
	}
}

func TestFixingMarkSource_AbsentMarkRefused(t *testing.T) {
	src := &FixingMarkSource{Get: fakeMarkGet(nil)}
	if _, err := src.FixingRate(context.Background(), "EUR/USD", "WM_LONDON_4PM",
		time.Now()); err == nil {
		t.Fatal("absent mark must error — SKIPPED, never fabricated")
	}
}

func TestFixingMarkSource_NonPositiveMarkRefused(t *testing.T) {
	now := time.Now().UTC()
	for _, bad := range []string{"0", "-1.2", "banana"} {
		src := &FixingMarkSource{
			Now: func() time.Time { return now },
			Get: fakeMarkGet(map[string]string{
				MarkKey("EUR/USD"):         bad,
				OracleMarkTsKey("EUR/USD"): strconv.FormatInt(now.UnixNano(), 10),
			}),
		}
		if _, err := src.FixingRate(context.Background(), "EUR/USD", "ECB_REF_1415", now); err == nil {
			t.Fatalf("mark %q must be refused", bad)
		}
	}
}

func TestFixingMarkSource_UnwiredIsFailClosed(t *testing.T) {
	var src *FixingMarkSource
	if _, err := src.FixingRate(context.Background(), "EUR/USD", "ECB_REF_1415",
		time.Now()); err == nil {
		t.Fatal("nil source must error")
	}
	if _, err := (&FixingMarkSource{}).FixingRate(context.Background(),
		"EUR/USD", "ECB_REF_1415", time.Now()); err == nil {
		t.Fatal("nil Get seam must error")
	}
}

// A future-dated stamp is as suspicious as a stale one — a skewed
// oracle clock must not pass the freshness bound.
func TestFixingMarkSource_FutureDatedMarkRefused(t *testing.T) {
	now := time.Now().UTC()
	src := &FixingMarkSource{
		Now: func() time.Time { return now },
		Get: fakeMarkGet(map[string]string{
			MarkKey("EUR/USD"): "1.08525000",
			OracleMarkTsKey("EUR/USD"): strconv.FormatInt(
				now.Add(10*time.Second).UnixNano(), 10),
		}),
	}
	if _, err := src.FixingRate(context.Background(), "EUR/USD", "ECB_REF_1415", now); err == nil {
		t.Fatal("future-dated mark must be refused")
	}
}

func TestFixingMarkSource_GetErrorPropagates(t *testing.T) {
	src := &FixingMarkSource{Get: func(context.Context, string) (string, error) {
		return "", fmt.Errorf("redis down")
	}}
	if _, err := src.FixingRate(context.Background(), "EUR/USD", "ECB_REF_1415",
		time.Now()); err == nil {
		t.Fatal("read error must propagate — SKIPPED")
	}
}

// Redis-gated end-to-end: publisher writes the mark keys, the source
// reads them — pins the real key contract both directions.
func TestRedisFixingMarkSourceRoundtrip(t *testing.T) {
	rdb := redisClient(t)
	ctx := context.Background()
	pub := &RedisPublisher{C: rdb.Client}
	mark := decimal.RequireFromString("1.08525000")
	if err := pub.PublishMark(ctx, MarkResult{
		Symbol: "ITF/EUR", At: time.Now().UTC(), Mark: mark,
		Index: mark, OK: true, Fresh: []string{"a", "b"},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	src := NewRedisFixingMarkSource(rdb.Client)
	fr, err := src.FixingRate(ctx, "ITF/EUR", "ECB_REF_1415", time.Now().UTC())
	if err != nil {
		t.Fatalf("fixing rate: %v", err)
	}
	if !fr.Rate.Equal(mark) {
		t.Fatalf("rate %s != %s", fr.Rate, mark)
	}
	// Cleanup — the keys are TTL-less, so delete explicitly.
	_, _ = rdb.Del(ctx, MarkKey("ITF/EUR"), MarkPriceKey("ITF/EUR"),
		OracleMarkKey("ITF/EUR"), OracleMarkTsKey("ITF/EUR"),
		IndexPriceKey("ITF/EUR"), OracleIndexKey("ITF/EUR"),
		OracleIndexTsKey("ITF/EUR")).Result()
}
