package compliance

// Phase-21 Task 21.3.1 — tests for the production-feed surfaces added
// to the file-backed screener: OFAC alt.csv alternate identities, UK
// HMT/OFSI header-driven CSV, EU/UN XML alias containers, PEP-kind
// entries, per-list provenance, reload deltas and the provider-gate
// fail-closed seam (Task 21.3.23).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassifyFile(t *testing.T) {
	cases := []struct {
		name   string
		kind   EntryKind
		source string
		alias  bool
	}{
		{"sdn.csv", EntryKindSanctions, SrcOFACSDN, false},
		{"alt.csv", EntryKindSanctions, SrcOFACSDNAlt, true},
		{"ofac-alt-2026.csv", EntryKindSanctions, SrcOFACSDNAlt, true},
		{"uk-hmt.csv", EntryKindSanctions, SrcUKHMT, false},
		{"ofsi-consolidated.csv", EntryKindSanctions, SrcUKHMT, false},
		{"eu-consolidated.xml", EntryKindSanctions, SrcEUConsolidated, false},
		{"un-consolidated.xml", EntryKindSanctions, SrcUNConsolidated, false},
		{"other.xml", EntryKindSanctions, SrcEUConsolidated, false},
		{"pep.csv", EntryKindPEP, SrcPEP, false},
		{"vendor_pep.txt", EntryKindPEP, SrcPEP, false},
		{"names.txt", EntryKindSanctions, SrcLocal, false},
		{"names.lst", EntryKindSanctions, SrcLocal, false},
	}
	for _, c := range cases {
		kind, src, alias := classifyFile(c.name)
		if kind != c.kind || src != c.source || alias != c.alias {
			t.Errorf("classifyFile(%q) = (%s,%s,%v), want (%s,%s,%v)",
				c.name, kind, src, alias, c.kind, c.source, c.alias)
		}
	}
}

func TestOFACAltAliasResolvesToPrimary(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"sdn.csv": `9001,"SANCTIONOVICH, SANCTIONED",Individual,"DEV"` + "\n" +
			`9002,"BLOCKED HOLDINGS LTD",Entity,"DEV"` + "\n",
		"alt.csv": `80001,9001,"SANCTIONOVICH, S.",aka,"DEV"` + "\n" +
			`80004,9002,"BHG LIMITED",aka,"DEV"` + "\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	hit, ok := s.MatchDetail("BHG Limited")
	if !ok {
		t.Fatal("alias name not matched")
	}
	if !hit.Alias {
		t.Fatal("hit not flagged as alias")
	}
	if hit.List != SrcOFACSDNAlt {
		t.Fatalf("alias provenance: got %s", hit.List)
	}
	// Alias resolves back to the primary listed party (normalized
	// "BLOCKED HOLDINGS LTD" → legal-form strip → "BLOCKED HOLDINGS").
	if hit.AliasOf == "" {
		t.Fatal("alias did not resolve to primary party")
	}
	if _, ok := s.Match(hit.AliasOf); !ok {
		t.Fatalf("resolved primary %q is not itself a hit", hit.AliasOf)
	}
}

func TestUKHMTHeaderDrivenCSV(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"uk-hmt.csv": "Group ID,Name1,Name2,Name6,Title,Alias1\n" +
			`HMT1,Embargo,Markets,,,"Embargo Markets LLP"` + "\n" +
			`HMT2,,,Sanctioned Shipping SA,,"SS Lines, SanShip"` + "\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s.Match("Embargo Markets"); !ok {
		t.Fatal("HMT Name1..Name6 composite not matched")
	}
	if _, ok := s.Match("Sanctioned Shipping SA"); !ok {
		t.Fatal("HMT Name6-only primary not matched")
	}
	// Comma-split alias column contributes alias rows.
	hit, ok := s.MatchDetail("SanShip")
	if !ok || !hit.Alias {
		t.Fatalf("HMT alias column not matched (ok=%v alias=%v)", ok, hit.Alias)
	}
	if hit.AliasOf == "" {
		t.Fatal("HMT alias missing parent resolution")
	}
}

func TestXMLAliasContainers(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"eu.xml": `<?xml version="1.0"?><CONSOLIDATED_LIST dateGenerated="2026-02-01">` +
			`<SUBJECT><NAME><WHOLENAME>Maximilian Powers</WHOLENAME></NAME>` +
			`<NAME_ALIAS><WHOLENAME>Max Powers Junior</WHOLENAME></NAME_ALIAS>` +
			`<ALIAS wholeName="Powers, Max"/>` +
			`</SUBJECT></CONSOLIDATED_LIST>`,
		"un.xml": `<?xml version="1.0"?><CONSOLIDATED_LIST><INDIVIDUALS>` +
			`<INDIVIDUAL><DATAID>555</DATAID><FIRST_NAME>DR</FIRST_NAME>` +
			`<SECOND_NAME>EVIL</SECOND_NAME><THIRD_NAME>TESTCASE</THIRD_NAME>` +
			`<INDIVIDUAL_ALIAS><ALIAS_NAME>DOCTOR EVIL</ALIAS_NAME></INDIVIDUAL_ALIAS>` +
			`</INDIVIDUAL></INDIVIDUALS></CONSOLIDATED_LIST>`,
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, want := range []string{
		"Max Powers Junior", // EU NAME_ALIAS element text
		"Powers Max",        // EU ALIAS attribute name (normalized)
		"Doctor Evil",       // UN INDIVIDUAL_ALIAS
	} {
		hit, ok := s.MatchDetail(want)
		if !ok {
			t.Fatalf("alias %q not matched", want)
		}
		if !hit.Alias {
			t.Fatalf("alias %q not flagged alias=true", want)
		}
	}
	// UN alias carries the parent DATAID → resolves to the composed
	// primary name.
	hit, _ := s.MatchDetail("Doctor Evil")
	if hit.AliasOf == "" {
		t.Fatal("UN alias did not resolve to primary via DATAID")
	}
	// Version surfaces from the root dateGenerated attribute.
	var euMeta *ListMeta
	for _, m := range s.Provenance() {
		if m.File == "eu.xml" {
			cp := m
			euMeta = &cp
		}
	}
	if euMeta == nil || euMeta.Version != "2026-02-01" {
		t.Fatalf("EU provenance version: %+v", euMeta)
	}
}

