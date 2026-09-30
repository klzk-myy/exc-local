// Phase-21 Task 21.3.22 — CRS/FATCA XML golden tests. The artifacts are
// schema-shaped and deterministic: same input → same bytes. Golden
// files under testdata/ pin the byte shape; regenerate with:
//
//	go test ./internal/compliance/ -run TestTaxXML -update
package compliance

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func goldenAccounts() []reportableAccount {
	return []reportableAccount{
		{
			AccountID: 1001, AccountNumber: "ACC-1001",
			HolderName:       "Ada Lovelace",
			Address:          "12 Example Street\nLondon\nNW1 1AA",
			ResidenceCountry: "GB", TIN: "QQ123456C", TINIssuedBy: "GB",
			Balances: []reportAmount{
				{Currency: "USD", Amount: "12500.00"},
				{Currency: "EUR", Amount: "8000.55"},
			},
			Payments: []reportAmount{
				{Currency: "USD", Amount: "412.10"},
			},
		},
		{
			AccountID: 2002, AccountNumber: "ACC-2002",
			HolderName:       "Acme Trading Ltd",
			Address:          "Bahnhofstrasse 1\n8001 Zürich",
			ResidenceCountry: "CH", TIN: "CHE-123.456.789",
			TINIssuedBy: "CH", Entity: true,
			Balances: []reportAmount{
				{Currency: "CHF", Amount: "98000.00"},
			},
		},
	}
}

func goldenCheck(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s drifted from golden; run -update if intentional", name)
	}
}

func TestTaxXMLCRSGolden(t *testing.T) {
	venue := TaxVenue{Name: "Exc Global Markets Ltd",
		IN: "ABCDEF.00000.LE.826", Country: "GB"}
	ts := time.Date(2030, 3, 1, 9, 30, 0, 0, time.UTC)
	out, err := BuildCRSXML(venue, "DE", 2029, 1, ts, goldenAccounts())
	if err != nil {
		t.Fatalf("build crs xml: %v", err)
	}
	goldenCheck(t, "crs_golden.xml", out)

	// Determinism: same inputs → identical bytes.
	again, err := BuildCRSXML(venue, "DE", 2029, 1, ts, goldenAccounts())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !bytes.Equal(out, again) {
		t.Fatal("CRS build not deterministic")
	}
	// Content-derived MessageRefId must move when the set moves.
	alt, err := BuildCRSXML(venue, "DE", 2029, 1, ts, goldenAccounts()[:1])
	if err != nil {
		t.Fatalf("alt build: %v", err)
	}
	if bytes.Equal(out, alt) {
		t.Fatal("distinct account sets produced identical artifacts")
	}
}

func TestTaxXMLFATCAGolden(t *testing.T) {
	venue := TaxVenue{Name: "Exc Global Markets Ltd",
		IN: "ABCDEF.00000.LE.826", Country: "GB"}
	ts := time.Date(2030, 3, 1, 9, 30, 0, 0, time.UTC)
	usAccounts := []reportableAccount{{
		AccountID: 3003, AccountNumber: "ACC-3003",
		HolderName:       "John Q Public",
		Address:          "1 Main St\nNew York, NY 10001",
		ResidenceCountry: "US", TIN: "123-45-6789", TINIssuedBy: "US",
		Balances: []reportAmount{{Currency: "USD", Amount: "44000.00"}},
		Payments: []reportAmount{{Currency: "USD", Amount: "900.00"}},
	}}
	out, err := BuildFATCAXML(venue, 2029, 1, ts, usAccounts)
	if err != nil {
		t.Fatalf("build fatca xml: %v", err)
	}
	goldenCheck(t, "fatca_golden.xml", out)

	again, err := BuildFATCAXML(venue, 2029, 1, ts, usAccounts)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !bytes.Equal(out, again) {
		t.Fatal("FATCA build not deterministic")
	}
}
