// Travel Rule (FATF Recommendation 16) — Phase-21 Task 21.3.2.
//
// Every fiat transfer >= $1,000 USD carries originator + beneficiary
// identity: originator {name, account_number, address}, beneficiary
// {name, account_number} (spec §14.3, §24 #107; §27.1 Travel Rule
// matrix row → TRAVEL_RULE_MISSING_INFO 400 / TRAVEL_RULE_REJECTED 403).
//
// Outbound: funding.DispatchService calls EnforceOutbound inside the
// dispatch tx before the rail envelope is built. A complete record
// enriches OutboundPayment.Originator* — SwiftAdapter maps them onto
// MT103 field 50K; creditor fields land field 59. Missing fields → the
// record row persists (status MISSING_INFO) and the withdrawal queues
// HELD — the wire never dispatches without the required data
// (fail-closed, spec §2.7).
//
// Inbound: funding.DepositService calls CheckInbound inside the
// dual-source resolution tx; missing fields park the deposit in
// PENDING_REVIEW until an officer supplies them via SupplyInfo.
//
// Records are idempotent per (transfer_id, direction): sweep retries
// re-evaluate the same row, and stored fields always win over
// re-resolution so officer-supplied data is never overwritten.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/audit"
	"exchange/internal/funding"
	excerrors "exchange/pkg/errors"
)

// TravelRuleThresholdUSD is the FATF R.16 application floor — transfers
// at or above $1,000 USD equivalent require the full data set.
var TravelRuleThresholdUSD = decimal.RequireFromString("1000")

// Directions / record statuses (migration 032 CHECK mirrors).
const (
	TravelRuleInbound  = "INBOUND"
	TravelRuleOutbound = "OUTBOUND"

	TravelRuleMissing = "MISSING_INFO"
	TravelRuleOK      = "COMPLETE"
	TravelRuleDeny    = "REJECTED"
)

// TravelRuleParty is the IVMS-101-shaped identity block persisted in
// travel_rule_records.originator / .beneficiary.
type TravelRuleParty struct {
	Name          string `json:"name,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
	Address       string `json:"address,omitempty"` // originator only
	Country       string `json:"country,omitempty"`
}

// TravelRuleRecord is one travel_rule_records row.
type TravelRuleRecord struct {
	ID            int64            `json:"id"`
	TransferID    int64            `json:"transfer_id"`
	Direction     string           `json:"direction"`
	AccountID     int64            `json:"account_id"`
	Rail          string           `json:"rail,omitempty"`
	Currency      string           `json:"currency"`
	Amount        decimal.Decimal  `json:"amount"`
	USDAmount     *decimal.Decimal `json:"usd_amount,omitempty"`
	ThresholdUSD  decimal.Decimal  `json:"threshold_usd"`
	Originator    TravelRuleParty  `json:"originator"`
	Beneficiary   TravelRuleParty  `json:"beneficiary"`
	Status        string           `json:"status"`
	MissingFields []string         `json:"missing_fields"`
	SwiftFieldRef string           `json:"swift_field_ref,omitempty"`
	HoldRef       string           `json:"hold_ref,omitempty"`
	SuppliedBy    *int64           `json:"supplied_by,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// TravelRuleService owns record capture + validation. Bound to funding
// via DispatchService.WithTravelRule / DepositService.WithTravelRule.
type TravelRuleService struct {
	pool *pgxpool.Pool
	// resolveClient yields our client's legal identity (KYC name +
	// last-supplied originator address). nil → the built-in PG resolver
	// (verified bank_accounts beneficiary name + prior supplied rows).
	resolveClient func(ctx context.Context, tx pgx.Tx, accountID int64) (*TravelRuleParty, error)
	now           func() time.Time
}

// NewTravelRuleService wires the service; the pool is required.
func NewTravelRuleService(pool *pgxpool.Pool) (*TravelRuleService, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR", "travel rule service requires pool")
	}
	return &TravelRuleService{pool: pool, now: time.Now}, nil
}

