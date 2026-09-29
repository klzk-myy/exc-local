// Phase-13 Task 13.3.7 — PostgreSQL persistence for solvency snapshots
// (migration 209).
package reconciliation

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// SolvencyPgStore is the production LiabilitySource + SnapshotStore over pgx.
type SolvencyPgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore binds the store to the OLTP pool.
func NewSolvencyStore(pool *pgxpool.Pool) (*SolvencyPgStore, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "reconciliation: pool is nil")
	}
	return &SolvencyPgStore{pool: pool}, nil
}

// ActiveLiabilities extracts every outstanding client liability: one row
// per (account_id, currency) in balances whose account is not CLOSED —
// plus any non-zero residual on a CLOSED account (a closed account with
// a balance is still owed; excluding it would understate liabilities).
// Zero-balance rows are INCLUDED (task edge case: inclusion proof must
// exist for them too). Negative totals are surfaced to the caller — the
// generator aborts the run on them, they are never silently filtered.
func (s *SolvencyPgStore) ActiveLiabilities(ctx context.Context) ([]Liability, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.account_id, b.currency, b.total::text
		  FROM balances b
		  JOIN accounts a ON a.id = b.account_id
		 WHERE a.status <> 'CLOSED' OR b.total <> 0
		 ORDER BY b.account_id, b.currency`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency liabilities", err)
	}
	defer rows.Close()
	out := []Liability{}
	for rows.Next() {
		var l Liability
		var total string
		if err := rows.Scan(&l.AccountID, &l.Currency, &total); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency liabilities scan", err)
		}
		d, err := decimal.NewFromString(total)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency liability decode", err)
		}
		l.Balance = d
		out = append(out, l)
	}
	return out, rows.Err()
}

// NostroAssets aggregates ACTIVE nostro balances per currency — the
// asset side of the attestation (migration 018; internally maintained,
// see the honesty note in service.go).
func (s *SolvencyPgStore) NostroAssets(ctx context.Context) (map[string]decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency, COALESCE(sum(balance),0)::text
		  FROM nostro_accounts
		 WHERE status = 'ACTIVE'
		 GROUP BY currency`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency nostro assets", err)
	}
	defer rows.Close()
	out := map[string]decimal.Decimal{}
	for rows.Next() {
		var ccy, total string
		if err := rows.Scan(&ccy, &total); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency nostro scan", err)
		}
		d, err := decimal.NewFromString(total)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "solvency nostro decode", err)
		}
		out[ccy] = d
	}
	return out, rows.Err()
}

