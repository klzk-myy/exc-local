// Phase-13 Task 13.3.7 — daily solvency snapshot generation.
//
// Generate runs at 22:00 UTC (NY close / FX EOD — deploy/crons/
// solvency-tree.sh → `exchange solvency-tree`) and:
//
//  1. extracts every client liability — one row per (account, currency)
//     in balances joined to accounts; CLOSED accounts contribute only
//     non-zero residuals (a closed account with a remaining balance is
//     still a liability — fail closed, never understated);
//  2. FAILS THE RUN LOUDLY on any negative liability — a negative balance
//     is a ledger defect (retail NBP makes client balances non-negative);
//     publishing a tree that silently included or skipped it would be a
//     false attestation;
//  3. salts + builds the Merkle tree (O(n) hashing);
//  4. computes the nostro asset attestation per currency — aggregate
//     ACTIVE nostro_accounts balances (Phase-11 Task 11.3.6, migration
//  018. vs liabilities → reserve ratio, global solvent flag;
//  5. signs the canonical payload via the Signer seam (production:
//     cold-storage GPG — signers.go) and persists snapshot + per-leaf
//     proof rows in ONE transaction: a crashed run leaves nothing.
//
// Honesty note (task constraint): nostro_accounts rows are the
// internally-maintained correspondent-account ledger, not a third-party
// custodian attestation feed — currencies with no nostro row report
// assets=0 and therefore fail the ≥100% ratio whenever liabilities
// exist, which is the correct pessimistic posture.
package reconciliation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Liability is one client balance row feeding a leaf.
type Liability struct {
	AccountID int64
	Currency  string
	Balance   decimal.Decimal
}

// LiabilitySource reads the liability set (PgStore satisfies it).
type LiabilitySource interface {
	ActiveLiabilities(ctx context.Context) ([]Liability, error)
	NostroAssets(ctx context.Context) (map[string]decimal.Decimal, error)
}

// Snapshot is one solvency_snapshots row (JSON field shapes mirror the
// table columns).
type Snapshot struct {
	ID                int64              `json:"id"`
	GeneratedAt       time.Time          `json:"generated_at"`
	LeafCount         int64              `json:"leaf_count"`
	MerkleRoot        string             `json:"merkle_root"` // hex
	Liabilities       map[string]string  `json:"liabilities"` // currency → decimal string
	NostroAssets      map[string]string  `json:"nostro_assets"`
	ReserveRatios     map[string]*string `json:"reserve_ratios"` // null when liabilities are zero
	Solvent           bool               `json:"solvent"`
	SignedPayload     []byte             `json:"-"`
	Signature         string             `json:"signature"`
	SignerFingerprint string             `json:"signer_fingerprint"`
	SignerKind        string             `json:"signer_kind"`
}

// ProofResult is the client-facing inclusion proof: the caller's own
// salted leaf plus sibling digests — sibling positions carry hashes ONLY
// (no peer account ids / balances, task DoD).
type ProofResult struct {
	SnapshotID  int64           `json:"snapshot_id"`
	GeneratedAt time.Time       `json:"generated_at"`
	MerkleRoot  string          `json:"merkle_root"`
	AccountID   int64           `json:"account_id"`
	Currency    string          `json:"currency"`
	Balance     string          `json:"balance"`
	Salt        string          `json:"salt"` // hex — the caller's own leaf salt
	LeafIndex   int64           `json:"leaf_index"`
	LeafHash    string          `json:"leaf_hash"`
	Path        []ProofStepJSON `json:"path"` // leaf → root order
}

// ProofStepJSON is the storage/wire shape of a ProofStep.
type ProofStepJSON struct {
	Position string `json:"position"` // "left" | "right" — where the SIBLING sits
	Hash     string `json:"hash"`     // sibling digest, hex
}