// WithClientResolver overrides the legal-name/address resolver (tests).
func (s *TravelRuleService) WithClientResolver(
	f func(ctx context.Context, tx pgx.Tx, accountID int64) (*TravelRuleParty, error)) *TravelRuleService {
	s.resolveClient = f
	return s
}

// WithClock overrides the clock (tests).
func (s *TravelRuleService) WithClock(c func() time.Time) *TravelRuleService {
	s.now = c
	return s
}

// ---------------------------------------------------------------------------
// Threshold scope
// ---------------------------------------------------------------------------

// inScope reports whether the FATF rule applies to this transfer. An
// unpriced non-USD leg is pessimistically in scope — an unconvertible
// amount is never a reason to skip the gate (§2.7 pessimism).
func inScope(amount decimal.Decimal, currency string, usd *decimal.Decimal) bool {
	if usd != nil && usd.IsPositive() {
		return usd.GreaterThanOrEqual(TravelRuleThresholdUSD)
	}
	if strings.EqualFold(currency, "USD") {
		return amount.GreaterThanOrEqual(TravelRuleThresholdUSD)
	}
	return true
}

// missingFields lists the absent required members. Originator requires
// name + account_number + address; beneficiary requires name +
// account_number.
func missingFields(orig, bene TravelRuleParty) []string {
	var m []string
	if strings.TrimSpace(orig.Name) == "" {
		m = append(m, "originator.name")
	}
	if strings.TrimSpace(orig.AccountNumber) == "" {
		m = append(m, "originator.account_number")
	}
	if strings.TrimSpace(orig.Address) == "" {
		m = append(m, "originator.address")
	}
	if strings.TrimSpace(bene.Name) == "" {
		m = append(m, "beneficiary.name")
	}
	if strings.TrimSpace(bene.AccountNumber) == "" {
		m = append(m, "beneficiary.account_number")
	}
	return m
}

// mergeParty keeps stored non-empty fields over freshly resolved ones —
// officer-supplied data is never overwritten by re-resolution.
func mergeParty(stored, fresh TravelRuleParty) TravelRuleParty {
	out := fresh
	if strings.TrimSpace(stored.Name) != "" {
		out.Name = stored.Name
	}
	if strings.TrimSpace(stored.AccountNumber) != "" {
		out.AccountNumber = stored.AccountNumber
	}
	if strings.TrimSpace(stored.Address) != "" {
		out.Address = stored.Address
	}
	if strings.TrimSpace(stored.Country) != "" {
		out.Country = stored.Country
	}
	return out
}

// swiftFieldRef documents which wire fields carry the parties — the
// MT103 maps originator→50K and beneficiary→59; other rails note their
// ISO 20022 equivalents.
func swiftFieldRef(rail string) string {
	switch strings.ToUpper(rail) {
	case "SWIFT", "":
		return "MT103:50K/59"
	case "SEPA", "FEDNOW", "CHAPS", "TARGET2":
		return "pacs.008:Dbtr/Cdtr"
	case "ACH":
		return "ACH:addenda"
	default:
		return "rail:message"
	}
}

// ---------------------------------------------------------------------------
// Outbound gate — funding.DispatchService seam
// ---------------------------------------------------------------------------

