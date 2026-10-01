// PgStore — the PostgreSQL implementation of the reporting Store seam
// (migration 054 tables). Mutations run in serializable transactions
// with the §5.40 retry ladder and carry their audit_hash_chain link
// in-tx (audit.Append nil-payload convention — the row itself is the
// hash preimage).
package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// PgStore implements Store over pgx.
type PgStore struct {
	Pool *pgxpool.Pool
}

// NewPgStore wraps a pool; nil pool fails closed at construction.
func NewPgStore(pool *pgxpool.Pool) (*PgStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("reporting: nil pg pool")
	}
	return &PgStore{Pool: pool}, nil
}

// ---------------------------------------------------------------------------
// Shared retry ladder (same SQLSTATE set as compliance/admin — spec §5.40)
// ---------------------------------------------------------------------------

var retryableSQLSTATE = map[string]bool{"23505": true, "40001": true, "40P01": true}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return retryableSQLSTATE[pgErr.Code]
	}
	return false
}

// inTx runs fn inside a serializable transaction, retrying the whole
// transaction on retryable conflicts (max 3 attempts).
func (s *PgStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return fmt.Errorf("reporting: begin tx: %w", err)
		}
		if err = fn(tx); err == nil {
			if err = tx.Commit(ctx); err == nil {
				return nil
			}
		}
		_ = tx.Rollback(ctx)
		lastErr = err
		if !isRetryable(err) {
			return err
		}
	}
	return excerrors.Wrap("TRANSACTION_CONFLICT_RETRY_EXHAUSTED",
		"reporting transaction failed after 3 attempts", lastErr)
}

func jsonOrEmpty(v json.RawMessage) any {
	if len(v) == 0 {
		return json.RawMessage("{}")
	}
	return v
}

func jsonListOrEmpty(v json.RawMessage) any {
	if len(v) == 0 {
		return json.RawMessage("[]")
	}
	return v
}

func nilStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ---------------------------------------------------------------------------
// Source resolution
// ---------------------------------------------------------------------------

// ResolveTrade — trades ⨝ instruments ⨝ orders ⨝ accounts. trades is
// partitioned on created_at; lookup by id alone scans all daily
// partitions — bounded fine for per-fill reporting volume.
func (s *PgStore) ResolveTrade(ctx context.Context, tradeID int64) (*TradeContext, error) {
	var (
		tc                    TradeContext
		settle                *time.Time
		shard, seq            *int64
		status                *string
		buyerAlgo, sellerAlgo *string
		buyerJur, sellerJur   *string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT t.id, t.instrument_id, i.symbol, i.instrument_type::text,
		       i.base_currency, i.quote_currency,
		       t.buy_order_id, t.sell_order_id,
		       t.buyer_account_id, t.seller_account_id,
		       ba.user_id, sa.user_id,
		       bo.algo_params->>'algo_id', so.algo_params->>'algo_id',
		       t.price::text, t.quantity::text, t.settlement_date,
		       t.shard_id, t.trade_seq,
		       COALESCE(t.status::text,'COMPLETED'), t.created_at,
		       (SELECT ks.jurisdiction FROM kyc_submissions ks
		          WHERE ks.account_id = t.buyer_account_id
		          ORDER BY ks.id DESC LIMIT 1),
		       (SELECT ks.jurisdiction FROM kyc_submissions ks
		          WHERE ks.account_id = t.seller_account_id
		          ORDER BY ks.id DESC LIMIT 1)
		  FROM trades t
		  JOIN instruments i  ON i.id  = t.instrument_id
		  JOIN orders bo      ON bo.id = t.buy_order_id
		  JOIN orders so      ON so.id = t.sell_order_id
		  JOIN accounts ba    ON ba.id = t.buyer_account_id
		  JOIN accounts sa    ON sa.id = t.seller_account_id
		 WHERE t.id = $1`, tradeID).Scan(
		&tc.TradeID, &tc.InstrumentID, &tc.InstrumentCode, &tc.InstrumentType,
		&tc.BaseCurrency, &tc.QuoteCurrency,
		&tc.BuyOrderID, &tc.SellOrderID,
		&tc.BuyerAccountID, &tc.SellerAccountID,
		&tc.BuyerUserID, &tc.SellerUserID,
		&buyerAlgo, &sellerAlgo,
		&tc.Price, &tc.Quantity, &settle, &shard, &seq,
		&status, &tc.ExecutedAt, &buyerJur, &sellerJur)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: resolve trade %d: %w", tradeID, err)
	}
	if buyerJur != nil {
		tc.BuyerJurisdiction = *buyerJur
	}
	if sellerJur != nil {
		tc.SellerJurisdiction = *sellerJur
	}
	tc.SettlementDate = settle
	if shard != nil {
		tc.ShardID = *shard
	}
	if seq != nil {
		tc.TradeSeq = *seq
	}
	if status != nil {
		tc.TradeStatus = *status
	}
	if buyerAlgo != nil {
		tc.BuyerAlgoID = *buyerAlgo
	}
	if sellerAlgo != nil {
		tc.SellerAlgoID = *sellerAlgo
	}
	return &tc, nil
}

// PartyFor — party_identifiers for one account; nil when unregistered
// (missing identifiers quarantine the report — never fabricated).
func (s *PgStore) PartyFor(ctx context.Context, accountID int64) (*PartyIdentifiers, error) {
	var (
		p                 PartyIdentifiers
		lei, nidType, nid *string
		dmID, dmType      *string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT account_id, lei, national_id_type, national_id,
		       decision_maker_id, decision_maker_type::text
		  FROM party_identifiers WHERE account_id=$1`, accountID).Scan(
		&p.AccountID, &lei, &nidType, &nid, &dmID, &dmType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: party %d: %w", accountID, err)
	}
	if lei != nil {
		p.LEI = *lei
	}
	if nidType != nil {
		p.NationalIDType = *nidType
	}
	if nid != nil {
		p.NationalID = *nid
	}
	if dmID != nil {
		p.DecisionMakerID = *dmID
	}
	if dmType != nil {
		p.DecisionMakerType = *dmType
	}
	return &p, nil
}

