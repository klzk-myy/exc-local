// Task 13.3.7 — solvency generation unit tests over fake seams (no DB):
// liability extraction contract, negative-balance abort, reserve-ratio
// semantics, signer wiring and proof self-verification.
package reconciliation

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

type solvFakeSource struct {
	liabs  []Liability
	assets map[string]decimal.Decimal
	err    error
}

func (f *solvFakeSource) ActiveLiabilities(context.Context) ([]Liability, error) {
	return f.liabs, f.err
}
func (f *solvFakeSource) NostroAssets(context.Context) (map[string]decimal.Decimal, error) {
	return f.assets, f.err
}

type solvFakeStore struct {
	snap    *Snapshot
	proofs  []proofRow
	insErr  error
	nextID  int64
	sawTx   bool
	proof   *ProofResult
	lstSnap *Snapshot
}

func (f *solvFakeStore) InsertSnapshot(_ context.Context, s *Snapshot, p []proofRow) (int64, error) {
	f.snap, f.proofs, f.sawTx = s, p, true
	f.lstSnap = s
	if f.insErr != nil {
		return 0, f.insErr
	}
	if f.nextID == 0 {
		f.nextID = 1
	}
	return f.nextID, nil
}
func (f *solvFakeStore) LatestSnapshot(context.Context) (*Snapshot, error) { return f.lstSnap, nil }
func (f *solvFakeStore) ProofFor(context.Context, int64, string) (*ProofResult, error) {
	return f.proof, nil
}
func (f *solvFakeStore) ProofsForAccount(context.Context, int64) ([]ProofResult, error) {
	return nil, nil
}

type solvFakeSigner struct {
	err  error
	kind string
}

func (f solvFakeSigner) Sign(context.Context, []byte) (string, string, string, error) {
	if f.err != nil {
		return "", "", "", f.err
	}
	k := f.kind
	if k == "" {
		k = SignerKindDevHMAC
	}
	return "SIG", "FPR", k, nil
}

func newSvc(t *testing.T, src LiabilitySource, st SnapshotStore, sg Signer) *SolvencyService {
	t.Helper()
	svc, err := NewSolvencyService(src, st, sg)
	if err != nil {
		t.Fatalf("NewSolvencyService: %v", err)
	}
	return svc
}

func TestGenerateHappyPath(t *testing.T) {
	src := &solvFakeSource{
		liabs: []Liability{
			{AccountID: 1, Currency: "USD", Balance: decimal.RequireFromString("100.5")},
			{AccountID: 2, Currency: "USD", Balance: decimal.Zero}, // zero rows included
			{AccountID: 2, Currency: "EUR", Balance: decimal.RequireFromString("7.25")},
		},
		assets: map[string]decimal.Decimal{
			"USD": decimal.RequireFromString("200"),
			"EUR": decimal.RequireFromString("10"),
		},
	}
	st := &solvFakeStore{}
	svc := newSvc(t, src, st, solvFakeSigner{})
	snap, err := svc.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if snap.ID != 1 || snap.LeafCount != 3 || !snap.Solvent {
		t.Fatalf("snap=%+v", snap)
	}
	if snap.Liabilities["USD"] != "100.5" || snap.Liabilities["EUR"] != "7.25" {
		t.Fatalf("liabilities=%v", snap.Liabilities)
	}
	if snap.ReserveRatios["USD"] == nil || snap.ReserveRatios["EUR"] == nil {
		t.Fatalf("ratios=%v", snap.ReserveRatios)
	}
	if snap.Signature != "SIG" || snap.SignerFingerprint != "FPR" {
		t.Fatalf("signature fields=%+v", snap)
	}
	// Every stored proof row self-verifies against the signed root —
	// the generator's own check plus this cross-check.
	root, err := ParseHexDigest(snap.MerkleRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.proofs) != 3 {
		t.Fatalf("proofs=%d", len(st.proofs))
	}
	for i, p := range st.proofs {
		steps := make([]ProofStep, len(p.Path))
		for j, s := range p.Path {
			h, err := ParseHexDigest(s.Hash)
			if err != nil {
				t.Fatalf("proof %d step %d: %v", i, j, err)
			}
			steps[j] = ProofStep{Hash: h, Right: s.Position == "right"}
		}
		in := LeafInput{AccountID: p.AccountID, Currency: p.Currency,
			Balance: p.Balance, Salt: p.Salt}
		if !Verify(in, steps, root) {
			t.Fatalf("stored proof %d does not verify", i)
		}
		if leafDigest(in) != p.LeafHash {
			t.Fatalf("stored proof %d leaf hash mismatch", i)
		}
		// Peer-data guard: path entries carry position+hash ONLY.
		for _, s := range p.Path {
			if s.Position != "left" && s.Position != "right" {
				t.Fatalf("proof %d bad position %q", i, s.Position)
			}
		}
	}
}

func TestGenerateNegativeBalanceAborts(t *testing.T) {
	src := &solvFakeSource{
		liabs: []Liability{
			{AccountID: 1, Currency: "USD", Balance: decimal.RequireFromString("10")},
			{AccountID: 2, Currency: "USD", Balance: decimal.RequireFromString("-0.01")},
		},
		assets: map[string]decimal.Decimal{"USD": decimal.NewFromInt(100)},
	}
	st := &solvFakeStore{}
	svc := newSvc(t, src, st, solvFakeSigner{})
	_, err := svc.Generate(context.Background())
	if err == nil {
		t.Fatal("negative liability must abort the run")
	}
	var e *excerrors.Error
	if !stderrors.As(err, &e) || e.Code != "INTERNAL_ERROR" {
		t.Fatalf("want coded INTERNAL_ERROR, got %v", err)
	}
	if st.sawTx {
		t.Fatal("store touched after abort — partial snapshot leaked")
	}
}