// EnforceOutbound implements funding.TravelRuleGate. See the interface
// doc for the (payment, missing, error) contract.
func (s *TravelRuleService) EnforceOutbound(ctx context.Context, tx pgx.Tx,
	w *funding.DispatchableWithdrawal, p funding.OutboundPayment) (funding.OutboundPayment, []string, error) {
	if !inScope(w.Amount, w.Currency, w.USDAmount) {
		return p, nil, nil
	}
	stored, err := s.loadForUpdate(ctx, tx, w.ID, TravelRuleOutbound)
	if err != nil {
		return p, nil, err
	}
	if stored != nil && stored.Status == TravelRuleDeny {
		// A rejected record is terminal — the wire never dispatches.
		return p, []string{"record.status=REJECTED"}, nil
	}
	// Originator = our client (ordering customer); the account-of-record
	// reference is deterministic ("acct-<id>") when the resolver cannot
	// supply a client bank account number.
	freshP, err := s.resolve(ctx, tx, w.AccountID)
	if err != nil {
		return p, nil, err
	}
	fresh := TravelRuleParty{}
	if freshP != nil {
		fresh = *freshP
	}
	if fresh.AccountNumber == "" {
		fresh.AccountNumber = fmt.Sprintf("acct-%d", w.AccountID)
	}
	if stored != nil {
		fresh = mergeParty(stored.Originator, fresh)
	}
	bene := TravelRuleParty{
		Name:          strings.TrimSpace(p.CreditorName),
		AccountNumber: strings.TrimSpace(p.CreditorIBAN),
	}
	if stored != nil {
		bene = mergeParty(stored.Beneficiary, bene)
	}
	missing := missingFields(fresh, bene)
	status := TravelRuleOK
	if len(missing) > 0 {
		status = TravelRuleMissing
	}
	rail := string(p.Rail)
	if stored != nil && stored.Rail != "" && rail == "" {
		rail = stored.Rail
	}
	recID, err := s.upsert(ctx, tx, upsertArgs{
		Existing:   stored,
		TransferID: w.ID, Direction: TravelRuleOutbound,
		AccountID: w.AccountID, Rail: rail,
		Currency: w.Currency, Amount: w.Amount, USD: w.USDAmount,
		Originator: fresh, Beneficiary: bene,
		Status: status, Missing: missing,
		FieldRef: swiftFieldRef(rail),
	})
	if err != nil {
		return p, nil, err
	}
	action := "TR_RECORDED"
	if status == TravelRuleMissing {
		action = "TR_MISSING_INFO"
	}
	if _, err := audit.Append(ctx, tx, "travel_rule_records", &recID, action, nil); err != nil {
		return p, nil, excerrors.Wrap("INTERNAL_ERROR", "travel rule audit", err)
	}
	if len(missing) > 0 {
		return p, missing, nil
	}
	// Complete — enrich the instruction: the ordering customer (field
	// 50K) is our client, not the exchange nostro holder.
	p.OriginatorName = fresh.Name
	p.OriginatorAccount = fresh.AccountNumber
	p.OriginatorAddress = fresh.Address
	return p, nil, nil
}

// ---------------------------------------------------------------------------
// Inbound check — funding.DepositService seam
// ---------------------------------------------------------------------------

// CheckInbound implements funding.InboundTravelRuleChecker. The record
// row is written inside the deposit's resolution tx; the returned
// missing list turns into the TRAVEL_RULE_MISSING_INFO review flag.
func (s *TravelRuleService) CheckInbound(ctx context.Context, tx pgx.Tx,
	row *funding.FundingTxRow, confs []funding.DepositConfirmationRow) ([]string, error) {
	if !inScope(row.Amount, row.Currency, row.USDAmount) {
		return nil, nil
	}
	stored, err := s.loadForUpdate(ctx, tx, row.ID, TravelRuleInbound)
	if err != nil {
		return nil, err
	}
	// Originator = the wire sender, taken from the most recent
	// confirmation that carried it.
	var orig TravelRuleParty
	for _, c := range confs {
		if c.SenderName != nil && strings.TrimSpace(*c.SenderName) != "" {
			orig.Name = strings.TrimSpace(*c.SenderName)
		}
		if c.SenderAccount != nil && strings.TrimSpace(*c.SenderAccount) != "" {
			orig.AccountNumber = strings.TrimSpace(*c.SenderAccount)
		}
	}
	if stored != nil {
		orig = mergeParty(stored.Originator, orig)
	}
	// Beneficiary = our client (account holder).
	beneP, err := s.resolve(ctx, tx, row.AccountID)
	if err != nil {
		return nil, err
	}
	bene := TravelRuleParty{}
	if beneP != nil {
		bene = *beneP
	}
	bene.AccountNumber = fmt.Sprintf("acct-%d", row.AccountID)
	if stored != nil {
		bene = mergeParty(stored.Beneficiary, bene)
	}
	missing := missingFields(orig, bene)
	status := TravelRuleOK
	if len(missing) > 0 {
		status = TravelRuleMissing
	}
	recID, err := s.upsert(ctx, tx, upsertArgs{
		Existing:   stored,
		TransferID: row.ID, Direction: TravelRuleInbound,
		AccountID: row.AccountID,
		Rail:      strDeref(row.BankMethod),
		Currency:  row.Currency, Amount: row.Amount, USD: row.USDAmount,
		Originator: orig, Beneficiary: bene,
		Status: status, Missing: missing,
		FieldRef: swiftFieldRef(strDeref(row.BankMethod)),
	})
	if err != nil {
		return nil, err
	}
	action := "TR_RECORDED"
	if status == TravelRuleMissing {
		action = "TR_MISSING_INFO"
	}
	if _, err := audit.Append(ctx, tx, "travel_rule_records", &recID, action, nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "travel rule audit", err)
	}
	return missing, nil
}

