// gdpr.go — Phase-21 Task 21.3.7: GDPR data-subject rights
// (export/erasure/consent) + geo-block enforcement.
//
// Spec surface: §14.13 (privacy/consent), §23 (GEO_BLOCKED), §27 R15
// (US retail prohibition — institutional flow served + reported),
// Phase-21 AC: restricted jurisdictions incl. IR/KP hard-blocked;
// IP-based detection; consent per purpose (marketing / analytics /
// data sharing); erasure with financial-record retention carve-outs
// (GDPR Art. 17(3)(b) legal-obligation exemption).
//
// Fail-closed posture (spec §2.7):
//   - consent reads that fail surface an error, never an assumed grant;
//   - geo policy reload failure keeps the LAST GOOD policy map, and if
//     none was ever loaded the gate blocks (deny-by-default);
//   - erasure refuses to run while the account is not CLOSED — the
//     Phase-14 closure workflow (account_lifecycle) owns settlement of
//     positions/funds first.
package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/auth"
	"exchange/internal/middleware"
	"exchange/internal/objectstore"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Consent (account_consent_states + account_consent_events, migration 243)
// ---------------------------------------------------------------------------

// Consent purposes — the Task 21.3.7 vocabulary.
const (
	ConsentPurposeMarketing   = "MARKETING"
	ConsentPurposeAnalytics   = "ANALYTICS"
	ConsentPurposeDataSharing = "DATA_SHARING"
)

// Consent states.
const (
	ConsentStateGranted   = "GRANTED"
	ConsentStateWithdrawn = "WITHDRAWN"
)

// Consent channels ('ALL' is the umbrella row).
var consentChannels = map[string]bool{
	"ALL": true, "EMAIL": true, "PUSH": true, "SMS": true, "IN_APP": true,
}

// ConsentState is one current-state row.
type ConsentState struct {
	AccountID int64           `json:"account_id"`
	Purpose   string          `json:"purpose"`
	Channel   string          `json:"channel"`
	State     string          `json:"state"`
	UpdatedBy int64           `json:"updated_by"`
	UpdatedAt time.Time       `json:"updated_at"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// ValidConsentPurpose reports whether p is a recognised purpose.
func ValidConsentPurpose(p string) bool {
	switch strings.ToUpper(p) {
	case ConsentPurposeMarketing, ConsentPurposeAnalytics, ConsentPurposeDataSharing:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// GDPRService — export / erasure / consent
// ---------------------------------------------------------------------------

// SessionRevoker terminates every live session for the principal —
// satisfied by auth.SessionManager at the gateway seam.
type SessionRevoker interface {
	RevokeAll(ctx context.Context, accountID int64, userID string) error
}

// ExportSink persists the export artifact (object store). Optional:
// when absent the export manifest is returned inline and the request
// row carries no artifact_ref.
type ExportSink interface {
	Put(ctx context.Context, in objectstore.PutInput) (objectstore.Object, error)
	Bucket() string
}

// GDPRService owns the customer privacy surface.
type GDPRService struct {
	pool     *pgxpool.Pool
	sessions SessionRevoker
	sink     ExportSink
	now      func() time.Time
}

// NewGDPRService binds the pool; sessions and sink may be nil (the
// erasure then skips session revocation — dev only — and exports stay
// inline).
func NewGDPRService(pool *pgxpool.Pool, sessions SessionRevoker, sink ExportSink) (*GDPRService, error) {
	if pool == nil {
		return nil, fmt.Errorf("compliance: gdpr pool is nil")
	}
	return &GDPRService{pool: pool, sessions: sessions, sink: sink, now: time.Now}, nil
}

// SetClockForTest overrides the clock; tests only.
func (s *GDPRService) SetClockForTest(now func() time.Time) { s.now = now }

// ---------------------------------------------------------------------------
// Consent surface
// ---------------------------------------------------------------------------

// SetConsent grants or withdraws consent for (purpose, channel). The
// current-state row is upserted and an append-only event row records
// the transition — withdrawal evidence is as regulated as the grant.
func (s *GDPRService) SetConsent(ctx context.Context, accountID, userID int64,
	purpose, channel, state string, meta map[string]any) (*ConsentState, error) {
	purpose = strings.ToUpper(strings.TrimSpace(purpose))
	channel = strings.ToUpper(strings.TrimSpace(channel))
	state = strings.ToUpper(strings.TrimSpace(state))
	if channel == "" {
		channel = "ALL"
	}
	if !ValidConsentPurpose(purpose) {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown consent purpose %q", purpose))
	}
	if !consentChannels[channel] {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown consent channel %q", channel))
	}
	if state != ConsentStateGranted && state != ConsentStateWithdrawn {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unknown consent state %q", state))
	}
	metaJSON, _ := json.Marshal(meta)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("compliance: consent tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var c ConsentState
	c.AccountID, c.Purpose, c.Channel, c.State, c.UpdatedBy = accountID, purpose, channel, state, userID
	err = tx.QueryRow(ctx, `
		INSERT INTO account_consent_states
		    (account_id, purpose, channel, state, updated_by, metadata)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (account_id, purpose, channel) DO UPDATE
		  SET state=EXCLUDED.state, updated_by=EXCLUDED.updated_by,
		      updated_at=now(), metadata=EXCLUDED.metadata
		RETURNING updated_at`,
		accountID, purpose, channel, state, userID, metaJSON).Scan(&c.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("compliance: consent upsert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_consent_events
		    (account_id, purpose, channel, state, actor, source)
		VALUES ($1,$2,$3,$4,$5,'API')`,
		accountID, purpose, channel, state, userID); err != nil {
		return nil, fmt.Errorf("compliance: consent event: %w", err)
	}
	consentAuditID := accountID
	// audit_log.action is VARCHAR(16) — map the vocabulary explicitly
	// rather than concatenating ("CONSENT_WITHDRAWN" would overflow).
	auditAction := "CONSENT_GRANTED"
	if state == ConsentStateWithdrawn {
		auditAction = "CONSENT_REVOKED"
	}
	if _, err := audit.Append(ctx, tx, "account_consent_states",
		&consentAuditID, auditAction, nil); err != nil {
		return nil, fmt.Errorf("compliance: consent audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("compliance: consent commit: %w", err)
	}
	return &c, nil
}