func TestGenerateInsolventFlag(t *testing.T) {
	src := &solvFakeSource{
		liabs: []Liability{
			{AccountID: 1, Currency: "USD", Balance: decimal.RequireFromString("100")},
		},
		assets: map[string]decimal.Decimal{"USD": decimal.RequireFromString("99.99")},
	}
	st := &solvFakeStore{}
	svc := newSvc(t, src, st, solvFakeSigner{})
	snap, err := svc.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if snap.Solvent {
		t.Fatal("99.99% cover must flag insolvent")
	}
}

func TestGenerateMissingNostroFailsRatio(t *testing.T) {
	src := &solvFakeSource{
		liabs:  []Liability{{AccountID: 1, Currency: "JPY", Balance: decimal.NewFromInt(5)}},
		assets: map[string]decimal.Decimal{}, // no nostro row at all
	}
	st := &solvFakeStore{}
	snap, err := newSvc(t, src, st, solvFakeSigner{}).Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if snap.Solvent || *snap.ReserveRatios["JPY"] != "0" {
		t.Fatalf("unattested liabilities must flag insolvent: %+v", snap.ReserveRatios)
	}
}

func TestGenerateZeroLiabilitiesNullRatio(t *testing.T) {
	src := &solvFakeSource{
		liabs:  []Liability{{AccountID: 1, Currency: "USD", Balance: decimal.Zero}},
		assets: map[string]decimal.Decimal{"USD": decimal.NewFromInt(50)},
	}
	snap, err := newSvc(t, src, &solvFakeStore{}, solvFakeSigner{}).Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !snap.Solvent {
		t.Fatal("zero liabilities cannot be insolvent")
	}
	if snap.ReserveRatios["USD"] != nil {
		t.Fatalf("zero-liability ratio must be null, got %v", *snap.ReserveRatios["USD"])
	}
}

func TestGenerateSignerFailurePublishesNothing(t *testing.T) {
	src := &solvFakeSource{
		liabs:  []Liability{{AccountID: 1, Currency: "USD", Balance: decimal.NewFromInt(1)}},
		assets: map[string]decimal.Decimal{"USD": decimal.NewFromInt(1)},
	}
	st := &solvFakeStore{}
	_, err := newSvc(t, src, st, solvFakeSigner{err: stderrors.New("no key")}).Generate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signing") {
		t.Fatalf("want signing failure, got %v", err)
	}
	if st.sawTx {
		t.Fatal("snapshot persisted despite signing failure")
	}
}

func TestNewServiceFailClosed(t *testing.T) {
	src := &solvFakeSource{}
	st := &solvFakeStore{}
	for i, args := range [][3]any{
		{nil, st, solvFakeSigner{}}, {src, nil, solvFakeSigner{}}, {src, st, nil},
	} {
		var li LiabilitySource
		var ss SnapshotStore
		var sg Signer
		if v, ok := args[0].(LiabilitySource); ok {
			li = v
		}
		if v, ok := args[1].(SnapshotStore); ok {
			ss = v
		}
		if v, ok := args[2].(Signer); ok {
			sg = v
		}
		if _, err := NewSolvencyService(li, ss, sg); err == nil {
			t.Fatalf("case %d: nil seam accepted", i)
		}
	}
}

func TestSignerFromEnv(t *testing.T) {
	// GPG without fingerprint refuses construction.
	if _, err := SignerFromEnv(func(k string) string {
		if k == "EXC_SOLVENCY_SIGNER" {
			return "gpg"
		}
		return ""
	}, "production"); err == nil {
		t.Fatal("gpg without fingerprint must fail closed")
	}
	// dev-hmac in production is refused.
	if _, err := SignerFromEnv(func(k string) string {
		if k == "EXC_SOLVENCY_SIGNER" {
			return "dev-hmac"
		}
		return ""
	}, "production"); err == nil {
		t.Fatal("dev-hmac in production must fail closed")
	}
	// dev-hmac elsewhere signs, clearly labelled.
	sg, err := SignerFromEnv(func(k string) string {
		if k == "EXC_SOLVENCY_SIGNER" {
			return "dev-hmac"
		}
		return ""
	}, "dev")
	if err != nil {
		t.Fatalf("dev signer: %v", err)
	}
	sig, fpr, kind, err := sg.Sign(context.Background(), []byte("payload"))
	if err != nil {
		t.Fatalf("dev sign: %v", err)
	}
	if !strings.HasPrefix(sig, "DEV-HMAC-SHA256:") || fpr != "DEV-HMAC" || kind != SignerKindDevHMAC {
		t.Fatalf("dev signature not labelled: sig=%q fpr=%q kind=%q", sig, fpr, kind)
	}
	// Unknown kind refuses.
	if _, err := SignerFromEnv(func(string) string { return "hsm-exotic" }, "dev"); err == nil {
		t.Fatal("unknown signer kind must fail closed")
	}
}