// UpsertParty — the Compliance Officer repair path for identifier
// corrections (spec §14.5 repair workflow). Atomic upsert + admin audit
// row + chain link.
func (s *PgStore) UpsertParty(ctx context.Context, p PartyIdentifiers, adminUserID int64, clientIP string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var prev *PartyIdentifiers
		if cur, err := s.PartyFor(ctx, p.AccountID); err == nil {
			prev = cur
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO party_identifiers
			    (account_id, lei, national_id_type, national_id,
			     decision_maker_id, decision_maker_type, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (account_id) DO UPDATE SET
			    lei=EXCLUDED.lei, national_id_type=EXCLUDED.national_id_type,
			    national_id=EXCLUDED.national_id,
			    decision_maker_id=EXCLUDED.decision_maker_id,
			    decision_maker_type=EXCLUDED.decision_maker_type,
			    updated_by=EXCLUDED.updated_by, updated_at=now()`,
			p.AccountID, nilStr(p.LEI), nilStr(p.NationalIDType),
			nilStr(p.NationalID), nilStr(p.DecisionMakerID),
			nilStr(p.DecisionMakerType), adminUserID)
		if err != nil {
			return fmt.Errorf("reporting: upsert party %d: %w", p.AccountID, err)
		}
		before := map[string]any{}
		if prev != nil {
			before["party"] = prev
		}
		_, _, err = admin.Log(ctx, tx, admin.AuditEntry{
			AdminUserID: adminUserID,
			Action:      "regreport.party_upsert",
			TargetType:  "account",
			TargetID:    &p.AccountID,
			BeforeState: before,
			AfterState:  map[string]any{"party": p},
			IPAddress:   clientIP,
		})
		return err
	})
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

const eventCols = `event_id, uti, usi, upi, prior_uti, prior_usi,
	regime::text, action_type::text, event_type::text, report_seq,
	trade_id, position_id, instrument_id, instrument_code, instrument_type,
	account_id, counterparty_account_id,
	buyer_lei, seller_lei, buyer_id_type, buyer_id, seller_id_type, seller_id,
	decision_maker_type, decision_maker_id, trader_id, algo_id,
	venue_mic, jurisdiction, dual_sided,
	price::text, quantity::text, notional::text, currency,
	valuation, margin, clearing, confirmation, allocation, payload,
	schema_version, status::text, validation_errors,
	dissemination_due_at, supersedes_event_id, event_ts, reported_at, created_at`

func scanEvent(row pgx.Row) (*Event, error) {
	var e Event
	var (
		usi, upi, puti, pusi                 *string
		tradeID, posID, instID, acctID, cpID *int64
		buyerLEI, sellerLEI                  *string
		bidType, bid, sidType, sid           *string
		dmType, dmID, trader, algo           *string
		mic, jur                             *string
		price, qty, notional, ccy            *string
		schema                               *string
		instType                             *string
	)
	err := row.Scan(
		&e.EventID, &e.UTI, &usi, &upi, &puti, &pusi,
		&e.Regime, &e.Action, &e.EventType, &e.ReportSeq,
		&tradeID, &posID, &instID, &e.InstrumentCode, &instType,
		&acctID, &cpID,
		&buyerLEI, &sellerLEI, &bidType, &bid, &sidType, &sid,
		&dmType, &dmID, &trader, &algo,
		&mic, &jur, &e.DualSided,
		&price, &qty, &notional, &ccy,
		&e.Valuation, &e.Margin, &e.Clearing, &e.Confirmation, &e.Allocation,
		&e.Payload, &schema, &e.Status, &e.ValidationErrors,
		&e.DisseminationDueAt, &e.SupersedesEventID, &e.EventTS,
		&e.ReportedAt, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&e.USI, usi)
	set(&e.UPI, upi)
	set(&e.PriorUTI, puti)
	set(&e.PriorUSI, pusi)
	set(&e.BuyerLEI, buyerLEI)
	set(&e.SellerLEI, sellerLEI)
	set(&e.BuyerIDType, bidType)
	set(&e.BuyerID, bid)
	set(&e.SellerIDType, sidType)
	set(&e.SellerID, sid)
	set(&e.DecisionMakerType, dmType)
	set(&e.DecisionMakerID, dmID)
	set(&e.TraderID, trader)
	set(&e.AlgoID, algo)
	set(&e.VenueMIC, mic)
	set(&e.Jurisdiction, jur)
	set(&e.Price, price)
	set(&e.Quantity, qty)
	set(&e.Notional, notional)
	set(&e.Currency, ccy)
	set(&e.SchemaVersion, schema)
	set(&e.InstrumentType, instType)
	if tradeID != nil {
		e.TradeID = *tradeID
	}
	if posID != nil {
		e.PositionID = *posID
	}
	if instID != nil {
		e.InstrumentID = *instID
	}
	if acctID != nil {
		e.AccountID = *acctID
	}
	if cpID != nil {
		e.CounterpartyAccountID = *cpID
	}
	return &e, nil
}

// InsertEvent appends the immutable event + its audit chain link.
// The (uti, regime, report_seq) unique key is the collision-reject —
// a duplicate insert surfaces 23505 → the caller maps it to an
// ID_COLLISION break, never a silent overwrite.
func (s *PgStore) InsertEvent(ctx context.Context, e *Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO regulatory_report_events
			    (uti, usi, upi, prior_uti, prior_usi, regime, action_type,
			     event_type, report_seq, trade_id, position_id, instrument_id,
			     instrument_code, instrument_type, account_id,
			     counterparty_account_id, buyer_lei, seller_lei,
			     buyer_id_type, buyer_id, seller_id_type, seller_id,
			     decision_maker_type, decision_maker_id, trader_id, algo_id,
			     venue_mic, jurisdiction, dual_sided,
			     price, quantity, notional, currency,
			     valuation, margin, clearing, confirmation, allocation,
			     payload, schema_version, status, validation_errors,
			     dissemination_due_at, supersedes_event_id, event_ts, reported_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
			        $17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,
			        $30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,
			        $43,$44,$45,$46)
			RETURNING event_id, created_at`,
			e.UTI, nilStr(e.USI), nilStr(e.UPI), nilStr(e.PriorUTI),
			nilStr(e.PriorUSI), string(e.Regime), string(e.Action),
			string(e.EventType), e.ReportSeq, nilInt64(e.TradeID),
			nilInt64(e.PositionID), nilInt64(e.InstrumentID),
			e.InstrumentCode, e.InstrumentType, nilInt64(e.AccountID),
			nilInt64(e.CounterpartyAccountID), nilStr(e.BuyerLEI),
			nilStr(e.SellerLEI), nilStr(e.BuyerIDType), nilStr(e.BuyerID),
			nilStr(e.SellerIDType), nilStr(e.SellerID),
			nilStr(e.DecisionMakerType), nilStr(e.DecisionMakerID),
			nilStr(e.TraderID), nilStr(e.AlgoID),
			nilStr(e.VenueMIC), nilStr(e.Jurisdiction), e.DualSided,
			nilStr(e.Price), nilStr(e.Quantity), nilStr(e.Notional),
			nilStr(e.Currency),
			jsonOrEmpty(e.Valuation), jsonOrEmpty(e.Margin),
			jsonOrEmpty(e.Clearing), jsonOrEmpty(e.Confirmation),
			jsonOrEmpty(e.Allocation), jsonOrEmpty(e.Payload),
			nilStr(e.SchemaVersion), string(e.Status),
			jsonListOrEmpty(e.ValidationErrors),
			e.DisseminationDueAt, e.SupersedesEventID, e.EventTS,
			e.ReportedAt).Scan(&e.EventID, &e.CreatedAt)
		if err != nil {
			return fmt.Errorf("reporting: insert event uti=%s: %w", e.UTI, err)
		}
		_, err = audit.Append(ctx, tx, "regulatory_report_events",
			&e.EventID, "INSERT", nil)
		return err
	})
}