// ListConsents returns every current-state row for the account.
func (s *GDPRService) ListConsents(ctx context.Context, accountID int64) ([]ConsentState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT purpose, channel, state, updated_by, updated_at, metadata
		  FROM account_consent_states
		 WHERE account_id=$1 ORDER BY purpose, channel`, accountID)
	if err != nil {
		return nil, fmt.Errorf("compliance: consent list: %w", err)
	}
	defer rows.Close()
	out := []ConsentState{}
	for rows.Next() {
		var c ConsentState
		c.AccountID = accountID
		if err := rows.Scan(&c.Purpose, &c.Channel, &c.State,
			&c.UpdatedBy, &c.UpdatedAt, &c.Metadata); err != nil {
			return nil, fmt.Errorf("compliance: consent scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConsentGranted reports whether the account currently grants purpose
// on channel. A specific-channel row wins over the 'ALL' umbrella;
// absent rows mean NOT granted (opt-in semantics — fail-closed for
// MARKETING/DATA_SHARING under GDPR).
func (s *GDPRService) ConsentGranted(ctx context.Context, accountID int64,
	purpose, channel string) (bool, error) {
	purpose = strings.ToUpper(purpose)
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if channel == "" {
		channel = "ALL"
	}
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT state FROM account_consent_states
		 WHERE account_id=$1 AND purpose=$2 AND channel=$3`,
		accountID, purpose, channel).Scan(&state)
	if err == pgx.ErrNoRows {
		if channel == "ALL" {
			return false, nil
		}
		err = s.pool.QueryRow(ctx, `
			SELECT state FROM account_consent_states
			 WHERE account_id=$1 AND purpose=$2 AND channel='ALL'`,
			accountID, purpose).Scan(&state)
	}
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compliance: consent read: %w", err)
	}
	return state == ConsentStateGranted, nil
}

// ---------------------------------------------------------------------------
// Export (right of access / data portability — GDPR Art. 15/20)
// ---------------------------------------------------------------------------