// SnapshotStore persists snapshots + proofs (SolvencyPgStore satisfies it).
type SnapshotStore interface {
	InsertSnapshot(ctx context.Context, s *Snapshot, proofs []proofRow) (int64, error)
	LatestSnapshot(ctx context.Context) (*Snapshot, error)
	ProofFor(ctx context.Context, accountID int64, currency string) (*ProofResult, error)
	ProofsForAccount(ctx context.Context, accountID int64) ([]ProofResult, error)
}

// proofRow is one solvency_proofs row at insert time.
type proofRow struct {
	AccountID int64
	Currency  string
	Balance   decimal.Decimal
	Salt      [SaltBytes]byte
	LeafIndex int64
	LeafHash  [32]byte
	Path      []ProofStepJSON
}

// Signer produces the detached signature over the canonical snapshot
// payload. Implementations: GPGSigner (production, cold-storage key) and
// DevHMACSigner (non-production only — NOT a proof-of-reserves signature).
type Signer interface {
	// Sign returns (signature text, signer fingerprint, kind, error).
	Sign(ctx context.Context, payload []byte) (sig, fingerprint, kind string, err error)
}

// SolvencyService generates and serves solvency snapshots.
type SolvencyService struct {
	src    LiabilitySource
	store  SnapshotStore
	signer Signer
	now    func() time.Time
	logf   func(format string, args ...any)
}

// NewService wires the generator; all three seams are mandatory (a nil
// signer would publish unsigned attestations — fail closed at wiring).
func NewSolvencyService(src LiabilitySource, store SnapshotStore, signer Signer) (*SolvencyService, error) {
	if src == nil || store == nil || signer == nil {
		return nil, fmt.Errorf("reconciliation: source, store and signer are required")
	}
	return &SolvencyService{src: src, store: store, signer: signer, now: time.Now}, nil
}

// WithLogger wires a diagnostic sink (nil = drop, funding convention).
func (s *SolvencyService) WithLogger(f func(format string, args ...any)) *SolvencyService {
	s.logf = f
	return s
}

// WithClock overrides the clock (tests).
func (s *SolvencyService) WithClock(c func() time.Time) *SolvencyService { s.now = c; return s }