func TestPEPKindSeparation(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"pep.txt": "Senator Fixture Example\n",
		"dev.txt": "Blocked Beneficiary Trading\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.EntryCount(EntryKindPEP) != 1 {
		t.Fatalf("pep entries: %d", s.EntryCount(EntryKindPEP))
	}
	// PEP entry hits only the PEP kind surface…
	hit, ok := s.MatchKind("Senator Fixture Example", EntryKindPEP)
	if !ok || hit.Kind != string(EntryKindPEP) {
		t.Fatalf("pep kind match: ok=%v hit=%+v", ok, hit)
	}
	// …and never the SANCTIONS funding-block surface.
	if _, ok := s.MatchKind("Senator Fixture Example", EntryKindSanctions); ok {
		t.Fatal("PEP entry leaked into the sanctions kind")
	}
	if hit, err := s.ScreenDeposit(context.Background(), 7,
		"Senator Fixture Example", ""); err != nil || hit {
		t.Fatalf("pep name must not block deposits: hit=%v err=%v", hit, err)
	}
}

func TestProvenanceAndDelta(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"dev.txt": "Blocked Beneficiary Trading\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	prov := s.Provenance()
	if len(prov) != 1 || prov[0].SHA256 == "" || prov[0].Source != SrcLocal {
		t.Fatalf("provenance: %+v", prov)
	}
	// Initial load delta is the whole list.
	if len(s.LastDelta().Added) == 0 {
		t.Fatal("initial delta should carry the full entry set")
	}
	var hooked ListDelta
	var hookCalls int
	s.WithDeltaHook(func(d ListDelta) { hooked = d; hookCalls++ })
	// Identical reload → empty delta, no hook.
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if hookCalls != 0 {
		t.Fatal("hook fired on a no-op reload")
	}
	// Add an entry → delta records the addition, hook fires once.
	if err := os.WriteFile(filepath.Join(dir, "dev.txt"),
		[]byte("Blocked Beneficiary Trading\nNew Sanctioned Party\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload2: %v", err)
	}
	d := s.LastDelta()
	if len(d.Added) != 1 || len(d.Removed) != 0 {
		t.Fatalf("delta: %+v", d)
	}
	if hookCalls != 1 || len(hooked.Added) != 1 {
		t.Fatalf("hook calls=%d delta=%+v", hookCalls, hooked)
	}
	if _, ok := s.Match("New Sanctioned Party"); !ok {
		t.Fatal("reloaded entry not matching")
	}
	// Remove → delta records the delisting.
	if err := os.WriteFile(filepath.Join(dir, "dev.txt"),
		[]byte("Blocked Beneficiary Trading\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload3: %v", err)
	}
	if len(s.LastDelta().Removed) != 1 {
		t.Fatalf("removal delta: %+v", s.LastDelta())
	}
}

func TestReloadFailureRetainsLastGood(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"dev.txt": "Blocked Beneficiary Trading\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// A file that parses to zero usable names fails the whole reload —
	// the working set stays put.
	if err := os.WriteFile(filepath.Join(dir, "dev.txt"),
		[]byte("# only comments\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err == nil {
		t.Fatal("reload of an all-empty set must fail")
	}
	if _, ok := s.Match("Blocked Beneficiary Trading"); !ok {
		t.Fatal("failed reload unloaded the working set")
	}
}

func TestProviderGateFailClosed(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"dev.txt": "Blocked Beneficiary Trading\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	gate := NewProviderGate([]string{"vendorA"})
	s.WithProviderGate(gate)
	ctx := context.Background()
	// The only configured provider failing quarantines the gate.
	gate.ReportResult(ctx, "vendorA", errTest())
	if !gate.Quarantined() {
		t.Fatal("gate did not quarantine on all-providers-down")
	}
	if _, err := s.ScreenParty(ctx, "", "Blocked Beneficiary Trading"); err == nil {
		t.Fatal("screen passed while quarantined")
	}
	if hit, err := s.ScreenDeposit(ctx, 7, "Blocked Beneficiary Trading", ""); err == nil || hit {
		t.Fatal("deposit screen passed while quarantined")
	}
	// Recovery clears the gate and screens again.
	gate.ReportResult(ctx, "vendorA", nil)
	if gate.Quarantined() {
		t.Fatal("gate did not recover on provider success")
	}
	if _, err := s.ScreenParty(ctx, "", "Blocked Beneficiary Trading"); err != nil {
		t.Fatalf("screen still failing after recovery: %v", err)
	}
}

type testProviderErr struct{}

func (testProviderErr) Error() string { return "vendor timeout" }
func errTest() error                  { return testProviderErr{} }

var _ = time.Now // keep time import if unused
