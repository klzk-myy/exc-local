package golden

import (
	"context"
	"testing"
	"time"

	spec "exchange-testspec/spec"
)

// TestGoldenCorpus lets `go test ./golden` run the corpus standalone —
// identical CheckFuncs, one subtest each. Skip results (dependency absent)
// become t.Skip; anything other than pass/skip fails the subtest.
func TestGoldenCorpus(t *testing.T) {
	env := spec.DefaultEnv()
	reg := spec.NewRegistry()
	RegisterAll(reg)

	ids := reg.GoldenIDs()
	if len(ids) < 20 {
		t.Fatalf("golden corpus must have ≥20 cases, registered %d", len(ids))
	}
	for _, id := range ids {
		e, _ := reg.Lookup(id)
		t.Run(id, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res := e.Func(ctx, env)
			switch res.Status {
			case spec.StatusPass:
				t.Log(res.Detail)
			case spec.StatusSkip:
				t.Skip(res.Detail)
			default:
				t.Fatalf("%s: %s", res.Status, res.Detail)
			}
		})
	}
}
