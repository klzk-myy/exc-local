// Per-table-class lifecycle policy for the tiered archival engine
// (Phase-09 Tasks 9.3.17 / 9.3.24, spec §19.7, §19.12).
//
// Each partitioned parent gets a class policy:
//
//	HotDays    — range-end age at which a partition leaves the hot tier
//	             (still attached + indexed). Spec §19.7 pins 90 days for
//	             the OLTP parents.
//	WarmDays   — total age at which a warm (detached, cheap-storage)
//	             partition is exported to the cold tier (S3 WORM). §19.7
//	             defines warm as 90d–1y, so the default cold boundary is
//	             365 days. (Task 9.3.24 prose says "90 days–2 years"; the
//	             §19.7 1-year boundary is the canonical value — recorded
//	             as a §27 deviation candidate.)
//	RetainDays — WORM Object Lock retain-until window stamped on every
//	             uploaded object. 1825d (5y, MiFID II RTS 6 / CFTC) for
//	             order/trade classes; 2555d (7y) for audit/finance
//	             classes per §19.12 and Task 9.3.17 step 1.
//	WarmSchema — schema detached partitions are moved into (the PG-side
//	             "warm" tier). Defaults to "warm".
package archiver

import (
	"fmt"
	"strings"
)

// errUnsafePolicy reports a policy row with an unsafe/empty identifier.
func errUnsafePolicy(ident string) error {
	return fmt.Errorf("archiver: unsafe identifier in class policy: %q", ident)
}

// ClassPolicy is the lifecycle contract for one partitioned parent.
type ClassPolicy struct {
	Parent     string `json:"parent"`
	HotDays    int    `json:"hot_days"`
	WarmDays   int    `json:"warm_days"`
	RetainDays int    `json:"retain_days"`
	WarmSchema string `json:"warm_schema"`
}

// DefaultWarmSchema is where detached partitions live between the hot
// cutoff and cold export.
const DefaultWarmSchema = "warm"

// Retain5Y / Retain7Y are the canonical WORM windows.
const (
	Retain5Y = 5 * 365
	Retain7Y = 7 * 365
)

// DefaultClassPolicies returns the spec-conformant per-class schedule.
// Parents not present here fall back to policyFor defaults.
//
//	orders, trades, order_audit          — 5y (MiFID II RTS 6, §19.12)
//	ledger_lines, ledger_entries,
//	journal_entries                      — 7y finance (§19.12)
//	audit_hash_chain                     — 7y audit (§19.12, Task 9.3.17)
func DefaultClassPolicies() []ClassPolicy {
	return []ClassPolicy{
		{Parent: "orders", HotDays: 90, WarmDays: 365, RetainDays: Retain5Y, WarmSchema: DefaultWarmSchema},
		{Parent: "trades", HotDays: 90, WarmDays: 365, RetainDays: Retain5Y, WarmSchema: DefaultWarmSchema},
		{Parent: "order_audit", HotDays: 90, WarmDays: 365, RetainDays: Retain5Y, WarmSchema: DefaultWarmSchema},
		{Parent: "ledger_lines", HotDays: 90, WarmDays: 365, RetainDays: Retain7Y, WarmSchema: DefaultWarmSchema},
		{Parent: "ledger_entries", HotDays: 90, WarmDays: 365, RetainDays: Retain7Y, WarmSchema: DefaultWarmSchema},
		{Parent: "journal_entries", HotDays: 90, WarmDays: 365, RetainDays: Retain7Y, WarmSchema: DefaultWarmSchema},
		{Parent: "audit_hash_chain", HotDays: 90, WarmDays: 365, RetainDays: Retain7Y, WarmSchema: DefaultWarmSchema},
	}
}

// policyFor resolves the class policy for a parent. Unknown parents get a
// conservative default: same hot cutoff, 1y warm, 5y retain.
func (a *Archiver) policyFor(parent string) ClassPolicy {
	for _, p := range a.policies {
		if p.Parent == parent {
			return p
		}
	}
	return ClassPolicy{
		Parent:     parent,
		HotDays:    a.cutoffDays,
		WarmDays:   365,
		RetainDays: a.retainDays,
		WarmSchema: DefaultWarmSchema,
	}
}

// SetClassPolicies replaces the per-class schedule. Parents not covered
// by policies fall back to the Archiver's cutoff/retain defaults.
func (a *Archiver) SetClassPolicies(policies []ClassPolicy) {
	a.policies = append([]ClassPolicy{}, policies...)
	var parents []string
	for _, p := range policies {
		if identRe.MatchString(p.Parent) {
			parents = append(parents, p.Parent)
		}
	}
	a.parents = parents
}

// normalizePolicy fills zero-valued fields from defaults so partial YAML
// classes stay safe.
func normalizePolicy(p ClassPolicy) ClassPolicy {
	if p.HotDays <= 0 {
		p.HotDays = 90
	}
	if p.WarmDays <= p.HotDays {
		p.WarmDays = 365
	}
	if p.RetainDays <= 0 {
		p.RetainDays = Retain5Y
	}
	if p.WarmSchema == "" {
		p.WarmSchema = DefaultWarmSchema
	}
	return p
}

// NormalizePolicies applies normalizePolicy to every class and validates
// identifiers (fail-closed: an unsafe identifier is an error, not a skip,
// so a config typo cannot silently drop a regulated table from scope).
func NormalizePolicies(policies []ClassPolicy) ([]ClassPolicy, error) {
	out := make([]ClassPolicy, len(policies))
	for i, p := range policies {
		p = normalizePolicy(p)
		p.Parent = strings.TrimSpace(p.Parent)
		if !identRe.MatchString(p.Parent) {
			return nil, errUnsafePolicy(p.Parent)
		}
		if !identRe.MatchString(p.WarmSchema) {
			return nil, errUnsafePolicy(p.WarmSchema)
		}
		out[i] = p
	}
	return out, nil
}