// ---------------------------------------------------------------------------
// Officer surface — supply missing fields, list/get
// ---------------------------------------------------------------------------

// TravelRuleSupply is the officer-supplied party patch; empty fields
// leave the stored value untouched.
type TravelRuleSupply struct {
	Originator  *TravelRuleParty `json:"originator,omitempty"`
	Beneficiary *TravelRuleParty `json:"beneficiary,omitempty"`
}

// SupplyInfo records officer-supplied data against a MISSING_INFO
// record and re-validates; the transfer becomes dispatchable once the
// record reaches COMPLETE (the next dispatch sweep re-evaluates).
func (s *TravelRuleService) SupplyInfo(ctx context.Context, recordID, officer int64,
	in TravelRuleSupply) (*TravelRuleRecord, error) {
	if in.Originator == nil && in.Beneficiary == nil {
		return nil, excerrors.New("INVALID_REQUEST", "no fields supplied")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "supply tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rec, err := s.loadIDForUpdate(ctx, tx, recordID)
	if err != nil {
		return nil, err
	}
	if rec.Status == TravelRuleDeny {
		return nil, excerrors.New("TRAVEL_RULE_REJECTED",
			"record was rejected — amendment not permitted")
	}
	if in.Originator != nil {
		rec.Originator = mergeParty(*in.Originator, rec.Originator)
	}
	if in.Beneficiary != nil {
		rec.Beneficiary = mergeParty(*in.Beneficiary, rec.Beneficiary)
	}
	rec.MissingFields = missingFields(rec.Originator, rec.Beneficiary)
	if rec.MissingFields == nil {
		rec.MissingFields = []string{} // TEXT[] NOT NULL
	}
	if len(rec.MissingFields) == 0 {
		rec.Status = TravelRuleOK
	}
	tag, err := tx.Exec(ctx, `
		UPDATE travel_rule_records
		SET originator = $2, beneficiary = $3, status = $4,
		    missing_fields = $5, supplied_by = $6, updated_at = now()
		WHERE id = $1`,
		rec.ID, mustJSON(rec.Originator), mustJSON(rec.Beneficiary),
		rec.Status, rec.MissingFields, officer)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "travel rule supply", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, excerrors.New("INTERNAL_ERROR",
			"travel rule record vanished mid-update")
	}
	if _, err := audit.Append(ctx, tx, "travel_rule_records", &rec.ID,
		"TR_SUPPLIED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "travel rule audit", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "supply commit", err)
	}
	return rec, nil
}

