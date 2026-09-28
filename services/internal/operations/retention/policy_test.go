package retention_test

import (
	"os"
	"path/filepath"
	"testing"

	"exchange/internal/operations/retention"
)

func TestBuiltinPolicyValidates(t *testing.T) {
	if err := retention.BuiltinPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
	// Spec §19.12 canonical schedule spot checks.
	p := retention.BuiltinPolicy()
	if c := p.Class("trades"); c == nil || c.RetainDays != 1825 {
		t.Fatalf("trades class: %+v", c)
	}
	if c := p.Class("audit_hash_chain"); c == nil || c.RetainDays != 2555 {
		t.Fatalf("audit class: %+v", c)
	}
	if c := p.Class("tick_history"); c == nil || c.RetainDays != 90 {
		t.Fatalf("ticks class: %+v", c)
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pol.yaml")
	yaml := `
version: 1
classes:
  - name: trades
    store: postgres_partitioned
    table: trades
    hot_days: 90
    warm_days: 365
    retain_days: 1825
    mechanism: "detach→warm→worm"
    regulatory_basis: "MiFID II RTS 6"
    gdpr_interaction: "Art. 17(3)(b)"
  - name: dedup
    store: postgres_table
    table: client_order_id_dedup
    ts_column: created_at
    retain_days: 7
    purgeable: true
    mechanism: "row purge"
    regulatory_basis: "internal"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := retention.LoadYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Classes) != 2 || p.Class("dedup") == nil {
		t.Fatalf("policy: %+v", p)
	}
}

// The shipped tiering_policy.yaml is the doc-of-record for §19.12 — it
// must cover every builtin class and agree on retention-critical fields.
func TestShippedPolicyCoversBuiltin(t *testing.T) {
	path := filepath.Join("..", "..", "..", "infrastructure",
		"data-tiering", "tiering_policy.yaml")
	p, err := retention.LoadYAML(path)
	if err != nil {
		t.Skipf("repo policy file not readable: %v", err)
	}
	b := retention.BuiltinPolicy()
	for _, bc := range b.Classes {
		yc := p.Class(bc.Name)
		if yc == nil {
			t.Fatalf("shipped policy missing class %q", bc.Name)
		}
		if yc.Store != bc.Store || yc.Table != bc.Table ||
			yc.RetainDays != bc.RetainDays || yc.Purgeable != bc.Purgeable {
			t.Fatalf("class %q drifted: builtin %+v vs shipped %+v",
				bc.Name, bc, yc)
		}
	}
}

func TestLoadYAMLRejectsUnsafe(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"dup.yaml": `version: 1
classes:
  - {name: a, store: postgres_table, table: t1, retain_days: 5}
  - {name: a, store: postgres_table, table: t2, retain_days: 5}`,
		"inj.yaml": `version: 1
classes:
  - {name: x, store: postgres_table, table: "t; DROP TABLE users", retain_days: 5}`,
		"empty.yaml": `version: 1
classes: []`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := retention.LoadYAML(path); err == nil {
			t.Fatalf("%s: expected validation error", name)
		}
	}
}