// EventByID loads one event or nil.
func (s *PgStore) EventByID(ctx context.Context, eventID int64) (*Event, error) {
	e, err := scanEvent(s.Pool.QueryRow(ctx,
		`SELECT `+eventCols+` FROM regulatory_report_events WHERE event_id=$1`,
		eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: event %d: %w", eventID, err)
	}
	return e, nil
}

// LatestEvent — highest report_seq for (uti, regime), or nil.
func (s *PgStore) LatestEvent(ctx context.Context, uti string, regime Regime) (*Event, error) {
	e, err := scanEvent(s.Pool.QueryRow(ctx, `
		SELECT `+eventCols+` FROM regulatory_report_events
		 WHERE uti=$1 AND regime=$2
		 ORDER BY report_seq DESC LIMIT 1`, uti, string(regime)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: latest event %s/%s: %w", uti, regime, err)
	}
	return e, nil
}

// NextReportSeq — latest.report_seq + 1, or 1 for the first row.
func (s *PgStore) NextReportSeq(ctx context.Context, uti string, regime Regime) (int, error) {
	var seq *int
	err := s.Pool.QueryRow(ctx, `
		SELECT max(report_seq) FROM regulatory_report_events
		 WHERE uti=$1 AND regime=$2`, uti, string(regime)).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("reporting: report_seq %s: %w", uti, err)
	}
	if seq == nil {
		return 1, nil
	}
	return *seq + 1, nil
}

// UTIOwner — the first trade_id the UTI was minted for (ID_COLLISION
// detection): any (uti) row's trade_id; two distinct trade ids on one
// UTI is a collision.
func (s *PgStore) UTIOwner(ctx context.Context, uti string) (int64, bool, error) {
	var tid *int64
	err := s.Pool.QueryRow(ctx, `
		SELECT trade_id FROM regulatory_report_events
		 WHERE uti=$1 AND trade_id IS NOT NULL
		 ORDER BY event_id LIMIT 1`, uti).Scan(&tid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reporting: uti owner %s: %w", uti, err)
	}
	if tid == nil {
		return 0, false, nil
	}
	return *tid, true, nil
}

// SetEventStatus — status transition + validation error record.
// Status changes are lifecycle bookkeeping (the event fields stay
// immutable); QUARANTINED carries the pinned-rule misses.
func (s *PgStore) SetEventStatus(ctx context.Context, eventID int64, status EventStatus, validationErrors json.RawMessage) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE regulatory_report_events
		   SET status=$2, validation_errors=$3,
		       reported_at = CASE WHEN $2 IN ('SUBMITTED','ACCEPTED','REJECTED')
		                          AND reported_at IS NULL THEN now()
		                          ELSE reported_at END
		 WHERE event_id=$1`,
		eventID, string(status), jsonListOrEmpty(validationErrors))
	if err != nil {
		return fmt.Errorf("reporting: event %d status %s: %w", eventID, status, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("regulatory event %d not found", eventID))
	}
	return nil
}

// ExportEvents — the report export feed over event_ts.
func (s *PgStore) ExportEvents(ctx context.Context, regime Regime, from, to *time.Time, limit int) ([]Event, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	q := `SELECT ` + eventCols + ` FROM regulatory_report_events WHERE regime=$1`
	args := []any{string(regime)}
	if from != nil {
		args = append(args, *from)
		q += fmt.Sprintf(" AND event_ts >= $%d", len(args))
	}
	if to != nil {
		args = append(args, *to)
		q += fmt.Sprintf(" AND event_ts < $%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY event_ts, event_id LIMIT $%d", len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("reporting: export %s: %w", regime, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("reporting: export scan: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// OpenEventsForReconcile — latest event per uti for the regime that is
// NOT terminal (not TERM/ERRO action), used as the "repository-accepted
// open derivatives" proxy in the daily reconciliation.
func (s *PgStore) OpenEventsForReconcile(ctx context.Context, regime Regime) ([]Event, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+eventCols+` FROM regulatory_report_events e
		 WHERE e.regime=$1
		   AND e.report_seq = (SELECT max(e2.report_seq)
		                         FROM regulatory_report_events e2
		                        WHERE e2.uti=e.uti AND e2.regime=e.regime)
		   AND e.action_type NOT IN ('TERM','ERRO')
		   AND e.status IN ('ACCEPTED','VALIDATED','SUBMITTED')`,
		string(regime))
	if err != nil {
		return nil, fmt.Errorf("reporting: open events %s: %w", regime, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("reporting: reconcile scan: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// LatestEvents — newest row per uti in the regime regardless of
// action/status (the reconciler's full repository-side view).
func (s *PgStore) LatestEvents(ctx context.Context, regime Regime) ([]Event, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+eventCols+` FROM regulatory_report_events e
		 WHERE e.regime=$1
		   AND e.report_seq = (SELECT max(e2.report_seq)
		                         FROM regulatory_report_events e2
		                        WHERE e2.uti=e.uti AND e2.regime=e.regime)
		 ORDER BY e.event_ts`, string(regime))
	if err != nil {
		return nil, fmt.Errorf("reporting: latest events %s: %w", regime, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("reporting: latest events scan: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// DuplicateNEWT — utis carrying more than one live NEWT row (DUPLICATE
// reconciliation feed).
func (s *PgStore) DuplicateNEWT(ctx context.Context, regime Regime) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT uti FROM regulatory_report_events
		 WHERE regime=$1 AND action_type='NEWT'
		 GROUP BY uti HAVING COUNT(*) > 1`, string(regime))
	if err != nil {
		return nil, fmt.Errorf("reporting: duplicate newt %s: %w", regime, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CollidingUTIs — utis bound to more than one distinct trade_id across
// regimes (ID_COLLISION sweep).
func (s *PgStore) CollidingUTIs(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT uti FROM regulatory_report_events
		 WHERE trade_id IS NOT NULL
		 GROUP BY uti HAVING COUNT(DISTINCT trade_id) > 1`)
	if err != nil {
		return nil, fmt.Errorf("reporting: uti collision sweep: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Artifact journal
// ---------------------------------------------------------------------------

const subCols = `report_submission_id, event_id, regime::text,
	destination::text, attempt, schema_name, schema_version, payload,
	payload_xml, payload_hash, status::text, external_ref, error_code,
	error_text, batch_id, submitted_at, resolved_at, created_at`

// subColsRS is subCols qualified to the rs alias for the JOIN queries —
// regulatory_report_events also carries event_id, so a bare reference is
// ambiguous there (SQLSTATE 42702).
var subColsRS = "rs." + strings.ReplaceAll(subCols, ", ", ", rs.")

func scanSubmission(row pgx.Row) (*Submission, error) {
	var s Submission
	var xml, ref, ecode, etext, batch *string
	err := row.Scan(&s.ReportSubmissionID, &s.EventID, &s.Regime,
		&s.Destination, &s.Attempt, &s.SchemaName, &s.SchemaVersion,
		&s.Payload, &xml, &s.PayloadHash, &s.Status, &ref, &ecode, &etext,
		&batch, &s.SubmittedAt, &s.ResolvedAt, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	if xml != nil {
		s.PayloadXML = *xml
	}
	if ref != nil {
		s.ExternalRef = *ref
	}
	if ecode != nil {
		s.ErrorCode = *ecode
	}
	if etext != nil {
		s.ErrorText = *etext
	}
	if batch != nil {
		s.BatchID = *batch
	}
	return &s, nil
}

// InsertSubmission appends an artifact row (attempt uniqueness enforces
// the immutable-history contract).
func (s *PgStore) InsertSubmission(ctx context.Context, sub *Submission) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO regulatory_report_submissions
			    (event_id, regime, destination, attempt, schema_name,
			     schema_version, payload, payload_xml, payload_hash,
			     status, batch_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING report_submission_id, created_at`,
			sub.EventID, string(sub.Regime), string(sub.Destination),
			sub.Attempt, sub.SchemaName, sub.SchemaVersion, sub.Payload,
			nilStr(sub.PayloadXML), sub.PayloadHash, string(sub.Status),
			nilStr(sub.BatchID)).Scan(&sub.ReportSubmissionID, &sub.CreatedAt)
		if err != nil {
			return fmt.Errorf("reporting: insert submission event=%d: %w",
				sub.EventID, err)
		}
		_, err = audit.Append(ctx, tx, "regulatory_report_submissions",
			&sub.ReportSubmissionID, "INSERT", nil)
		return err
	})
}