// Get returns one record.
func (s *TravelRuleService) Get(ctx context.Context, id int64) (*TravelRuleRecord, error) {
	rec, err := s.scanOne(s.pool.QueryRow(ctx, `
		SELECT id, transfer_id, direction, account_id, rail, currency,
		       amount::text, usd_amount::text, threshold_usd::text,
		       originator, beneficiary, status, missing_fields,
		       swift_field_ref, hold_ref, supplied_by, created_at, updated_at
		FROM travel_rule_records WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "travel rule record not found")
	}
	return rec, err
}

// List returns newest-first records for the officer dashboard; status
// "" lists all.
func (s *TravelRuleService) List(ctx context.Context, status string,
	limit int) ([]TravelRuleRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `
		SELECT id, transfer_id, direction, account_id, rail, currency,
		       amount::text, usd_amount::text, threshold_usd::text,
		       originator, beneficiary, status, missing_fields,
		       swift_field_ref, hold_ref, supplied_by, created_at, updated_at
		FROM travel_rule_records`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ` + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "travel rule list", err)
	}
	defer rows.Close()
	var out []TravelRuleRecord
	for rows.Next() {
		rec, err := s.scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Persistence helpers
// ---------------------------------------------------------------------------

type upsertArgs struct {
	Existing    *TravelRuleRecord
	TransferID  int64
	Direction   string
	AccountID   int64
	Rail        string
	Currency    string
	Amount      decimal.Decimal
	USD         *decimal.Decimal
	Originator  TravelRuleParty
	Beneficiary TravelRuleParty
	Status      string
	Missing     []string
	FieldRef    string
}

// upsert inserts or refreshes the (transfer_id, direction) row. A
// REJECTED record is never resurrected — a rejected wire stays rejected.
func (s *TravelRuleService) upsert(ctx context.Context, tx pgx.Tx,
	a upsertArgs) (int64, error) {
	if a.Existing != nil && a.Existing.Status == TravelRuleDeny {
		return a.Existing.ID, nil
	}
	if a.Missing == nil {
		a.Missing = []string{}
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO travel_rule_records
		    (transfer_id, direction, account_id, rail, currency, amount,
		     usd_amount, originator, beneficiary, status, missing_fields,
		     swift_field_ref)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (transfer_id, direction) DO UPDATE SET
		    originator     = EXCLUDED.originator,
		    beneficiary    = EXCLUDED.beneficiary,
		    status         = EXCLUDED.status,
		    missing_fields = EXCLUDED.missing_fields,
		    rail           = EXCLUDED.rail,
		    swift_field_ref = EXCLUDED.swift_field_ref,
		    usd_amount     = COALESCE(travel_rule_records.usd_amount, EXCLUDED.usd_amount),
		    updated_at     = now()
		RETURNING id`,
		a.TransferID, a.Direction, a.AccountID, a.Rail, a.Currency,
		a.Amount, a.USD, mustJSON(a.Originator), mustJSON(a.Beneficiary),
		a.Status, a.Missing, a.FieldRef).Scan(&id)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "travel rule upsert", err)
	}
	return id, nil
}

func (s *TravelRuleService) loadForUpdate(ctx context.Context, tx pgx.Tx,
	transferID int64, direction string) (*TravelRuleRecord, error) {
	rec, err := s.scanOne(tx.QueryRow(ctx, `
		SELECT id, transfer_id, direction, account_id, rail, currency,
		       amount::text, usd_amount::text, threshold_usd::text,
		       originator, beneficiary, status, missing_fields,
		       swift_field_ref, hold_ref, supplied_by, created_at, updated_at
		FROM travel_rule_records
		WHERE transfer_id = $1 AND direction = $2 FOR UPDATE`,
		transferID, direction))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return rec, err
}

func (s *TravelRuleService) loadIDForUpdate(ctx context.Context, tx pgx.Tx,
	id int64) (*TravelRuleRecord, error) {
	rec, err := s.scanOne(tx.QueryRow(ctx, `
		SELECT id, transfer_id, direction, account_id, rail, currency,
		       amount::text, usd_amount::text, threshold_usd::text,
		       originator, beneficiary, status, missing_fields,
		       swift_field_ref, hold_ref, supplied_by, created_at, updated_at
		FROM travel_rule_records WHERE id = $1 FOR UPDATE`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "travel rule record not found")
	}
	return rec, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *TravelRuleService) scanOne(row rowScanner) (*TravelRuleRecord, error) {
	var rec TravelRuleRecord
	var rail, fieldRef, holdRef *string
	var amt, usd, thr *string
	var orig, bene []byte
	err := row.Scan(&rec.ID, &rec.TransferID, &rec.Direction, &rec.AccountID,
		&rail, &rec.Currency, &amt, &usd, &thr, &orig, &bene,
		&rec.Status, &rec.MissingFields, &fieldRef, &holdRef,
		&rec.SuppliedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if amt != nil {
		rec.Amount = decimal.RequireFromString(*amt)
	}
	if usd != nil {
		v := decimal.RequireFromString(*usd)
		rec.USDAmount = &v
	}
	if thr != nil {
		rec.ThresholdUSD = decimal.RequireFromString(*thr)
	}
	if rail != nil {
		rec.Rail = *rail
	}
	if fieldRef != nil {
		rec.SwiftFieldRef = *fieldRef
	}
	if holdRef != nil {
		rec.HoldRef = *holdRef
	}
	if err := json.Unmarshal(orig, &rec.Originator); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "originator decode", err)
	}
	if err := json.Unmarshal(bene, &rec.Beneficiary); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "beneficiary decode", err)
	}
	return &rec, nil
}

func (s *TravelRuleService) scanRow(rows pgx.Rows) (*TravelRuleRecord, error) {
	var rec TravelRuleRecord
	var rail, fieldRef, holdRef *string
	var amt, usd, thr *string
	var orig, bene []byte
	err := rows.Scan(&rec.ID, &rec.TransferID, &rec.Direction, &rec.AccountID,
		&rail, &rec.Currency, &amt, &usd, &thr, &orig, &bene,
		&rec.Status, &rec.MissingFields, &fieldRef, &holdRef,
		&rec.SuppliedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "travel rule scan", err)
	}
	if amt != nil {
		rec.Amount = decimal.RequireFromString(*amt)
	}
	if usd != nil {
		v := decimal.RequireFromString(*usd)
		rec.USDAmount = &v
	}
	if thr != nil {
		rec.ThresholdUSD = decimal.RequireFromString(*thr)
	}
	if rail != nil {
		rec.Rail = *rail
	}
	if fieldRef != nil {
		rec.SwiftFieldRef = *fieldRef
	}
	if holdRef != nil {
		rec.HoldRef = *holdRef
	}
	if err := json.Unmarshal(orig, &rec.Originator); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "originator decode", err)
	}
	if err := json.Unmarshal(bene, &rec.Beneficiary); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "beneficiary decode", err)
	}
	return &rec, nil
}

// resolve is the built-in originator/client resolver: the legal name
// comes from the account's verified beneficiary registry row (migration
// 040 — beneficiary_name "must match KYC legal name"), the address from
// the most recent prior travel-rule record that carried one (officer
// supply persists across transfers). Both may be absent — the record
// then reports the gap instead of fabricating it.
func (s *TravelRuleService) resolve(ctx context.Context, tx pgx.Tx,
	accountID int64) (*TravelRuleParty, error) {
	if s.resolveClient != nil {
		return s.resolveClient(ctx, tx, accountID)
	}
	var p TravelRuleParty
	if err := tx.QueryRow(ctx, `
		SELECT beneficiary_name
		FROM bank_accounts
		WHERE account_id = $1 AND status = 'VERIFIED'
		  AND beneficiary_name <> ''
		ORDER BY verified_at DESC NULLS LAST, bank_account_id
		LIMIT 1`, accountID).Scan(&p.Name); err != nil && err != pgx.ErrNoRows {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "originator name resolve", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT originator->>'address'
		FROM travel_rule_records
		WHERE account_id = $1 AND originator->>'address' IS NOT NULL
		  AND originator->>'address' <> ''
		ORDER BY updated_at DESC LIMIT 1`, accountID).Scan(&p.Address); err != nil && err != pgx.ErrNoRows {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "originator address resolve", err)
	}
	return &p, nil
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