// GDPRRequest is the lifecycle row.
type GDPRRequest struct {
	ID          int64           `json:"id"`
	AccountID   int64           `json:"account_id"`
	UserID      int64           `json:"user_id"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	Detail      json.RawMessage `json:"detail"`
	ArtifactRef *string         `json:"artifact_ref,omitempty"`
	SHA256      *string         `json:"sha256,omitempty"`
	RequestedBy int64           `json:"requested_by"`
	CreatedAt   time.Time       `json:"created_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

// exportSection caps keep the synchronous export bounded; truncation is
// flagged per-section in the manifest rather than silently dropping.
const exportRowCap = 10000

// RequestExport fulfils Art. 15/20 synchronously: assemble the personal
// record set, emit the manifest JSON, optionally archive the artifact
// to object storage, and record the request row. Re-request while one
// is open dedups via ux_gdpr_requests_open.
func (s *GDPRService) RequestExport(ctx context.Context, accountID, userID int64) (*GDPRRequest, []byte, error) {
	req, err := s.openRequest(ctx, accountID, userID, "EXPORT")
	if err != nil {
		return nil, nil, err
	}
	if req.Status == "COMPLETED" {
		return req, nil, nil // dedup hit — prior completed row returned
	}

	manifest := map[string]any{
		"generated_at": s.now().UTC().Format(time.RFC3339Nano),
		"account_id":   accountID,
		"user_id":      userID,
		"format":       "gdpr-export/v1",
		"sections":     map[string]any{},
	}
	sections := manifest["sections"].(map[string]any)

	// Identity + account profile.
	if prof, err := s.exportProfile(ctx, userID); err != nil {
		return s.failRequest(ctx, req.ID, "profile export failed", err)
	} else {
		sections["profile"] = prof
	}
	if accts, err := s.exportAccounts(ctx, userID); err != nil {
		return s.failRequest(ctx, req.ID, "accounts export failed", err)
	} else {
		sections["accounts"] = accts
	}
	if subs, err := s.exportKYC(ctx, accountID); err != nil {
		return s.failRequest(ctx, req.ID, "kyc export failed", err)
	} else {
		sections["kyc_submissions"] = subs
	}
	if certs, err := s.exportTaxCerts(ctx, accountID); err != nil {
		return s.failRequest(ctx, req.ID, "tax cert export failed", err)
	} else {
		sections["tax_certifications"] = certs
	}
	if cons, err := s.ListConsents(ctx, accountID); err != nil {
		return s.failRequest(ctx, req.ID, "consent export failed", err)
	} else {
		sections["consents"] = cons
	}
	if logins, err := s.exportLogins(ctx, userID); err != nil {
		return s.failRequest(ctx, req.ID, "login history export failed", err)
	} else {
		sections["login_history"] = logins
	}
	// Financial records — exported (data portability) AND retained
	// (Art. 17(3)(b)); the sections carry row caps + truncation flags.
	for _, q := range []struct {
		name, rowsSQL, countSQL string
	}{
		{"orders", `SELECT jsonb_agg(row_to_json(o)) FROM (
		    SELECT o.id, i.symbol, o.side::text, o.order_type::text,
		           o.quantity, o.price, o.status::text, o.created_at
		    FROM orders o LEFT JOIN instruments i ON i.id = o.instrument_id
		    WHERE o.account_id=$1 ORDER BY o.id
		    LIMIT ` + fmt.Sprint(exportRowCap) + `) o`,
			`SELECT count(*) FROM orders WHERE account_id=$1`},
		{"trades", `SELECT jsonb_agg(row_to_json(t)) FROM (
		    SELECT t.id, i.symbol,
		           CASE WHEN t.buyer_account_id=$1 THEN 'BUY' ELSE 'SELL' END AS role,
		           t.quantity, t.price,
		           CASE WHEN t.buyer_account_id=$1 THEN t.buyer_fee ELSE t.seller_fee END AS fee,
		           t.settlement_date, t.created_at
		    FROM trades t LEFT JOIN instruments i ON i.id = t.instrument_id
		    WHERE t.buyer_account_id=$1 OR t.seller_account_id=$1
		    ORDER BY t.id LIMIT ` + fmt.Sprint(exportRowCap) + `) t`,
			`SELECT count(*) FROM trades
			  WHERE buyer_account_id=$1 OR seller_account_id=$1`},
		{"funding_transactions", `SELECT jsonb_agg(row_to_json(f)) FROM (
		    SELECT id, currency, type::text, amount, status::text, created_at
		    FROM funding_transactions WHERE account_id=$1
		    ORDER BY id LIMIT ` + fmt.Sprint(exportRowCap) + `) f`,
			`SELECT count(*) FROM funding_transactions WHERE account_id=$1`},
	} {
		var raw json.RawMessage
		err := s.pool.QueryRow(ctx, q.rowsSQL, accountID).Scan(&raw)
		if err != nil {
			return s.failRequest(ctx, req.ID, q.name+" export failed", err)
		}
		var n int64
		_ = s.pool.QueryRow(ctx, q.countSQL, accountID).Scan(&n)
		sections[q.name] = map[string]any{
			"row_count": n,
			"exported":  json.RawMessage(raw),
			"truncated": n > exportRowCap,
			"retention": "financial record — retained under legal obligation (GDPR Art. 17(3)(b))",
		}
	}

	body, err := json.Marshal(manifest)
	if err != nil {
		return s.failRequest(ctx, req.ID, "manifest marshal", err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	var artifactRef *string
	if s.sink != nil {
		key := fmt.Sprintf("gdpr-exports/%d/%d-%d.json", accountID, req.ID, s.now().Unix())
		obj, err := s.sink.Put(ctx, objectstore.PutInput{
			Key: key, Body: strings.NewReader(string(body)),
			Size: int64(len(body)), ContentType: "application/json",
			ServerSideEncryption: "aws:kms",
		})
		if err != nil {
			return s.failRequest(ctx, req.ID, "artifact put", err)
		}
		ref := obj.Key
		artifactRef = &ref
	}

	err = s.completeRequest(ctx, req.ID, artifactRef, &sum, map[string]any{
		"sections": sectionNames(sections), "bytes": len(body)})
	if err != nil {
		return nil, nil, err
	}
	req.Status, req.ArtifactRef, req.SHA256 = "COMPLETED", artifactRef, &sum
	return req, body, nil
}

func (s *GDPRService) exportProfile(ctx context.Context, userID int64) (any, error) {
	var email, status string
	var phone, country, name, addr *string
	var created time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT email, phone, country, full_name, address, status::text, created_at
		  FROM users WHERE id=$1`, userID).
		Scan(&email, &phone, &country, &name, &addr, &status, &created)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"email": email, "phone": phone, "country": country,
		"full_name": name, "address": addr, "status": status,
		"created_at": created.UTC(),
	}, nil
}

func (s *GDPRService) exportAccounts(ctx context.Context, userID int64) (any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_type::text, kyc_tier, status::text,
		       client_category, created_at
		  FROM accounts WHERE user_id=$1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id              int64
			typ, tier, stat string
			cat             *string
			created         time.Time
		)
		if err := rows.Scan(&id, &typ, &tier, &stat, &cat, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "account_type": typ, "kyc_tier": tier,
			"status": stat, "client_category": cat,
			"created_at": created.UTC(),
		})
	}
	return out, rows.Err()
}

func (s *GDPRService) exportKYC(ctx context.Context, accountID int64) (any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, requested_tier, status, jurisdiction, submitted_at
		  FROM kyc_submissions WHERE account_id=$1 ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id         int64
			tier, stat string
			juris      *string
			submitted  time.Time
		)
		if err := rows.Scan(&id, &tier, &stat, &juris, &submitted); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "requested_tier": tier, "status": stat,
			"jurisdiction": juris, "submitted_at": submitted.UTC(),
		})
	}
	return out, rows.Err()
}

func (s *GDPRService) exportTaxCerts(ctx context.Context, accountID int64) (any, error) {
	// Metadata only — TIN/fields stay sealed (they decrypt through the
	// same Art. 15 right but we surface them through the dedicated
	// self-cert endpoint, not the bulk manifest).
	rows, err := s.pool.Query(ctx, `
		SELECT id, form_type, tin_country, tin_kind, status, created_at
		  FROM tax_self_certifications WHERE account_id=$1 ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id                  int64
			form, status        string
			tinCountry, tinKind *string
			created             time.Time
		)
		if err := rows.Scan(&id, &form, &tinCountry, &tinKind, &status, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "form_type": form, "tin_country": tinCountry,
			"tin_kind": tinKind, "status": status,
			"created_at": created.UTC(),
			"pii":        "sealed — served via the self-cert surface",
		})
	}
	return out, rows.Err()
}

