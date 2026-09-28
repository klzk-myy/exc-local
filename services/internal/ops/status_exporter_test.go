package ops

import (
	"context"
	"errors"
	"testing"
	"time"

	exchredis "exchange/internal/redis"
)

type fakeMode struct {
	mode exchredis.DegradationMode
	err  error
}

func (f fakeMode) GetDegradationMode(context.Context) (exchredis.DegradationState, error) {
	return exchredis.DegradationState{Mode: f.mode}, f.err
}

func agg(mode exchredis.DegradationMode, comps []Component) *Aggregator {
	return NewAggregator(nil, nil, fakeMode{mode: mode}, comps, nil)
}

func okProbe(context.Context) error   { return nil }
func downProbe(context.Context) error { return errors.New("refused") }

func TestCollectAggregates(t *testing.T) {
	cases := []struct {
		name  string
		mode  exchredis.DegradationMode
		comps []Component
		want  string
	}{
		{"all healthy", "Normal", []Component{
			{Name: "pg", Critical: true, Probe: okProbe},
			{Name: "nats", Probe: okProbe}}, StateOperational},
		{"critical down", "Normal", []Component{
			{Name: "pg", Critical: true, Probe: downProbe},
			{Name: "nats", Probe: okProbe}}, StateMajor},
		{"optional down", "Normal", []Component{
			{Name: "pg", Critical: true, Probe: okProbe},
			{Name: "nats", Probe: downProbe}}, StatePartial},
		{"readonly mode", "ReadOnly", []Component{
			{Name: "pg", Critical: true, Probe: okProbe}}, StateDegraded},
		{"maintenance mode", "Maintenance", []Component{
			{Name: "pg", Critical: true, Probe: okProbe}}, StateMaintenance},
	}
	for _, c := range cases {
		st := agg(c.mode, c.comps).Collect(context.Background())
		if st.Status != c.want {
			t.Fatalf("%s: status = %q, want %q", c.name, st.Status, c.want)
		}
	}
}

// Mode read failure reports Maintenance — fail-closed, never "Normal".
func TestModeReadFailureIsMaintenance(t *testing.T) {
	a := NewAggregator(nil, nil,
		fakeMode{err: errors.New("redis down")},
		[]Component{{Name: "pg", Critical: true, Probe: okProbe}}, nil)
	st := a.Collect(context.Background())
	if st.Mode != "Maintenance" || st.Status != StateMaintenance {
		t.Fatalf("mode failure → mode=%q status=%q", st.Mode, st.Status)
	}
}

// A probe that hangs is bounded by the per-probe timeout and reports down.
func TestProbeTimeoutIsDown(t *testing.T) {
	a := agg("Normal", []Component{{
		Name: "slow", Critical: true,
		Probe: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}})
	start := time.Now()
	st := a.Collect(context.Background())
	if time.Since(start) > 4*time.Second {
		t.Fatal("collect outlived probe budget")
	}
	if st.Components[0].State != "down" || st.Status != StateMajor {
		t.Fatalf("hung probe → %+v", st.Components[0])
	}
}

// Uptime math from the event ledger is covered by the PG integration
// test; here we assert the no-pool contract.
func TestUptimeRequiresPG(t *testing.T) {
	a := agg("Normal", nil)
	_, _, err := a.Uptime(context.Background(), "pg", time.Hour)
	if err == nil {
		t.Fatal("uptime without pool must error")
	}
}
