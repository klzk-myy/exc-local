// Scratch-PG seeding for the live-stack legs. Rows are namespaced
// (itest-*@exc.local identities, ak_itest_* key ids) so they are
// attributable and cleanable; the migverify scratch DB is the designated
// test database.
package itest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool opens a pgx pool on the resolved scratch DSN.
func (e *Env) Pool(ctx context.Context) (*pgxpool.Pool, error) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	pool, err := pgxpool.New(cctx, e.PostgresDSN)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Fixture is the seeded identity set for one suite run. Two
// counterparty accounts exist because STP correctly refuses same-account
// crosses — the fill leg needs distinct principals.
type Fixture struct {
	UserID    int64
	AccountID int64   // trader A
	AccountB  int64   // counterparty B (same user)
	Key       *APIKey // HMAC trade key for A
	KeyB      *APIKey // HMAC trade key for B
	ReadKey   *APIKey // HMAC read-only key (scope enforcement leg)
	AllowKey  *APIKey // HMAC key IP-allowlisted to an unroutable probe IP
	DataKey   []byte  // the SecretBox key the gateway was booted with
}

// SeedFixture creates user + two accounts + funded balances + HMAC API
// keys sealed under dataKey (the same key the stack's gateway runs with).
func SeedFixture(ctx context.Context, pool *pgxpool.Pool, dataKey []byte, runTag string) (*Fixture, error) {
	fx := &Fixture{DataKey: dataKey}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email,status,kyc_status)
		 VALUES ($1,'ACTIVE','VERIFIED') RETURNING id`,
		fmt.Sprintf("itest-%s@exc.local", runTag)).Scan(&fx.UserID); err != nil {
		return nil, fmt.Errorf("seed user: %w", err)
	}
	for _, dst := range []*int64{&fx.AccountID, &fx.AccountB} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO accounts (user_id,account_type,kyc_tier,status,base_currency,settlement_intent)
			 VALUES ($1,'MARGIN','T2','ACTIVE','USD','ROLLING_MARGIN') RETURNING id`,
			fx.UserID).Scan(dst); err != nil {
			return nil, fmt.Errorf("seed account: %w", err)
		}
		for _, cur := range []string{"USD", "EUR"} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO balances (account_id,currency,available,locked)
				 VALUES ($1,$2,1000000,0)
				 ON CONFLICT (account_id,currency) DO UPDATE SET available=1000000, locked=0`,
				*dst, cur); err != nil {
				return nil, fmt.Errorf("seed balance %s: %w", cur, err)
			}
			// §5.3 ledger verification cache must mirror the seeded
			// wallet — the ledger service asserts
			// journal_sums.net_balance == balances.total on every post,
			// so a balance without a matching sum poisons every
			// money-moving leg (transfers, withdrawals, funding).
			if _, err := pool.Exec(ctx,
				`INSERT INTO journal_sums (account_id,currency,total_debits,total_credits,entry_count)
				 VALUES ($1,$2,1000000,0,0)
				 ON CONFLICT (account_id,currency) DO UPDATE
				 SET total_debits=EXCLUDED.total_debits, total_credits=0`,
				*dst, cur); err != nil {
				return nil, fmt.Errorf("seed journal_sums %s: %w", cur, err)
			}
		}
	}
	// The delegated p21gov legs publish ACTIVE execution policies into
	// this persistent scratch DB — once one exists, Task 14.3.7's
	// consent gate makes order entry close-only for any account lacking
	// an EXECUTION_POLICY consent row. Record the fixture's consent
	// exactly as ConsentExecPolicy does (dual write) so order legs
	// stay admissible regardless of scratch-DB policy residue.
	var polID int64
	var polVersion string
	polErr := pool.QueryRow(ctx,
		`SELECT id, version FROM execution_policies WHERE status='ACTIVE'`).
		Scan(&polID, &polVersion)
	switch {
	case polErr == nil:
		for _, acct := range []int64{fx.AccountID, fx.AccountB} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO execution_policy_consents
				    (account_id, policy_id, version, consented_by)
				 VALUES ($1,$2,$3,$4)
				 ON CONFLICT (account_id, policy_id) DO NOTHING`,
				acct, polID, polVersion, fx.UserID); err != nil {
				return nil, fmt.Errorf("consent execution_policy_consents: %w", err)
			}
			if _, err := pool.Exec(ctx,
				`INSERT INTO account_consents
				    (account_id, consent_type, doc_ref, consented_by)
				 VALUES ($1,'EXECUTION_POLICY',$2,$3)
				 ON CONFLICT (account_id, consent_type, doc_ref) DO NOTHING`,
				acct, polVersion, fx.UserID); err != nil {
				return nil, fmt.Errorf("consent account_consents: %w", err)
			}
		}
	case errors.Is(polErr, pgx.ErrNoRows):
		// pre-launch scratch DB — nothing to consent to
	default:
		return nil, fmt.Errorf("probe active execution policy: %w", polErr)
	}
	var err error
	kf := &Fixture{UserID: fx.UserID, AccountID: fx.AccountID}
	if fx.Key, err = seedAPIKey(ctx, pool, dataKey, kf, "itest-trade-"+runTag,
		[]string{"trade", "read"}, nil); err != nil {
		return nil, err
	}
	kf.AccountID = fx.AccountB
	if fx.KeyB, err = seedAPIKey(ctx, pool, dataKey, kf, "itest-tradeB-"+runTag,
		[]string{"trade", "read"}, nil); err != nil {
		return nil, err
	}
	kf.AccountID = fx.AccountID
	if fx.ReadKey, err = seedAPIKey(ctx, pool, dataKey, kf, "itest-read-"+runTag,
		[]string{"read"}, nil); err != nil {
		return nil, err
	}
	// IP allowlist leg: a key whose allowlist can never match (TEST-NET-1
	// is not our loopback) — used only to assert TOKEN_IP_FORBIDDEN.
	if fx.AllowKey, err = seedAPIKey(ctx, pool, dataKey, kf, "itest-ipallow-"+runTag,
		[]string{"trade", "read"}, []string{"192.0.2.0/24"}); err != nil {
		return nil, err
	}
	return fx, nil
}