// InsertSnapshot writes the snapshot row plus every leaf proof in one
// transaction — a crash leaves no half-published attestation.
func (s *SolvencyPgStore) InsertSnapshot(ctx context.Context, snap *Snapshot, proofs []proofRow) (int64, error) {
	lj, err := json.Marshal(snap.Liabilities)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "liabilities encode", err)
	}
	aj, err := json.Marshal(snap.NostroAssets)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "assets encode", err)
	}
	rj, err := json.Marshal(snap.ReserveRatios)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "ratios encode", err)
	}
	root, err := ParseHexDigest(snap.MerkleRoot)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "root decode", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "snapshot tx begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO solvency_snapshots
		 (generated_at, leaf_count, merkle_root, liabilities, nostro_assets,
		  reserve_ratios, solvent, signed_payload, signature,
		  signer_fingerprint, signer_kind)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id`,
		snap.GeneratedAt, snap.LeafCount, root[:], lj, aj, rj,
		snap.Solvent, snap.SignedPayload, snap.Signature,
		snap.SignerFingerprint, snap.SignerKind).Scan(&id)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "snapshot insert", err)
	}
	for _, p := range proofs {
		pj, err := json.Marshal(p.Path)
		if err != nil {
			return 0, excerrors.Wrap("INTERNAL_ERROR", "proof path encode", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO solvency_proofs
			 (snapshot_id, account_id, currency, balance, salt,
			  leaf_index, leaf_hash, path)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, p.AccountID, p.Currency, p.Balance.String(), p.Salt[:],
			p.LeafIndex, p.LeafHash[:], pj); err != nil {
			return 0, excerrors.Wrap("INTERNAL_ERROR", "proof insert", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "snapshot commit", err)
	}
	return id, nil
}

const snapshotCols = `id, generated_at, leaf_count, merkle_root, liabilities,
	nostro_assets, reserve_ratios, solvent, signed_payload, signature,
	signer_fingerprint, signer_kind`

func scanSnapshot(row pgx.Row) (*Snapshot, error) {
	var s Snapshot
	var root []byte
	var lj, aj, rj []byte
	err := row.Scan(&s.ID, &s.GeneratedAt, &s.LeafCount, &root,
		&lj, &aj, &rj, &s.Solvent, &s.SignedPayload, &s.Signature,
		&s.SignerFingerprint, &s.SignerKind)
	if err != nil {
		return nil, err
	}
	s.MerkleRoot = hex.EncodeToString(root)
	if err := json.Unmarshal(lj, &s.Liabilities); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(aj, &s.NostroAssets); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rj, &s.ReserveRatios); err != nil {
		return nil, err
	}
	return &s, nil
}

// LatestSnapshot returns the most recent published snapshot.
func (s *SolvencyPgStore) LatestSnapshot(ctx context.Context) (*Snapshot, error) {
	snap, err := scanSnapshot(s.pool.QueryRow(ctx,
		`SELECT `+snapshotCols+` FROM solvency_snapshots ORDER BY id DESC LIMIT 1`))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "no solvency snapshot published yet")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "latest snapshot", err)
	}
	return snap, nil
}

func scanProof(rows pgx.Rows) ([]ProofResult, error) {
	defer rows.Close()
	out := []ProofResult{}
	for rows.Next() {
		var p ProofResult
		var salt, leafHash []byte
		var pj []byte
		var balStr string
		if err := rows.Scan(&p.SnapshotID, &p.GeneratedAt, &p.MerkleRoot,
			&p.AccountID, &p.Currency, &balStr, &salt, &p.LeafIndex,
			&leafHash, &pj); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "proof scan", err)
		}
		p.Balance = balStr
		p.Salt = hex.EncodeToString(salt)
		p.LeafHash = hex.EncodeToString(leafHash)
		if err := json.Unmarshal(pj, &p.Path); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "proof path decode", err)
		}
		if p.Path == nil {
			p.Path = []ProofStepJSON{}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProofFor returns the caller's leaf + sibling path in the LATEST
// snapshot — proofs always attest against the published root.
func (s *SolvencyPgStore) ProofFor(ctx context.Context, accountID int64, currency string) (*ProofResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.snapshot_id, s.generated_at, encode(s.merkle_root,'hex'),
		       p.account_id, p.currency, p.balance::text, p.salt, p.leaf_index,
		       p.leaf_hash, p.path
		  FROM solvency_proofs p
		  JOIN solvency_snapshots s ON s.id = p.snapshot_id
		 WHERE p.account_id = $1 AND p.currency = $2
		   AND p.snapshot_id = (SELECT max(id) FROM solvency_snapshots)`,
		accountID, currency)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "proof read", err)
	}
	list, err := scanProof(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, excerrors.New("NOT_FOUND",
			"no solvency proof for this account/currency in the latest snapshot")
	}
	return &list[0], nil
}

// ProofsForAccount returns every currency leaf for the account in the
// latest snapshot.
func (s *SolvencyPgStore) ProofsForAccount(ctx context.Context, accountID int64) ([]ProofResult, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.snapshot_id, s.generated_at, encode(s.merkle_root,'hex'),
		       p.account_id, p.currency, p.balance::text, p.salt, p.leaf_index,
		       p.leaf_hash, p.path
		  FROM solvency_proofs p
		  JOIN solvency_snapshots s ON s.id = p.snapshot_id
		 WHERE p.account_id = $1
		   AND p.snapshot_id = (SELECT max(id) FROM solvency_snapshots)
		 ORDER BY p.currency`, accountID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "proofs read", err)
	}
	return scanProof(rows)
}
