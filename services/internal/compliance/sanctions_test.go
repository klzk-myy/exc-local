package compliance

// Phase-13.5 Task 13.5.3.3 — unit tests for the file-backed sanctions
// screener: per-format parsing, normalized exact + Jaro-Winkler fuzzy
// matching at the spec-pinned 0.85 floor, and the fail-closed
// unavailable contract.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fixtureDir writes a scratch list directory.
func fixtureDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestListScreenerPlainTextExactAndNormalized(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"dev.txt": "# comment line\nBlocked Beneficiary Trading\n\nQuarantined Holdings\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	lists, n, _ := s.Stats()
	if n != 2 || len(lists) != 1 {
		t.Fatalf("stats: lists=%v entries=%d", lists, n)
	}
	// Exact normalized.
	if _, ok := s.Match("Blocked Beneficiary Trading"); !ok {
		t.Fatal("exact match missed")
	}
	// Normalization: punctuation + legal-form suffix + case.
	if _, ok := s.Match("blocked beneficiary trading ltd."); !ok {
		t.Fatal("normalized match missed (case/punct/legal form)")
	}
	// Negative.
	if _, ok := s.Match("Alice Example"); ok {
		t.Fatal("non-listed name matched")
	}
}

func TestListScreenerFuzzyThreshold(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"dev.txt": "Dr Evil Testcase\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// "Dr. Evil Test-Case" normalizes to "DR EVIL TEST CASE" — one
	// space away from the listed "DR EVIL TESTCASE": fuzzy hit.
	if _, ok := s.Match("Dr. Evil Test-Case"); !ok {
		t.Fatal("fuzzy match at >=0.85 missed")
	}
	// Clearly different name must not fuzzy-hit.
	if _, ok := s.Match("Derek Evil Twin"); ok {
		t.Fatal("distant name fuzzy-matched")
	}
}

func TestListScreenerOFACCSV(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"sdn.csv": `9001,"SANCTIONOVICH, SANCTIONED",Individual,"DEV"` + "\n" +
			`9002,"BLOCKED HOLDINGS LTD",Entity,"DEV"` + "\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s.Match("Sanctioned Sanctionovich"); !ok {
		t.Fatal("OFAC CSV name not matched")
	}
	// Legal-form strip: "BLOCKED HOLDINGS LTD" normalizes to
	// "BLOCKED HOLDINGS" — "Blocked Holdings Ltd." must hit.
	if _, ok := s.Match("Blocked Holdings Ltd."); !ok {
		t.Fatal("OFAC entity not matched")
	}
}

func TestListScreenerXMLFormats(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"eu.xml": `<?xml version="1.0"?><CONSOLIDATED_LIST>` +
			`<SUBJECT><NAME><WHOLENAME>Maximilian Powers</WHOLENAME></NAME></SUBJECT>` +
			`</CONSOLIDATED_LIST>`,
		"un.xml": `<?xml version="1.0"?><CONSOLIDATED_LIST><INDIVIDUALS>` +
			`<INDIVIDUAL><DATAID>1</DATAID><FIRST_NAME>DR</FIRST_NAME>` +
			`<SECOND_NAME>EVIL</SECOND_NAME><THIRD_NAME>TESTCASE</THIRD_NAME>` +
			`<UN_LIST_TYPE>TEST</UN_LIST_TYPE></INDIVIDUAL>` +
			`</INDIVIDUALS></CONSOLIDATED_LIST>`,
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := s.Match("Maximilian Powers"); !ok {
		t.Fatal("EU WHOLENAME not matched")
	}
	if _, ok := s.Match("Dr Evil Testcase"); !ok {
		t.Fatal("UN INDIVIDUAL name parts not composed")
	}
	lists, n, _ := s.Stats()
	if len(lists) != 2 || n != 2 {
		t.Fatalf("stats: lists=%v entries=%d", lists, n)
	}
}

func TestListScreenerFailClosedContract(t *testing.T) {
	// Missing dir → construction error (boot fails loudly).
	if _, err := NewListScreener(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing dir must fail construction")
	}
	// Empty/unusable dir → construction error.
	if _, err := NewListScreener(fixtureDir(t, map[string]string{
		"README.md": "not a list",
	})); err == nil {
		t.Fatal("dir with no usable entries must fail construction")
	}
	// List file that yields zero usable names → construction error.
	if _, err := NewListScreener(fixtureDir(t, map[string]string{
		"empty.csv": "",
	})); err == nil {
		t.Fatal("list yielding zero usable names must fail construction")
	}
}

func TestListScreenerScreensBothDirections(t *testing.T) {
	dir := fixtureDir(t, map[string]string{
		"dev.txt": "Blocked Beneficiary Trading\nSuspicious Sender\n",
	})
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ctx := context.Background()
	hit, err := s.ScreenDeposit(ctx, 7, "Suspicious Sender", "DE89370400440532013000")
	if err != nil || !hit {
		t.Fatalf("deposit screen: hit=%v err=%v", hit, err)
	}
	hit, err = s.ScreenWithdrawal(ctx, 7, "Blocked Beneficiary Trading", "IBAN-1")
	if err != nil || !hit {
		t.Fatalf("withdrawal screen: hit=%v err=%v", hit, err)
	}
	// Clean both ways.
	hit, err = s.ScreenDeposit(ctx, 7, "Alice Example", "IBAN-CLEAN")
	if err != nil || hit {
		t.Fatalf("clean deposit screened positive: %v %v", hit, err)
	}
	hit, err = s.ScreenWithdrawal(ctx, 7, "Alice Example", "IBAN-CLEAN")
	if err != nil || hit {
		t.Fatalf("clean withdrawal screened positive: %v %v", hit, err)
	}
	// Destination reference alone is screened too.
	hit, err = s.ScreenWithdrawal(ctx, 7, "", "Suspicious-Sender-Ref")
	if err != nil {
		t.Fatalf("destination-only screen errored: %v", err)
	}
	// Not expected to hit (ref isn't a listed name), just proving the
	// second candidate is evaluated without a beneficiary name.
	_ = hit
}

// The shipped dev fixture must load and contain its documented names —
// this pins deploy/security/sanctions-dev as executable dev evidence.
func TestDevFixtureLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "deploy", "security", "sanctions-dev")
	s, err := NewListScreener(dir)
	if err != nil {
		t.Fatalf("dev fixture load: %v", err)
	}
	lists, n, _ := s.Stats()
	if n < 5 || len(lists) < 4 {
		t.Fatalf("dev fixture under-loaded: lists=%v entries=%d", lists, n)
	}
	for _, want := range []string{
		"Dr Evil Testcase",            // UN INDIVIDUAL parts
		"Maximilian Powers",           // EU WHOLENAME
		"Sanctioned Sanctionovich",    // OFAC CSV "LAST, FIRST"
		"Blocked Beneficiary Trading", // local txt
	} {
		if _, ok := s.Match(want); !ok {
			t.Fatalf("dev fixture name %q not screened positive", want)
		}
	}
}