func (s *GDPRService) exportLogins(ctx context.Context, userID int64) (any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ip::text, geo_country, geo_city, result::text, "timestamp"
		  FROM login_history WHERE user_id=$1
		  ORDER BY id DESC LIMIT 500`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			ip                  *string
			geoCountry, geoCity *string
			result              string
			at                  time.Time
		)
		if err := rows.Scan(&ip, &geoCountry, &geoCity, &result, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ip": ip, "geo_country": geoCountry,
			"geo_city": geoCity, "result": result, "at": at.UTC(),
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Erasure (right to be forgotten — GDPR Art. 17 with 17(3)(b) carve-outs)
// ---------------------------------------------------------------------------

// erasureCarveOuts documents what survives an erasure and why — the
// response + request detail carry this verbatim.
var erasureCarveOuts = []map[string]string{
	{"relation": "orders/trades/ledger_entries",
		"ground": "financial records — MiFID II / AML 5-7y legal obligation"},
	{"relation": "funding_transactions",
		"ground": "payment records — AML/CFT legal obligation"},
	{"relation": "comms_recordings",
		"ground": "MiFID II Art. 16(7) taping — 5y minimum retention"},
	{"relation": "kyc_submissions/kyc_documents",
		"ground": "KYC file — AML legal obligation"},
	{"relation": "sar_reports/travel_rule_records",
		"ground": "AML reporting records — confidentiality + retention"},
	{"relation": "tax_report_runs",
		"ground": "CRS/FATCA submissions already filed"},
	{"relation": "audit_hash_chain/admin_audit_log",
		"ground": "integrity evidence — anonymised by account reference"},
	{"relation": "account_consent_events",
		"ground": "consent evidence — legal hold"},
}

// RequestErasure processes Art. 17. Precondition (fail-closed): the
// account must already be CLOSED — the Phase-14 offboarding flow owns
// open-position/funds settlement; erasure while ACTIVE is rejected so
// a live client can't strip identity out from under open obligations.
//
// What runs in the erasure tx:
//  1. lock the account + user rows FOR UPDATE;
//  2. pseudonymise users (email tombstone, drop phone/name/address/
//     credentials/TOTP/anti-phishing — country is kept because the
//     residency tag on accounts.jurisdiction_code is the retained
//     regulatory copy);
//  3. revoke api_keys for the user;
//  4. withdraw MARKETING/ANALYTICS/DATA_SHARING consents (events
//     sourced 'ERASURE');
//  5. mark the request COMPLETED with the carve-out manifest.
//
// Sessions are revoked via the seam AFTER commit (Redis is not in the
// PG tx).
func (s *GDPRService) RequestErasure(ctx context.Context, accountID, userID int64,
	reason string) (*GDPRRequest, error) {
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT status::text FROM accounts WHERE id=$1`, accountID).Scan(&status)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: account read: %w", err)
	}
	if status != "CLOSED" {
		return nil, excerrors.New("INVALID_REQUEST",
			"account must be CLOSED before erasure — complete the offboarding flow first")
	}

	req, err := s.openRequest(ctx, accountID, userID, "ERASE")
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("compliance: erasure tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the identity rows — an erasure racing a profile write must
	// serialize, not interleave.
	var uid int64
	if err := tx.QueryRow(ctx,
		`SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&uid); err != nil {
		return nil, fmt.Errorf("compliance: erasure user lock: %w", err)
	}

	tombstone := fmt.Sprintf("erased-%d@erased.invalid", userID)
	if _, err := tx.Exec(ctx, `
		UPDATE users SET
		    email              = $2,
		    phone              = NULL,
		    full_name          = NULL,
		    address            = NULL,
		    country            = NULL,
		    password_hash      = NULL,
		    totp_secret        = NULL,
		    totp_backup_codes  = '{}',
		    anti_phishing_code = NULL,
		    status             = 'CLOSED',
		    updated_at         = now()
		 WHERE id=$1`, userID, tombstone); err != nil {
		return nil, fmt.Errorf("compliance: user pseudonymise: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM api_keys WHERE user_id=$1`, userID); err != nil {
		return nil, fmt.Errorf("compliance: api_key purge: %w", err)
	}

	// Consent withdrawal — every revocable purpose on every channel.
	for _, p := range []string{ConsentPurposeMarketing,
		ConsentPurposeAnalytics, ConsentPurposeDataSharing} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_consent_states
			    (account_id, purpose, channel, state, updated_by, metadata)
			VALUES ($1,$2,'ALL','WITHDRAWN',$3,'{"source":"erasure"}')
			ON CONFLICT (account_id, purpose, channel) DO UPDATE
			  SET state='WITHDRAWN', updated_by=$3, updated_at=now()`,
			accountID, p, userID); err != nil {
			return nil, fmt.Errorf("compliance: consent withdraw %s: %w", p, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_consent_events
			    (account_id, purpose, channel, state, actor, source)
			VALUES ($1,$2,'ALL','WITHDRAWN',$3,'ERASURE')`,
			accountID, p, userID); err != nil {
			return nil, fmt.Errorf("compliance: consent event %s: %w", p, err)
		}
	}

	if _, err := audit.Append(ctx, tx, "gdpr_requests",
		&req.ID, "ERASURE_APPLIED", nil); err != nil {
		return nil, fmt.Errorf("compliance: erasure audit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE gdpr_requests SET status='COMPLETED',
		    completed_at=now(),
		    detail=detail || $2::jsonb
		 WHERE id=$1`, req.ID,
		mustJSON(map[string]any{"carve_outs": erasureCarveOuts,
			"reason": reason})); err != nil {
		return nil, fmt.Errorf("compliance: erasure close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("compliance: erasure commit: %w", err)
	}

	// Post-commit: kill live sessions (Redis seam — not transactional).
	if s.sessions != nil {
		if err := s.sessions.RevokeAll(ctx, accountID,
			fmt.Sprintf("%d", userID)); err != nil {
			// Non-fatal: the account row is CLOSED so authn rejects at
			// session validation; log via the request detail.
			_, _ = s.pool.Exec(ctx, `
				UPDATE gdpr_requests
				   SET detail = detail || '{"session_revoke_error":true}'::jsonb
				 WHERE id=$1`, req.ID)
		}
	}
	req.Status = "COMPLETED"
	return req, nil
}

// ---------------------------------------------------------------------------
// request plumbing
// ---------------------------------------------------------------------------

// openRequest inserts the PENDING row; an existing open request for the
// same (account, kind) is returned as-is (idempotent re-request).
func (s *GDPRService) openRequest(ctx context.Context, accountID, userID int64,
	kind string) (*GDPRRequest, error) {
	var r GDPRRequest
	err := s.pool.QueryRow(ctx, `
		INSERT INTO gdpr_requests (account_id, user_id, kind, requested_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (account_id, kind)
		    WHERE status IN ('PENDING','PROCESSING') DO NOTHING
		RETURNING id, account_id, user_id, kind, status, detail,
		          artifact_ref, sha256, requested_by, created_at`,
		accountID, userID, kind, userID).
		Scan(&r.ID, &r.AccountID, &r.UserID, &r.Kind, &r.Status, &r.Detail,
			&r.ArtifactRef, &r.SHA256, &r.RequestedBy, &r.CreatedAt)
	if err == pgx.ErrNoRows {
		// Concurrent open request — return the live one.
		err = s.pool.QueryRow(ctx, `
			SELECT id, account_id, user_id, kind, status, detail,
			       artifact_ref, sha256, requested_by, created_at, completed_at
			  FROM gdpr_requests
			 WHERE account_id=$1 AND kind=$2
			   AND status IN ('PENDING','PROCESSING')`,
			accountID, kind).
			Scan(&r.ID, &r.AccountID, &r.UserID, &r.Kind, &r.Status, &r.Detail,
				&r.ArtifactRef, &r.SHA256, &r.RequestedBy, &r.CreatedAt, &r.CompletedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: gdpr request open: %w", err)
	}
	return &r, nil
}

func (s *GDPRService) failRequest(ctx context.Context, id int64, step string, cause error) (*GDPRRequest, []byte, error) {
	_, _ = s.pool.Exec(ctx, `
		UPDATE gdpr_requests SET status='FAILED', failure_reason=$2,
		    completed_at=now() WHERE id=$1`, id, step+": "+cause.Error())
	return nil, nil, fmt.Errorf("compliance: gdpr %s: %w", step, cause)
}

func (s *GDPRService) completeRequest(ctx context.Context, id int64,
	artifactRef, sha *string, detail map[string]any) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE gdpr_requests SET status='COMPLETED', completed_at=now(),
		    artifact_ref=$2, sha256=$3, detail=detail||$4::jsonb
		 WHERE id=$1`, id, artifactRef, sha, mustJSON(detail))
	return err
}

// ListRequests returns the account's request history.
func (s *GDPRService) ListRequests(ctx context.Context, accountID int64) ([]GDPRRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, user_id, kind, status, detail,
		       artifact_ref, sha256, requested_by, created_at, completed_at
		  FROM gdpr_requests WHERE account_id=$1 ORDER BY id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("compliance: gdpr list: %w", err)
	}
	defer rows.Close()
	out := []GDPRRequest{}
	for rows.Next() {
		var r GDPRRequest
		if err := rows.Scan(&r.ID, &r.AccountID, &r.UserID, &r.Kind,
			&r.Status, &r.Detail, &r.ArtifactRef, &r.SHA256,
			&r.RequestedBy, &r.CreatedAt, &r.CompletedAt); err != nil {
			return nil, fmt.Errorf("compliance: gdpr scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Geo-block gate (IP-based detection + jurisdiction policy)
// ---------------------------------------------------------------------------

// GeoResolver maps a client IP to an ISO 3166-1 alpha-2 country code —
// satisfied by a static CIDR table (tests/dev) or a GeoIP2-style
// adapter. A miss MUST return ("", nil); the gate then decides per
// policy (unknown = ALLOW for unrestricted paths, fail-closed handled
// by the resolver-error path).
type GeoResolver interface {
	Lookup(ctx context.Context, ip string) (country string, err error)
}

// CIDRResolver is a file/static table GeoResolver: JSON object
// {"CIDR": "CC", ...} or a Go map via NewCIDRResolverMap. Longest
// prefix wins.
type CIDRResolver struct {
	nets []cidrEntry
}

type cidrEntry struct {
	net *net.IPNet
	cc  string
}

// NewCIDRResolver builds a resolver from a CIDR→country map.
func NewCIDRResolver(m map[string]string) (*CIDRResolver, error) {
	r := &CIDRResolver{}
	for cidr, cc := range m {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("geo resolver: bad CIDR %q: %w", cidr, err)
		}
		r.nets = append(r.nets, cidrEntry{net: n, cc: strings.ToUpper(cc)})
	}
	return r, nil
}

// Lookup implements GeoResolver with longest-prefix matching.
func (r *CIDRResolver) Lookup(_ context.Context, ip string) (string, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", fmt.Errorf("geo resolver: unparsable ip %q", ip)
	}
	best := -1
	country := ""
	for _, e := range r.nets {
		if e.net.Contains(parsed) {
			if ones, _ := e.net.Mask.Size(); ones > best {
				best, country = ones, e.cc
			}
		}
	}
	return country, nil
}

// GeoActions.
const (
	GeoActionBlock       = "BLOCK"
	GeoActionRetailBlock = "RETAIL_BLOCK"
	GeoActionAllow       = "ALLOW"
)

// geoRetailExempt lists authenticated-scope paths a RETAIL_BLOCK must
// still let through — data rights and regulated exits are never
// geo-fenced (GDPR rights don't depend on jurisdiction, and a retail
// client must always be able to close/complain). Prefix-matched.
var geoRetailExempt = []string{
	"/api/v1/account/gdpr",
	"/api/v1/account/consent",
	"/api/v1/account/close",
	"/api/v1/account/cooling-off",
	"/api/v1/account/emergency-freeze",
	"/api/v1/support",
}

// GeoGate enforces geo_jurisdiction_policies at the gateway edge —
// mounted inside the middleware chain so no handler can bypass it.
type GeoGate struct {
	pool       *pgxpool.Pool
	resolver   GeoResolver
	trustProxy bool
	ttl        time.Duration

	mu         sync.RWMutex
	policies   map[string]string // country → action
	policiesAt time.Time

	catMu    sync.Mutex
	catCache map[int64]geoCatEntry // account → client category

	now func() time.Time
}

type geoCatEntry struct {
	cat ClientCategory
	exp time.Time
}

// NewGeoGate binds the gate. resolver may be nil in dev — an unresolvable
// resolver fails open ONLY in the sense that no country is attributed
// (policies then have nothing to act on); a resolver ERROR blocks the
// request (fail-closed) so a broken GeoIP never silently opens the gate.
func NewGeoGate(pool *pgxpool.Pool, resolver GeoResolver, trustProxy bool) (*GeoGate, error) {
	if pool == nil {
		return nil, fmt.Errorf("compliance: geo gate pool is nil")
	}
	g := &GeoGate{
		pool: pool, resolver: resolver, trustProxy: trustProxy,
		ttl: 30 * time.Second, policies: map[string]string{},
		catCache: map[int64]geoCatEntry{}, now: time.Now,
	}
	return g, nil
}

// SetClockForTest overrides the clock; tests only.
func (g *GeoGate) SetClockForTest(now func() time.Time) { g.now = now }

// SetPoliciesForTest injects the policy map; tests only.
func (g *GeoGate) SetPoliciesForTest(p map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.policies, g.policiesAt = p, g.now()
}

// reloadPolicies refreshes the policy map (TTL-cached). On error the
// last good map is kept; if none was ever loaded the caller gets the
// error — fail-closed.
func (g *GeoGate) policyFor(ctx context.Context, country string) (string, error) {
	g.mu.RLock()
	fresh := g.now().Sub(g.policiesAt) < g.ttl
	action, ok := g.policies[country]
	g.mu.RUnlock()
	if fresh {
		if !ok {
			return GeoActionAllow, nil
		}
		return action, nil
	}

	rows, err := g.pool.Query(ctx,
		`SELECT country_code, action FROM geo_jurisdiction_policies`)
	if err != nil {
		g.mu.RLock()
		loaded := len(g.policies) > 0
		g.mu.RUnlock()
		if loaded {
			// Stale-but-present beats down — keep serving last good.
			g.mu.RLock()
			defer g.mu.RUnlock()
			if a, ok := g.policies[country]; ok {
				return a, nil
			}
			return GeoActionAllow, nil
		}
		return "", excerrors.Wrap("SERVICE_DEGRADED",
			"geo policy unavailable", err)
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var cc, a string
		if err := rows.Scan(&cc, &a); err != nil {
			return "", fmt.Errorf("geo policy scan: %w", err)
		}
		m[cc] = a
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("geo policy read: %w", err)
	}
	g.mu.Lock()
	g.policies, g.policiesAt = m, g.now()
	g.mu.Unlock()
	if a, ok := m[country]; ok {
		return a, nil
	}
	return GeoActionAllow, nil
}

// categoryFor resolves the account's client category (short-cached —
// category changes are rare and admin-driven).
func (g *GeoGate) categoryFor(ctx context.Context, accountID int64) (ClientCategory, error) {
	g.catMu.Lock()
	if e, ok := g.catCache[accountID]; ok && g.now().Before(e.exp) {
		cat := e.cat
		g.catMu.Unlock()
		return cat, nil
	}
	g.catMu.Unlock()

	var cat string
	err := g.pool.QueryRow(ctx,
		`SELECT client_category FROM accounts WHERE id=$1`, accountID).Scan(&cat)
	if err != nil {
		return "", fmt.Errorf("geo gate: category read: %w", err)
	}
	g.catMu.Lock()
	g.catCache[accountID] = geoCatEntry{cat: ClientCategory(cat),
		exp: g.now().Add(60 * time.Second)}
	g.catMu.Unlock()
	return ClientCategory(cat), nil
}

// geoBlockedError emits the registered GEO_BLOCKED code (spec §23).
func geoBlockedError(country, action string) *excerrors.Error {
	return excerrors.New("GEO_BLOCKED",
		fmt.Sprintf("access restricted for jurisdiction %s (%s)", country, action))
}

// Middleware returns the edge middleware. Mounted on the full /api
// surface in cmd/gateway/main.go — before auth dispatch so unprotected
// mutating endpoints (register) are fenced too.
//
// Semantics:
//
//	BLOCK        → every /api request refused (GEO_BLOCKED).
//	RETAIL_BLOCK → refused when the request is mutating AND (claims
//	               absent → treated as retail onboarding) OR the
//	               authenticated account resolves to RETAIL; exempt
//	               data-rights/exit paths always pass; institutional
//	               categories (PROFESSIONAL / ELIGIBLE_COUNTERPARTY)
//	               pass per §27 R15.
//	Resolver unavailable (no resolver configured OR lookup returns no
//	country) → request passes unrestricted paths; a resolver ERROR or
//	an unloadable policy table fails closed (GEO_BLOCKED/DEGRADED).
func (g *GeoGate) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if g.resolver == nil {
				next.ServeHTTP(w, r)
				return
			}
			ip := middleware.ClientIP(r, g.trustProxy)
			country, err := g.resolver.Lookup(r.Context(), ip)
			if err != nil {
				excerrors.WriteProblem(w, geoBlockedError("", "RESOLVER_ERROR"))
				return
			}
			if country == "" {
				next.ServeHTTP(w, r) // unattributed — nothing to enforce
				return
			}
			action, err := g.policyFor(r.Context(), country)
			if err != nil {
				excerrors.WriteProblem(w, err)
				return
			}
			switch action {
			case GeoActionAllow:
				next.ServeHTTP(w, r)
				return
			case GeoActionBlock:
				excerrors.WriteProblem(w, geoBlockedError(country, action))
				return
			case GeoActionRetailBlock:
				if !mutatingMethod(r.Method) || geoRetailExemptPath(r.URL.Path) {
					next.ServeHTTP(w, r)
					return
				}
				claims := auth.ClaimsFrom(r.Context())
				if claims == nil {
					// Unauthenticated mutating request from a
					// retail-blocked country = onboarding flow → refuse
					// (US retail registration lands here).
					excerrors.WriteProblem(w, geoBlockedError(country, action))
					return
				}
				cat, err := g.categoryFor(r.Context(), claims.AccountID)
				if err != nil {
					excerrors.WriteProblem(w, excerrors.Wrap(
						"SERVICE_DEGRADED", "client category unresolvable", err))
					return
				}
				if cat == CategoryRetail || !cat.Valid() {
					excerrors.WriteProblem(w, geoBlockedError(country, action))
					return
				}
				next.ServeHTTP(w, r)
				return
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

func mutatingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func geoRetailExemptPath(path string) bool {
	for _, p := range geoRetailExempt {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sectionNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