// SubmissionByID loads one artifact row or nil.
func (s *PgStore) SubmissionByID(ctx context.Context, reportSubmissionID int64) (*Submission, error) {
	sub, err := scanSubmission(s.Pool.QueryRow(ctx,
		`SELECT `+subCols+` FROM regulatory_report_submissions
		 WHERE report_submission_id=$1`, reportSubmissionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: submission %d: %w", reportSubmissionID, err)
	}
	return sub, nil
}

// SubmissionsForEvent — all attempts, oldest first (immutable history).
func (s *PgStore) SubmissionsForEvent(ctx context.Context, eventID int64) ([]Submission, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT `+subCols+` FROM regulatory_report_submissions
		 WHERE event_id=$1 ORDER BY attempt`, eventID)
	if err != nil {
		return nil, fmt.Errorf("reporting: submissions event %d: %w", eventID, err)
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, fmt.Errorf("reporting: submission scan: %w", err)
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

// PendingSubmissions — dispatch feed: PENDING artifacts whose event is
// VALIDATED (quarantined events never produce submittable artifacts),
// oldest first.
func (s *PgStore) PendingSubmissions(ctx context.Context, limit int) ([]Submission, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+subColsRS+` FROM regulatory_report_submissions rs
		 JOIN regulatory_report_events e ON e.event_id = rs.event_id
		 WHERE rs.status='PENDING' AND e.status IN ('VALIDATED','SUBMITTED')
		 ORDER BY rs.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("reporting: pending submissions: %w", err)
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, fmt.Errorf("reporting: pending scan: %w", err)
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

// MarkSubmissionDispatched — PENDING → SUBMITTED.
func (s *PgStore) MarkSubmissionDispatched(ctx context.Context, reportSubmissionID int64, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE regulatory_report_submissions
		   SET status='SUBMITTED', submitted_at=$2
		 WHERE report_submission_id=$1 AND status='PENDING'`,
		reportSubmissionID, at)
	if err != nil {
		return fmt.Errorf("reporting: dispatch %d: %w", reportSubmissionID, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("TRANSACTION_CONFLICT",
			fmt.Sprintf("submission %d not PENDING", reportSubmissionID))
	}
	return nil
}

// IngestAckTx — the repository verdict lands atomically: ack row +
// submission status + event status + (on NACK) a NACK_REPAIR break.
func (s *PgStore) IngestAckTx(ctx context.Context, a Ack, submissionStatus SubmissionStatus,
	eventStatus EventStatus, nackBreak bool, slaDueAt time.Time) (*Break, error) {
	var br *Break
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO regulatory_report_acks
			    (report_submission_id, event_id, ack_status, ack_code,
			     ack_text, external_ref, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING ack_id, received_at`,
			a.ReportSubmissionID, a.EventID, string(a.AckStatus),
			nilStr(a.AckCode), nilStr(a.AckText), nilStr(a.ExternalRef),
			jsonOrEmpty(a.Payload)).Scan(&a.AckID, &a.ReceivedAt)
		if err != nil {
			return fmt.Errorf("reporting: insert ack: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE regulatory_report_submissions
			   SET status=$2, external_ref=COALESCE($3, external_ref),
			       error_code=$4, error_text=$5, resolved_at=$6
			 WHERE report_submission_id=$1`,
			a.ReportSubmissionID, string(submissionStatus),
			nilStr(a.ExternalRef), nilStr(a.AckCode), nilStr(a.AckText),
			a.ReceivedAt); err != nil {
			return fmt.Errorf("reporting: ack→submission: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE regulatory_report_events
			   SET status=$2, reported_at=COALESCE(reported_at,$3)
			 WHERE event_id=$1`,
			a.EventID, string(eventStatus), a.ReceivedAt); err != nil {
			return fmt.Errorf("reporting: ack→event: %w", err)
		}
		if nackBreak {
			detail, _ := json.Marshal(map[string]any{
				"ack_code": a.AckCode, "ack_text": a.AckText,
				"report_submission_id": a.ReportSubmissionID,
			})
			var uti *string
			var regime *string
			if err := tx.QueryRow(ctx, `
				SELECT uti, regime::text FROM regulatory_report_events
				 WHERE event_id=$1`, a.EventID).Scan(&uti, &regime); err != nil {
				return fmt.Errorf("reporting: ack event lookup: %w", err)
			}
			var bID int64
			err := tx.QueryRow(ctx, `
				INSERT INTO regulatory_report_breaks
				    (event_id, uti, regime, break_type, detected_by, detail, sla_due_at)
				VALUES ($1,$2,$3,'NACK_REPAIR','ack',$4,$5)
				RETURNING break_id`,
				a.EventID, uti, regime, detail, slaDueAt).Scan(&bID)
			if err != nil {
				return fmt.Errorf("reporting: nack break: %w", err)
			}
			br = &Break{BreakID: bID, EventID: a.EventID,
				BreakType: BreakNackRepair, Status: BreakOpen,
				DetectedBy: "ack", SLADueAt: slaDueAt}
			if uti != nil {
				br.UTI = *uti
			}
		}
		_, err = audit.Append(ctx, tx, "regulatory_report_acks",
			&a.AckID, "INSERT", nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return br, nil
}

// AcksForEvent — the immutable ack feed for one event (detail view).
func (s *PgStore) AcksForEvent(ctx context.Context, eventID int64) ([]Ack, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT ack_id, report_submission_id, event_id, ack_status::text,
		       ack_code, ack_text, external_ref, payload, received_at
		  FROM regulatory_report_acks
		 WHERE event_id=$1 ORDER BY received_at, ack_id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("reporting: acks event %d: %w", eventID, err)
	}
	defer rows.Close()
	var out []Ack
	for rows.Next() {
		var a Ack
		var code, text, ref *string
		if err := rows.Scan(&a.AckID, &a.ReportSubmissionID, &a.EventID,
			&a.AckStatus, &code, &text, &ref, &a.Payload,
			&a.ReceivedAt); err != nil {
			return nil, fmt.Errorf("reporting: ack scan: %w", err)
		}
		if code != nil {
			a.AckCode = *code
		}
		if text != nil {
			a.AckText = *text
		}
		if ref != nil {
			a.ExternalRef = *ref
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Breaks
// ---------------------------------------------------------------------------

// InsertBreak appends a reconciliation/validation break + chain link.
func (s *PgStore) InsertBreak(ctx context.Context, b *Break) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var regime *string
		if b.Regime != "" {
			r := string(b.Regime)
			regime = &r
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO regulatory_report_breaks
			    (event_id, uti, regime, break_type, detected_by, detail,
			     sla_due_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING break_id, detected_at`,
			nilInt64(b.EventID), nilStr(b.UTI), regime, string(b.BreakType),
			b.DetectedBy, jsonOrEmpty(b.Detail), b.SLADueAt).
			Scan(&b.BreakID, &b.DetectedAt)
		if err != nil {
			return fmt.Errorf("reporting: insert break: %w", err)
		}
		b.Status = BreakOpen
		_, err = audit.Append(ctx, tx, "regulatory_report_breaks",
			&b.BreakID, "INSERT", nil)
		return err
	})
}

// OpenBreaks — the repair queue feed (OPEN/REPAIRING, oldest SLA first).
func (s *PgStore) OpenBreaks(ctx context.Context, regime Regime, limit int) ([]Break, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT break_id, event_id, uti, regime::text, break_type::text,
	             status::text, detected_by, detail, sla_due_at, detected_at,
	             resolved_at, resolved_by, notes
	        FROM regulatory_report_breaks
	       WHERE status IN ('OPEN','REPAIRING')`
	args := []any{}
	if regime != "" {
		args = append(args, string(regime))
		q += fmt.Sprintf(" AND regime=$%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY sla_due_at, break_id LIMIT $%d", len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("reporting: open breaks: %w", err)
	}
	defer rows.Close()
	var out []Break
	for rows.Next() {
		var b Break
		var (
			eid     *int64
			uti, rg *string
			notes   *string
			rby     *int64
		)
		if err := rows.Scan(&b.BreakID, &eid, &uti, &rg, &b.BreakType,
			&b.Status, &b.DetectedBy, &b.Detail, &b.SLADueAt, &b.DetectedAt,
			&b.ResolvedAt, &rby, &notes); err != nil {
			return nil, fmt.Errorf("reporting: break scan: %w", err)
		}
		if eid != nil {
			b.EventID = *eid
		}
		if uti != nil {
			b.UTI = *uti
		}
		if rg != nil {
			b.Regime = Regime(*rg)
		}
		if rby != nil {
			b.ResolvedBy = *rby
		}
		if notes != nil {
			b.Notes = *notes
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ResolveBreakTx — Compliance Officer repair-queue disposition:
// OPEN/REPAIRING → RESOLVED/WONT_FIX with admin attribution + audit row.
func (s *PgStore) ResolveBreakTx(ctx context.Context, breakID int64, status BreakStatus,
	resolvedBy int64, notes string, at time.Time, clientIP string) (bool, error) {
	if status != BreakResolved && status != BreakWontFix {
		return false, excerrors.New("INVALID_REQUEST",
			"break resolution must be RESOLVED or WONT_FIX")
	}
	applied := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var cur BreakStatus
		err := tx.QueryRow(ctx, `
			SELECT status::text FROM regulatory_report_breaks
			 WHERE break_id=$1 FOR UPDATE`, breakID).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("break %d not found", breakID))
		}
		if err != nil {
			return fmt.Errorf("reporting: lock break %d: %w", breakID, err)
		}
		if cur == BreakResolved || cur == BreakWontFix {
			return nil // already resolved — idempotent
		}
		tag, err := tx.Exec(ctx, `
			UPDATE regulatory_report_breaks
			   SET status=$2, resolved_at=$3, resolved_by=$4, notes=$5
			 WHERE break_id=$1`, breakID, string(status), at,
			resolvedBy, nilStr(notes))
		if err != nil {
			return fmt.Errorf("reporting: resolve break %d: %w", breakID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		_, _, err = admin.Log(ctx, tx, admin.AuditEntry{
			AdminUserID: resolvedBy,
			Action:      "regreport.break_resolve",
			TargetType:  "regulatory_report_break",
			TargetID:    &breakID,
			BeforeState: map[string]any{"status": string(cur)},
			AfterState:  map[string]any{"status": string(status), "notes": notes},
			IPAddress:   clientIP,
		})
		if err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

// ---------------------------------------------------------------------------
// Schema registry
// ---------------------------------------------------------------------------

// ActiveSchema — the pinned ruleset effective at `at` (nil when none).
func (s *PgStore) ActiveSchema(ctx context.Context, regulation, schemaName string, at time.Time) (*SchemaVersion, error) {
	var sv SchemaVersion
	var req json.RawMessage
	var to *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT schema_id, regulation, schema_name, version, rules_ref,
		       required_fields, effective_from, effective_to, active, created_at
		  FROM regulatory_schema_versions
		 WHERE regulation=$1 AND schema_name=$2 AND active
		   AND effective_from <= $3::date
		   AND (effective_to IS NULL OR effective_to >= $3::date)
		 ORDER BY effective_from DESC LIMIT 1`,
		regulation, schemaName, at).Scan(
		&sv.SchemaID, &sv.Regulation, &sv.SchemaName, &sv.Version,
		&sv.RulesRef, &req, &sv.EffectiveFrom, &to, &sv.Active, &sv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: schema %s/%s: %w", regulation, schemaName, err)
	}
	sv.EffectiveTo = to
	if err := json.Unmarshal(req, &sv.RequiredFields); err != nil {
		return nil, fmt.Errorf("reporting: schema %s rules: %w", schemaName, err)
	}
	return &sv, nil
}

// ---------------------------------------------------------------------------
// Reconciliation reads
// ---------------------------------------------------------------------------

// OpenDerivativePositions — internal open derivative exposure (positions
// on non-SPOT instruments, quantity <> 0).
func (s *PgStore) OpenDerivativePositions(ctx context.Context) ([]PositionSnapshot, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.account_id, p.instrument_id, p.side::text,
		       p.quantity::text, p.mark_price::text, p.margin_used::text,
		       i.symbol, p.updated_at
		  FROM positions p
		  JOIN instruments i ON i.id = p.instrument_id
		 WHERE p.quantity <> 0 AND i.instrument_type <> 'SPOT'`)
	if err != nil {
		return nil, fmt.Errorf("reporting: open derivative positions: %w", err)
	}
	defer rows.Close()
	var out []PositionSnapshot
	for rows.Next() {
		var p PositionSnapshot
		if err := rows.Scan(&p.PositionID, &p.AccountID, &p.InstrumentID,
			&p.Side, &p.Quantity, &p.MarkPrice, &p.MarginUsed,
			&p.InstrumentCode, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("reporting: position scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// CFTC limits (Task 21.3.9)
// ---------------------------------------------------------------------------

// ActiveLimitFor — instrument-specific row wins; else the instrument_type
// class default (instrument_id IS NULL).
func (s *PgStore) ActiveLimitFor(ctx context.Context, instrumentID int64, instrumentType, pair string, at time.Time) (*CFTCLimit, error) {
	var l CFTCLimit
	var instID *int64
	var spot, all *string
	var effTo *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT limit_id, instrument_id, instrument_type,
		       COALESCE(currency_pair,''), spot_month_limit::text,
		       all_months_limit::text, large_trader_threshold::text,
		       currency, effective_from, effective_to
		  FROM cftc_position_limits
		 WHERE (instrument_id=$1 OR instrument_id IS NULL)
		   AND instrument_type=$2
		   AND effective_from <= $4::date
		   AND (effective_to IS NULL OR effective_to >= $4::date)
		   AND (currency_pair IS NULL OR currency_pair='' OR currency_pair=$3)
		 ORDER BY instrument_id NULLS LAST, effective_from DESC
		 LIMIT 1`, instrumentID, instrumentType, pair, at).Scan(
		&l.LimitID, &instID, &l.InstrumentType, &l.CurrencyPair,
		&spot, &all, &l.LargeTraderThreshold, &l.Currency,
		&l.EffectiveFrom, &effTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reporting: cftc limit: %w", err)
	}
	l.InstrumentID = instID
	l.SpotMonthLimit = spot
	l.AllMonthsLimit = all
	l.EffectiveTo = effTo
	return &l, nil
}

// OpenQuantityFor — the account's aggregate open quantity on the
// instrument (absolute exposure across both sides).
func (s *PgStore) OpenQuantityFor(ctx context.Context, accountID, instrumentID int64) (string, error) {
	var q *string
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(sum(quantity),0)::text
		  FROM positions
		 WHERE account_id=$1 AND instrument_id=$2`,
		accountID, instrumentID).Scan(&q)
	if err != nil {
		return "0", fmt.Errorf("reporting: open qty: %w", err)
	}
	if q == nil {
		return "0", nil
	}
	return *q, nil
}