// seedAPIKey inserts one api_keys row — the exact wire shape
// auth.KeyStore.CreateHMAC writes (HMAC + sealed secret + sha256 fp).
func seedAPIKey(ctx context.Context, pool *pgxpool.Pool, dataKey []byte,
	fx *Fixture, label string, scopes []string, ipAllow []string) (*APIKey, error) {
	keyID := "ak_itest_" + b64url(9)
	secret := b64url(32) // raw secret is the b64url string itself
	enc, err := SealSecret(dataKey, []byte(secret))
	if err != nil {
		return nil, fmt.Errorf("seal secret: %w", err)
	}
	var allow any
	if ipAllow != nil {
		allow = ipAllow
	}
	var id int64
	err = pool.QueryRow(ctx,
		`INSERT INTO api_keys
		 (key_id, account_id, user_id, key_hash, key_prefix, label, key_type,
		  algorithm, secret_enc, scopes, rate_limit_tier, ip_allowlist, status, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,'HMAC','HMAC-SHA256',$7,$8,'STANDARD',$9,'ACTIVE',$2)
		 RETURNING id`,
		keyID, fx.AccountID, fx.UserID, SecretKeyHash(secret), keyID[:8],
		label, enc, scopes, allow).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("seed api_key: %w", err)
	}
	return &APIKey{KeyID: keyID, Secret: secret, Account: fx.AccountID, User: fx.UserID, Scopes: scopes}, nil
}