func (s *SolvencyService) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// Generate builds, signs and persists one snapshot. Any defect aborts the
// run with a coded error BEFORE the insert — no partial or unsigned
// attestation is ever published.
func (s *SolvencyService) Generate(ctx context.Context) (*Snapshot, error) {
	liabs, err := s.src.ActiveLiabilities(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency: liabilities read", err)
	}
	// Negative-balance rejection: one bad row fails the whole run loudly.
	for _, l := range liabs {
		if l.Balance.IsNegative() {
			return nil, excerrors.New("INTERNAL_ERROR",
				fmt.Sprintf("solvency: negative liability account=%d currency=%s balance=%s — run aborted, snapshot NOT published",
					l.AccountID, l.Currency, l.Balance.String()))
		}
	}

	leaves := make([]LeafInput, 0, len(liabs))
	liabTotals := map[string]decimal.Decimal{}
	for _, l := range liabs {
		salt, err := newSalt()
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency: salt", err)
		}
		leaves = append(leaves, LeafInput{
			AccountID: l.AccountID, Currency: l.Currency,
			Balance: l.Balance, Salt: salt,
		})
		t := liabTotals[l.Currency]
		liabTotals[l.Currency] = t.Add(l.Balance)
	}
	tree := Build(leaves)
	root := tree.Root()

	assets, err := s.src.NostroAssets(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency: nostro assets read", err)
	}
	snap := &Snapshot{
		GeneratedAt:   s.now().UTC(),
		LeafCount:     int64(len(leaves)),
		MerkleRoot:    HexDigest(root),
		Liabilities:   decMapStrings(liabTotals),
		NostroAssets:  decMapStrings(assets),
		ReserveRatios: map[string]*string{},
		Solvent:       true,
	}
	// Reserve ratio per currency: assets / liabilities. A currency with
	// zero liabilities needs no cover (ratio null); a currency with
	// liabilities but no attested assets is ratio 0 → insolvent flag.
	for _, ccy := range unionKeys(liabTotals, assets) {
		l := liabTotals[ccy]
		if l.IsZero() {
			snap.ReserveRatios[ccy] = nil
			continue
		}
		a := assets[ccy]
		r := a.Div(l) // fixed-point ratio; liabilities > 0 here
		rs := r.String()
		snap.ReserveRatios[ccy] = &rs
		if a.LessThan(l) {
			snap.Solvent = false
		}
	}
	snap.SignedPayload = canonicalPayload(snap)
	sig, fpr, kind, err := s.signer.Sign(ctx, snap.SignedPayload)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency: root signing failed — snapshot NOT published", err)
	}
	snap.Signature, snap.SignerFingerprint, snap.SignerKind = sig, fpr, kind

	// Persist proof rows: sibling digests only — the stored path can never
	// disclose a peer's account_id or balance.
	proofs := make([]proofRow, 0, len(tree.Leaves))
	for i, l := range tree.Leaves {
		path, ok := tree.Proof(i)
		if !ok {
			return nil, excerrors.New("INTERNAL_ERROR",
				fmt.Sprintf("solvency: proof build failed at leaf %d", i))
		}
		// Self-check before persisting: a proof that doesn't fold to the
		// signed root must never be stored.
		if !Verify(l, path, root) {
			return nil, excerrors.New("INTERNAL_ERROR",
				fmt.Sprintf("solvency: self-verification failed at leaf %d", i))
		}
		steps := make([]ProofStepJSON, 0, len(path))
		for _, st := range path {
			pos := "left"
			if st.Right {
				pos = "right"
			}
			steps = append(steps, ProofStepJSON{Position: pos, Hash: HexDigest(st.Hash)})
		}
		proofs = append(proofs, proofRow{
			AccountID: l.AccountID, Currency: l.Currency, Balance: l.Balance,
			Salt: l.Salt, LeafIndex: int64(i),
			LeafHash: leafDigest(l), Path: steps,
		})
	}
	id, err := s.store.InsertSnapshot(ctx, snap, proofs)
	if err != nil {
		return nil, err // already coded by the store
	}
	snap.ID = id
	if !snap.Solvent {
		s.log("reconciliation: snapshot %d INSOLVENT — reserve ratio <100%% on at least one currency", id)
	}
	return snap, nil
}

// Latest returns the newest published snapshot (NOT_FOUND when none).
func (s *SolvencyService) Latest(ctx context.Context) (*Snapshot, error) {
	return s.store.LatestSnapshot(ctx)
}

// Proof returns the inclusion proof for (accountID, currency) against the
// latest snapshot (NOT_FOUND when absent).
func (s *SolvencyService) Proof(ctx context.Context, accountID int64, currency string) (*ProofResult, error) {
	return s.store.ProofFor(ctx, accountID, currency)
}

// Proofs returns every currency leaf for the account in the latest
// snapshot — the /account/solvency-proof convenience surface.
func (s *SolvencyService) Proofs(ctx context.Context, accountID int64) ([]ProofResult, error) {
	return s.store.ProofsForAccount(ctx, accountID)
}

// canonicalPayload is the exact byte string the signature covers — an
// unambiguous field list, pipe-separated, hex digest + RFC3339 timestamp +
// canonical (key-sorted) JSON maps.
func canonicalPayload(s *Snapshot) []byte {
	lj, _ := json.Marshal(s.Liabilities) // encoding/json sorts map keys
	aj, _ := json.Marshal(s.NostroAssets)
	rj, _ := json.Marshal(s.ReserveRatios)
	return []byte(fmt.Sprintf("EXC-SOLVENCY-SNAPSHOT\x00%s|%s|%d|%s|%s|%s",
		s.MerkleRoot, s.GeneratedAt.Format(time.RFC3339Nano),
		s.LeafCount, lj, aj, rj))
}

func decMapStrings(m map[string]decimal.Decimal) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	return out
}

func unionKeys(a, b map[string]decimal.Decimal) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