// FundAccount tops up a wallet while keeping the §5.3 invariant —
// balances.available and journal_sums.total_debits move together so
// net_balance stays equal to balances.total for the next ledger post.
func FundAccount(ctx context.Context, pool *pgxpool.Pool, accountID int64,
	currency string, amount string) error {
	if _, err := pool.Exec(ctx,
		`UPDATE balances SET available=available+$2::numeric
		  WHERE account_id=$1 AND currency=$3`, accountID, amount, currency); err != nil {
		return fmt.Errorf("fund balances: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO journal_sums (account_id,currency,total_debits,total_credits,entry_count)
		 VALUES ($1,$3,$2::numeric,0,0)
		 ON CONFLICT (account_id,currency) DO UPDATE
		 SET total_debits=journal_sums.total_debits+EXCLUDED.total_debits`,
		accountID, amount, currency); err != nil {
		return fmt.Errorf("fund journal_sums: %w", err)
	}
	return nil
}

// SeedAdminBinding grants the fixture user an admin role binding —
// the row admin.ActiveBindings reads (status ACTIVE, unexpired).
func SeedAdminBinding(ctx context.Context, pool *pgxpool.Pool, userID int64, role string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO admin_role_bindings (user_id, role, kind, scope, granter_id, granted_at, expires_at, status)
		 VALUES ($1,$2,'STANDARD','{}'::jsonb,$3,now(),now()+interval '1 day','ACTIVE')`,
		userID, role, userID)
	return err
}

// ScrubDelegatedResidue removes rows left by the delegated
// admin.TestRBACLifecycleIntegration leg on a persistent scratch DB —
// its principals are fixed IDs (9200xx) and its campaign labels are
// UNIQUE, so residue fails re-runs with 23505. Best-effort: every
// statement tolerates an absent table (pre-migration scratch DBs).
func ScrubDelegatedResidue(ctx context.Context, pool *pgxpool.Pool) {
	for _, q := range []string{
		// Decisions snapshot EVERY active binding (not just 9200xx), so
		// clear by campaign first — otherwise the FK blocks the campaign
		// delete and stale 'IT-<unix%1M>' labels collide with a same-second
		// re-run of the lifecycle test.
		`DELETE FROM admin_recert_decisions
		  WHERE campaign_id IN
		    (SELECT id FROM admin_recert_campaigns WHERE label LIKE 'IT-%')`,
		`DELETE FROM admin_recert_decisions WHERE user_id BETWEEN 920000 AND 920099`,
		`DELETE FROM admin_recert_campaigns WHERE label LIKE 'IT-%'`,
		`DELETE FROM admin_break_glass_grants WHERE grantee_id BETWEEN 920000 AND 920099
		    OR granter_id BETWEEN 920000 AND 920099`,
		`DELETE FROM admin_role_bindings WHERE user_id BETWEEN 920000 AND 920099`,
		`DELETE FROM users WHERE id BETWEEN 920000 AND 920099`,
	} {
		_, _ = pool.Exec(ctx, q)
	}
}

// SeedEd25519Key registers an asymmetric API key row (public key only —
// §24 #283: private keys never enter the platform).
func SeedEd25519Key(ctx context.Context, pool *pgxpool.Pool, fx *Fixture,
	pubDER []byte, label string, scopes []string) (string, error) {
	keyID := "ak_itest_" + b64url(9)
	fp := sha256Sum(pubDER)
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO api_keys
		 (key_id, account_id, user_id, key_hash, key_prefix, label, key_type,
		  algorithm, public_key, scopes, rate_limit_tier, status, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,'ED25519','EdDSA',$7,$8,'STANDARD','ACTIVE',$2)
		 RETURNING id`,
		keyID, fx.AccountID, fx.UserID, fp, keyID[:8], label, pubDER, scopes).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("seed ed25519 key: %w", err)
	}
	return keyID, nil
}

func b64url(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha256Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---------------------------------------------------------------------------
// Read-side probes (assertion helpers)
// ---------------------------------------------------------------------------

// OrderRow is the read-model state asserted after the engine round-trip.
type OrderRow struct {
	ID        int64
	Status    string
	FilledQty string
	AvgPrice  *string
}

// LoadOrder fetches one orders row by id.
func LoadOrder(ctx context.Context, pool *pgxpool.Pool, id int64) (*OrderRow, error) {
	var o OrderRow
	err := pool.QueryRow(ctx,
		`SELECT id, status::text, filled_qty::text, avg_fill_price::text
		   FROM orders WHERE id=$1`, id).
		Scan(&o.ID, &o.Status, &o.FilledQty, &o.AvgPrice)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// CountWhere runs SELECT count(*) with a caller-built predicate — test
// code only, never exposed to request data.
func CountWhere(ctx context.Context, pool *pgxpool.Pool, table, where string, args ...any) (int64, error) {
	var n int64
	q := "SELECT count(*) FROM " + table + " WHERE " + where
	err := pool.QueryRow(ctx, q, args...).Scan(&n)
	return n, err
}
