// client_money_test.go — DoD coverage for Phase-24 Tasks 24.3.11 and
// 24.3.16: classification, receipts, daily reconciliation (internal +
// external legs, snapshots, sign-off), the 4-tier shortfall waterfall,
// movement blocking, the daily stress test, pooling/wind-down exports,
// CLS quarantine and the 60-minute escalation.
package backoffice

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"exchange/internal/admin"
	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// In-memory Store — implements the full Tx+Store contract for service tests.
// Not goroutine-safe by design: tests are serial; InTx passes the same map
// set as the tx-scoped view (atomicity is exercised by error-path asserts).
// ---------------------------------------------------------------------------

type cmStore struct {
	nextID int64

	moneyAccts  map[int64]*MoneyAccount
	reviews     map[int64]*BankReview
	receipts    map[int64]*Receipt
	recons      map[int64]*Reconciliation
	breaks      map[int64]*Break
	rems        map[int64]*Remediation
	notices     map[int64]*RegulatorNotice
	stress      map[int64]*StressRun
	exports     map[int64]*PoolingExport
	quarantines map[int64]*Quarantine
	ownFunds    map[int64]*OwnFunds
	liqAssess   map[int64]*LiquidityAssessment
	controls    TreasuryControls
	commitments map[int64]*Commitment
	audits      map[int64]*ClientMoneyAudit
	packs       map[int64]*EvidencePack
	certs       map[int64]*SegregationCertification
	grants      map[int64]*AuditorGrant

	// live-source fixtures
	entitlements map[string][]Entitlement              // ccy → legs
	classified   map[string]map[string]decimal.Decimal // ccy → class → bal
	nostroBal    map[int64]decimal.Decimal
	roots        []PoRRoot
	glLines      []GLLine
}

func newCMStore() *cmStore {
	return &cmStore{
		nextID:       1,
		moneyAccts:   map[int64]*MoneyAccount{},
		reviews:      map[int64]*BankReview{},
		receipts:     map[int64]*Receipt{},
		recons:       map[int64]*Reconciliation{},
		breaks:       map[int64]*Break{},
		rems:         map[int64]*Remediation{},
		notices:      map[int64]*RegulatorNotice{},
		stress:       map[int64]*StressRun{},
		exports:      map[int64]*PoolingExport{},
		quarantines:  map[int64]*Quarantine{},
		ownFunds:     map[int64]*OwnFunds{},
		liqAssess:    map[int64]*LiquidityAssessment{},
		commitments:  map[int64]*Commitment{},
		audits:       map[int64]*ClientMoneyAudit{},
		packs:        map[int64]*EvidencePack{},
		certs:        map[int64]*SegregationCertification{},
		grants:       map[int64]*AuditorGrant{},
		entitlements: map[string][]Entitlement{},
		classified:   map[string]map[string]decimal.Decimal{},
		nostroBal:    map[int64]decimal.Decimal{},
	}
}

func (m *cmStore) alloc() int64 { id := m.nextID; m.nextID++; return id }

func (m *cmStore) InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	return fn(ctx, m) // serial mem execution — atomicity asserted via state checks
}

// -- classification -----------------------------------------------------------

func (m *cmStore) InsertMoneyAccount(_ context.Context, a MoneyAccount) (*MoneyAccount, error) {
	cp := a
	cp.ID = m.alloc()
	m.moneyAccts[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) MoneyAccountByID(_ context.Context, id int64) (*MoneyAccount, error) {
	if a, ok := m.moneyAccts[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) MoneyAccountByGLCode(_ context.Context, code string) (*MoneyAccount, error) {
	for _, a := range m.moneyAccts {
		if a.GLAccountCode == code {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}
func (m *cmStore) MoneyAccountByNostro(_ context.Context, nostroID int64) (*MoneyAccount, error) {
	for _, a := range m.moneyAccts {
		if a.NostroAccountID != nil && *a.NostroAccountID == nostroID {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}
func (m *cmStore) ListMoneyAccounts(_ context.Context, class string) ([]MoneyAccount, error) {
	out := []MoneyAccount{}
	for _, a := range m.moneyAccts {
		if class == "" || a.Classification == class {
			out = append(out, *a)
		}
	}
	return out, nil
}
func (m *cmStore) SetMoneyAccountTrust(_ context.Context, id int64, trust string, ackAt, nextDD *time.Time) error {
	a, ok := m.moneyAccts[id]
	if !ok {
		return fmt.Errorf("money account %d not found", id)
	}
	a.TrustStatus = trust
	a.AcknowledgementAt = ackAt
	if nextDD != nil {
		a.NextDueDiligenceAt = nextDD
	}
	return nil
}
func (m *cmStore) InsertBankReview(_ context.Context, r BankReview) (*BankReview, error) {
	cp := r
	cp.ID = m.alloc()
	m.reviews[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) ListBankReviews(_ context.Context, accountID int64) ([]BankReview, error) {
	out := []BankReview{}
	for _, r := range m.reviews {
		if r.ClientMoneyAccountID == accountID {
			out = append(out, *r)
		}
	}
	return out, nil
}

// -- receipts -------------------------------------------------------------------

func (m *cmStore) InsertReceipt(_ context.Context, r Receipt) (*Receipt, error) {
	for _, e := range m.receipts {
		sameNostro := (e.NostroAccountID == nil && r.NostroAccountID == nil) ||
			(e.NostroAccountID != nil && r.NostroAccountID != nil && *e.NostroAccountID == *r.NostroAccountID)
		if sameNostro && e.BankReference == r.BankReference {
			cp := *e
			return &cp, nil // dedup per (nostro, bank_reference)
		}
	}
	cp := r
	cp.ID = m.alloc()
	m.receipts[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) ReceiptByID(_ context.Context, id int64) (*Receipt, error) {
	if r, ok := m.receipts[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) UpdateReceipt(_ context.Context, r Receipt) error {
	if _, ok := m.receipts[r.ID]; !ok {
		return fmt.Errorf("receipt %d not found", r.ID)
	}
	cp := r
	m.receipts[r.ID] = &cp
	return nil
}
func (m *cmStore) ListReceipts(_ context.Context, status string, limit int) ([]Receipt, error) {
	out := []Receipt{}
	for _, r := range m.receipts {
		if status == "" || r.Status == status {
			out = append(out, *r)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (m *cmStore) UnidentifiedReceipts(_ context.Context, ccy string) (decimal.Decimal, int64, error) {
	total := decimal.Zero
	var n int64
	for _, r := range m.receipts {
		if r.Status == ReceiptUnidentified && r.Currency == ccy {
			total = total.Add(r.Amount)
			n++
		}
	}
	return total, n, nil
}

// -- reconciliations ------------------------------------------------------------

func (m *cmStore) InsertReconciliation(_ context.Context, r Reconciliation) (*Reconciliation, error) {
	cp := r
	cp.ID = m.alloc()
	m.recons[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) ReconciliationByID(_ context.Context, id int64) (*Reconciliation, error) {
	if r, ok := m.recons[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) ReconciliationForDay(_ context.Context, day time.Time, ccy string) (*Reconciliation, error) {
	d := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	for _, r := range m.recons {
		if r.ReconDate.Equal(d) && r.Currency == ccy {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}
func (m *cmStore) UpdateReconciliation(_ context.Context, r Reconciliation) error {
	if _, ok := m.recons[r.ID]; !ok {
		return fmt.Errorf("reconciliation %d not found", r.ID)
	}
	cp := r
	m.recons[r.ID] = &cp
	return nil
}
func (m *cmStore) ListReconciliations(_ context.Context, from, to time.Time) ([]Reconciliation, error) {
	out := []Reconciliation{}
	for _, r := range m.recons {
		if (from.IsZero() || !r.ReconDate.Before(from)) && (to.IsZero() || !r.ReconDate.After(to)) {
			out = append(out, *r)
		}
	}
	return out, nil
}

// -- live-state sources -----------------------------------------------------------

func (m *cmStore) ClientEntitlements(_ context.Context, ccy string) ([]Entitlement, error) {
	out := []Entitlement{}
	out = append(out, m.entitlements[ccy]...)
	return out, nil
}
func (m *cmStore) ClassifiedBalance(_ context.Context, ccy, class string) (decimal.Decimal, error) {
	if m.classified[ccy] == nil {
		return decimal.Zero, nil
	}
	return m.classified[ccy][class], nil
}
func (m *cmStore) NostroBalance(_ context.Context, nostroAccountID int64) (decimal.Decimal, error) {
	return m.nostroBal[nostroAccountID], nil
}
func (m *cmStore) MerkleRoots(_ context.Context, from, to time.Time) ([]PoRRoot, error) {
	out := []PoRRoot{}
	for _, r := range m.roots {
		if (from.IsZero() || !r.Date.Before(from)) && (to.IsZero() || !r.Date.After(to)) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *cmStore) GLLines(_ context.Context, codes []string, from, to time.Time) ([]GLLine, error) {
	want := map[string]bool{}
	for _, c := range codes {
		want[c] = true
	}
	out := []GLLine{}
	for _, l := range m.glLines {
		if !want[l.AccountCode] {
			continue
		}
		if (from.IsZero() || !l.PostedAt.Before(from)) && (to.IsZero() || l.PostedAt.Before(to)) {
			out = append(out, l)
		}
	}
	return out, nil
}

// -- breaks -----------------------------------------------------------------------

func (m *cmStore) InsertBreak(_ context.Context, b Break) (*Break, error) {
	cp := b
	cp.ID = m.alloc()
	m.breaks[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) BreakByID(_ context.Context, id int64) (*Break, error) {
	if b, ok := m.breaks[id]; ok {
		cp := *b
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) UpdateBreak(_ context.Context, b Break) error {
	if _, ok := m.breaks[b.ID]; !ok {
		return fmt.Errorf("break %d not found", b.ID)
	}
	cp := b
	m.breaks[b.ID] = &cp
	return nil
}
func (m *cmStore) OpenBreaks(_ context.Context, kind, ccy string) ([]Break, error) {
	out := []Break{}
	for _, b := range m.breaks {
		if kind != "" && b.Kind != kind {
			continue
		}
		if ccy != "" && b.Currency != ccy {
			continue
		}
		if b.Status == BreakResolved {
			continue
		}
		out = append(out, *b)
	}
	return out, nil
}
func (m *cmStore) ListBreaks(_ context.Context, from, to time.Time) ([]Break, error) {
	out := []Break{}
	for _, b := range m.breaks {
		if (from.IsZero() || !b.DetectedAt.Before(from)) && (to.IsZero() || !b.DetectedAt.After(to)) {
			out = append(out, *b)
		}
	}
	return out, nil
}

// -- remediations & notices --------------------------------------------------------

func (m *cmStore) InsertRemediation(_ context.Context, r Remediation) (*Remediation, error) {
	cp := r
	cp.ID = m.alloc()
	m.rems[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) RemediationByID(_ context.Context, id int64) (*Remediation, error) {
	if r, ok := m.rems[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) UpdateRemediation(_ context.Context, r Remediation) error {
	if _, ok := m.rems[r.ID]; !ok {
		return fmt.Errorf("remediation %d not found", r.ID)
	}
	cp := r
	m.rems[r.ID] = &cp
	return nil
}
func (m *cmStore) RemediationsForBreak(_ context.Context, breakID int64) ([]Remediation, error) {
	out := []Remediation{}
	for _, r := range m.rems {
		if r.BreakID == breakID {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (m *cmStore) PendingRemediations(_ context.Context, _ time.Time) ([]Remediation, error) {
	out := []Remediation{}
	for _, r := range m.rems {
		if r.Status == RemPending {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (m *cmStore) ListRemediations(_ context.Context, from, to time.Time) ([]Remediation, error) {
	out := []Remediation{}
	for _, r := range m.rems {
		if (from.IsZero() || !r.CreatedAt.Before(from)) && (to.IsZero() || !r.CreatedAt.After(to)) {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (m *cmStore) InsertRegulatorNotice(_ context.Context, n RegulatorNotice) (*RegulatorNotice, error) {
	cp := n
	cp.ID = m.alloc()
	m.notices[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) UpdateRegulatorNotice(_ context.Context, n RegulatorNotice) error {
	if _, ok := m.notices[n.ID]; !ok {
		return fmt.Errorf("notice %d not found", n.ID)
	}
	cp := n
	m.notices[n.ID] = &cp
	return nil
}
func (m *cmStore) NoticeExists(_ context.Context, breakID int64, trigger string) (bool, error) {
	for _, n := range m.notices {
		if n.BreakID != nil && *n.BreakID == breakID && n.Trigger == trigger {
			return true, nil
		}
	}
	return false, nil
}
func (m *cmStore) PendingNotices(_ context.Context, now time.Time) ([]RegulatorNotice, error) {
	out := []RegulatorNotice{}
	for _, n := range m.notices {
		if n.Status == NoticePending && !n.DeadlineAt.After(now) {
			out = append(out, *n)
		}
	}
	return out, nil
}

// -- stress runs & exports -----------------------------------------------------------

func (m *cmStore) InsertStressRun(_ context.Context, r StressRun) (*StressRun, error) {
	cp := r
	cp.ID = m.alloc()
	m.stress[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) ListStressRuns(_ context.Context, from, to time.Time) ([]StressRun, error) {
	out := []StressRun{}
	for _, r := range m.stress {
		if (from.IsZero() || !r.RunDate.Before(from)) && (to.IsZero() || !r.RunDate.After(to)) {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (m *cmStore) InsertPoolingExport(_ context.Context, e PoolingExport) (*PoolingExport, error) {
	cp := e
	cp.ID = m.alloc()
	m.exports[cp.ID] = &cp
	return &cp, nil
}

// -- quarantines ------------------------------------------------------------------

func (m *cmStore) InsertQuarantine(_ context.Context, q Quarantine) (*Quarantine, error) {
	cp := q
	cp.ID = m.alloc()
	m.quarantines[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) QuarantineByID(_ context.Context, id int64) (*Quarantine, error) {
	if q, ok := m.quarantines[id]; ok {
		cp := *q
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) QuarantineByBatch(_ context.Context, batchRef string) (*Quarantine, error) {
	var latest *Quarantine
	for _, q := range m.quarantines {
		if q.BatchRef == batchRef && (latest == nil || q.ID > latest.ID) {
			cp := *q
			latest = &cp
		}
	}
	return latest, nil
}
func (m *cmStore) UpdateQuarantine(_ context.Context, q Quarantine) error {
	if _, ok := m.quarantines[q.ID]; !ok {
		return fmt.Errorf("quarantine %d not found", q.ID)
	}
	cp := q
	m.quarantines[q.ID] = &cp
	return nil
}
func (m *cmStore) ActiveQuarantines(_ context.Context) ([]Quarantine, error) {
	out := []Quarantine{}
	for _, q := range m.quarantines {
		if q.Status == QuarantineActive {
			out = append(out, *q)
		}
	}
	return out, nil
}

// -- treasury ----------------------------------------------------------------------

func (m *cmStore) UpsertOwnFunds(_ context.Context, o OwnFunds) (*OwnFunds, error) {
	for _, e := range m.ownFunds {
		if e.LineKind == o.LineKind && e.Currency == o.Currency {
			cp := o
			cp.ID = e.ID
			m.ownFunds[e.ID] = &cp
			return &cp, nil
		}
	}
	cp := o
	cp.ID = m.alloc()
	m.ownFunds[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) OwnFundsByID(_ context.Context, id int64) (*OwnFunds, error) {
	if o, ok := m.ownFunds[id]; ok {
		cp := *o
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) ListOwnFunds(_ context.Context) ([]OwnFunds, error) {
	out := []OwnFunds{}
	for _, o := range m.ownFunds {
		out = append(out, *o)
	}
	return out, nil
}
func (m *cmStore) OwnFundsSum(_ context.Context, ccy string, kinds []string) (decimal.Decimal, error) {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	total := decimal.Zero
	for _, o := range m.ownFunds {
		if !want[o.LineKind] {
			continue
		}
		if ccy != "" && o.Currency != ccy {
			continue
		}
		total = total.Add(o.Balance)
	}
	return total, nil
}
func (m *cmStore) InsertLiquidityAssessment(_ context.Context, a LiquidityAssessment) (*LiquidityAssessment, error) {
	cp := a
	cp.ID = m.alloc()
	m.liqAssess[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) TreasuryControls(_ context.Context) (*TreasuryControls, error) {
	cp := m.controls
	return &cp, nil
}
func (m *cmStore) UpdateTreasuryControls(_ context.Context, c TreasuryControls) error {
	m.controls = c
	return nil
}
func (m *cmStore) InsertCommitment(_ context.Context, c Commitment) (*Commitment, error) {
	cp := c
	cp.ID = m.alloc()
	m.commitments[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) CommitmentByID(_ context.Context, id int64) (*Commitment, error) {
	if c, ok := m.commitments[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) UpdateCommitment(_ context.Context, c Commitment) error {
	if _, ok := m.commitments[c.ID]; !ok {
		return fmt.Errorf("commitment %d not found", c.ID)
	}
	cp := c
	m.commitments[c.ID] = &cp
	return nil
}
func (m *cmStore) ListCommitments(_ context.Context) ([]Commitment, error) {
	out := []Commitment{}
	for _, c := range m.commitments {
		out = append(out, *c)
	}
	return out, nil
}
func (m *cmStore) ExpiringPolicies(_ context.Context, horizon time.Time) ([]Commitment, error) {
	out := []Commitment{}
	for _, c := range m.commitments {
		if c.Kind == CommitInsurancePolicy && c.ExpiresAt != nil && !c.ExpiresAt.After(horizon) &&
			(c.Status == CommitCommitted || c.Status == CommitExecuted || c.Status == CommitDrawn) {
			out = append(out, *c)
		}
	}
	return out, nil
}

// -- assurance ----------------------------------------------------------------------

func (m *cmStore) InsertAudit(_ context.Context, a ClientMoneyAudit) (*ClientMoneyAudit, error) {
	cp := a
	cp.ID = m.alloc()
	m.audits[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) AuditByID(_ context.Context, id int64) (*ClientMoneyAudit, error) {
	if a, ok := m.audits[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) UpdateAudit(_ context.Context, a ClientMoneyAudit) error {
	if _, ok := m.audits[a.ID]; !ok {
		return fmt.Errorf("audit %d not found", a.ID)
	}
	cp := a
	m.audits[a.ID] = &cp
	return nil
}
func (m *cmStore) ListAudits(_ context.Context, status string, limit int) ([]ClientMoneyAudit, error) {
	out := []ClientMoneyAudit{}
	for _, a := range m.audits {
		if status == "" || a.Status == status {
			out = append(out, *a)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (m *cmStore) InsertEvidencePack(_ context.Context, p EvidencePack) (*EvidencePack, error) {
	cp := p
	cp.ID = m.alloc()
	m.packs[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) EvidencePackByID(_ context.Context, id int64) (*EvidencePack, error) {
	if p, ok := m.packs[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) EvidencePacksForAudit(_ context.Context, auditID int64) ([]EvidencePack, error) {
	out := []EvidencePack{}
	for _, p := range m.packs {
		if p.AuditID == auditID {
			out = append(out, *p)
		}
	}
	return out, nil
}
func (m *cmStore) InsertCertification(_ context.Context, c SegregationCertification) (*SegregationCertification, error) {
	cp := c
	cp.ID = m.alloc()
	m.certs[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) UpdateCertification(_ context.Context, c SegregationCertification) error {
	if _, ok := m.certs[c.ID]; !ok {
		return fmt.Errorf("certification %d not found", c.ID)
	}
	cp := c
	m.certs[c.ID] = &cp
	return nil
}
func (m *cmStore) ListCertifications(_ context.Context) ([]SegregationCertification, error) {
	out := []SegregationCertification{}
	for _, c := range m.certs {
		out = append(out, *c)
	}
	return out, nil
}
func (m *cmStore) CertificationsCovering(_ context.Context, day time.Time) ([]SegregationCertification, error) {
	out := []SegregationCertification{}
	for _, c := range m.certs {
		if c.Status == CertIssued && !c.PeriodStart.After(day) && !c.PeriodEnd.Before(day) {
			out = append(out, *c)
		}
	}
	return out, nil
}
func (m *cmStore) InsertAuditorGrant(_ context.Context, g AuditorGrant) (*AuditorGrant, error) {
	cp := g
	cp.ID = m.alloc()
	m.grants[cp.ID] = &cp
	return &cp, nil
}
func (m *cmStore) AuditorGrantByID(_ context.Context, id int64) (*AuditorGrant, error) {
	if g, ok := m.grants[id]; ok {
		cp := *g
		return &cp, nil
	}
	return nil, nil
}
func (m *cmStore) UpdateAuditorGrant(_ context.Context, g AuditorGrant) error {
	if _, ok := m.grants[g.ID]; !ok {
		return fmt.Errorf("grant %d not found", g.ID)
	}
	cp := g
	m.grants[g.ID] = &cp
	return nil
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type cmAlertCap struct{ alerts []OpsAlert }

func (f *cmAlertCap) Raise(_ context.Context, a OpsAlert) error {
	f.alerts = append(f.alerts, a)
	return nil
}

func (f *cmAlertCap) has(code string) bool {
	for _, a := range f.alerts {
		if a.Code == code {
			return true
		}
	}
	return false
}

type cmPoster struct {
	next     int64
	journals []ledger.Journal
	err      error
}

func (p *cmPoster) PostJournal(_ context.Context, _ Tx, j ledger.Journal) (int64, error) {
	if p.err != nil {
		return 0, p.err
	}
	p.next++
	p.journals = append(p.journals, j)
	return p.next, nil
}

type cmFund struct {
	bal    map[string]decimal.Decimal
	debits []decimal.Decimal
}

func (f *cmFund) Balance(_ context.Context, _ Tx, ccy string) (decimal.Decimal, error) {
	return f.bal[ccy], nil
}
func (f *cmFund) Debit(_ context.Context, _ Tx, ccy string, amount decimal.Decimal, _ int64) error {
	f.bal[ccy] = f.bal[ccy].Sub(amount)
	f.debits = append(f.debits, amount)
	return nil
}

type cmHouse struct{ bal map[string]decimal.Decimal }

func (f *cmHouse) HouseReserve(_ context.Context, _ Tx, ccy string) (decimal.Decimal, error) {
	return f.bal[ccy], nil
}

type cmNBP struct{ worst map[string]decimal.Decimal }

func (f *cmNBP) WorstNBPExposure(_ context.Context, ccy string) (decimal.Decimal, error) {
	return f.worst[ccy], nil
}

type cmStmtVal struct {
	bal decimal.Decimal
	ref string
}

type cmStmts struct {
	byAcct map[int64]map[string]cmStmtVal // nostro id → YYYY-MM-DD → closing
}

func (f *cmStmts) ClosingBalance(_ context.Context, id int64, day time.Time) (*decimal.Decimal, string, error) {
	v, ok := f.byAcct[id][day.Format("2006-01-02")]
	if !ok {
		return nil, "", nil
	}
	b := v.bal
	return &b, v.ref, nil
}

type cmSusp struct{ calls []string }

func (f *cmSusp) SuspendTrading(_ context.Context, reason string) error {
	f.calls = append(f.calls, reason)
	return nil
}

type cmNoticeCap struct {
	sent []RegulatorNotice
	fail bool
}

func (f *cmNoticeCap) DispatchRegulatorNotice(_ context.Context, n RegulatorNotice) error {
	if f.fail {
		return fmt.Errorf("dispatcher down")
	}
	f.sent = append(f.sent, n)
	return nil
}

// cmClock is a mutable clock for deadline sweeps.
type cmClock struct{ t time.Time }

func (f *cmClock) now() time.Time { return f.t }

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	uidFO      = 11 // Finance Ops
	uidFO2     = 12 // Finance Ops #2 (approver)
	uidCO      = 13 // Compliance Officer
	uidAuditor = 14 // Read-Only Auditor
	uidSupport = 15 // Support Agent (must be refused)
)

func cmRoles(_ context.Context, id int64) (string, error) {
	switch id {
	case uidFO, uidFO2:
		return admin.RoleFinanceOps, nil
	case uidCO:
		return admin.RoleComplianceOfficer, nil
	case uidAuditor:
		return admin.RoleReadOnlyAuditor, nil
	case uidSupport:
		return admin.RoleSupportAgent, nil
	}
	return "", fmt.Errorf("unknown admin %d", id)
}

func cmActor(id int64) admin.AdminActor { return admin.AdminActor{UserID: id} }

func cmAct2(id, approver int64) admin.AdminActor {
	return admin.AdminActor{UserID: id, ApproverID: approver}
}

func cmDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	return decimal.RequireFromString(s)
}

func newCMService(t *testing.T, store *cmStore, clk *cmClock) (*ClientMoneyService, *cmAlertCap, *cmPoster, *cmFund, *cmHouse, *cmNBP, *cmSusp, *cmNoticeCap) {
	t.Helper()
	al := &cmAlertCap{}
	po := &cmPoster{}
	fu := &cmFund{bal: map[string]decimal.Decimal{}}
	ho := &cmHouse{bal: map[string]decimal.Decimal{}}
	nb := &cmNBP{worst: map[string]decimal.Decimal{}}
	su := &cmSusp{}
	no := &cmNoticeCap{}
	svc, err := NewClientMoneyService(ClientMoneyDeps{
		Store: store, Poster: po, Fund: fu, House: ho, NBP: nb,
		Suspension: su, Notices: no, Alerter: al,
		Resolver: cmRoles, Now: clk.now,
	})
	if err != nil {
		t.Fatalf("NewClientMoneyService: %v", err)
	}
	return svc, al, po, fu, ho, nb, su, no
}

func cmPtr64(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// Task 24.3.11 — classification
// ---------------------------------------------------------------------------

func TestClassifyAccount_StructuralGLCrossCheck(t *testing.T) {
	st := newCMStore()
	svc, _, _, _, _, _, _, _ := newCMService(t, st, &cmClock{t: time.Now().UTC()})
	ctx := context.Background()

	// CLIENT classification on a client-segregation code — allowed.
	a, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindGL, GLAccountCode: ledger.ClientMoneySegregated("USD"),
		Classification: ClassClient, Currency: "USD",
	})
	if err != nil || a.Classification != ClassClient {
		t.Fatalf("client classification on client code failed: %v", err)
	}

	// CLIENT on a house code — fails closed.
	if _, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindGL, GLAccountCode: ledger.HouseEquity("USD"),
		Classification: ClassClient, Currency: "USD",
	}); err == nil {
		t.Fatal("CLIENT classification on house GL code must be refused")
	}

	// HOUSE on a client code — fails closed.
	if _, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindGL, GLAccountCode: ledger.ClientMoneySegregated("USD"),
		Classification: ClassHouse, Currency: "USD",
	}); err == nil {
		t.Fatal("HOUSE classification on client GL code must be refused")
	}

	// HOUSE on a house code — allowed.
	if _, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindGL, GLAccountCode: ledger.Nostro("USD"),
		Classification: ClassHouse, Currency: "USD",
	}); err != nil {
		t.Fatalf("house classification on house code: %v", err)
	}

	// Unknown GL codes fail closed (SegregationOf unknown → house side).
	if _, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindGL, GLAccountCode: "9999_UNKNOWN_USD",
		Classification: ClassClient, Currency: "USD",
	}); err == nil {
		t.Fatal("CLIENT classification on unknown GL code must be refused")
	}

	// Role enforcement — Support Agent cannot classify.
	if _, err := svc.ClassifyAccount(ctx, cmActor(uidSupport), ClassifyInput{
		AccountKind: KindGL, GLAccountCode: ledger.Nostro("EUR"),
		Classification: ClassHouse, Currency: "EUR",
	}); err == nil {
		t.Fatal("Support Agent must not classify accounts")
	}
}

func TestRecordBankReview_AcknowledgementPass(t *testing.T) {
	st := newCMStore()
	svc, _, _, _, _, _, _, _ := newCMService(t, st, &cmClock{t: time.Now().UTC()})
	ctx := context.Background()

	a, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindBank, BankIBAN: "GB29NWBK60161331926819", BankName: "Seg Bank",
		Classification: ClassClient, Currency: "GBP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.TrustStatus != TrustNone {
		t.Fatalf("new account trust=%s want NONE", a.TrustStatus)
	}
	if _, err := svc.RecordBankReview(ctx, cmActor(uidCO), BankReview{
		ClientMoneyAccountID: a.ID, ReviewKind: "ACKNOWLEDGEMENT", Outcome: "PASS",
		DocumentRef: "trust-letter-2026.pdf",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.MoneyAccountByID(ctx, a.ID)
	if got.TrustStatus != TrustAcknowledged || got.AcknowledgementAt == nil {
		t.Fatalf("trust=%s ack=%v — want ACKNOWLEDGED with timestamp", got.TrustStatus, got.AcknowledgementAt)
	}
}

// ---------------------------------------------------------------------------
// Task 24.3.11 — receipts
// ---------------------------------------------------------------------------

func TestReceipts_UnidentifiedThenAllocated(t *testing.T) {
	st := newCMStore()
	svc, _, _, _, _, _, _, _ := newCMService(t, st, &cmClock{t: time.Now().UTC()})
	ctx := context.Background()

	r, err := svc.RecordReceipt(ctx, cmActor(uidFO), Receipt{
		NostroAccountID: cmPtr64(7), BankReference: "BANK-REF-1",
		Amount: cmDec(t, "5000"), Currency: "USD",
	})
	if err != nil || r.Status != ReceiptUnidentified {
		t.Fatalf("receipt status=%s err=%v", r.Status, err)
	}
	// Duplicate feed row dedups, not double-counts.
	dup, err := svc.RecordReceipt(ctx, cmActor(uidFO), Receipt{
		NostroAccountID: cmPtr64(7), BankReference: "BANK-REF-1",
		Amount: cmDec(t, "5000"), Currency: "USD",
	})
	if err != nil || dup.ID != r.ID {
		t.Fatalf("dedup failed: id=%d err=%v", dup.ID, err)
	}
	total, n, _ := st.UnidentifiedReceipts(ctx, "USD")
	if !total.Equal(cmDec(t, "5000")) || n != 1 {
		t.Fatalf("unidentified=%s n=%d want 5000/1", total, n)
	}

	got, err := svc.AllocateReceipt(ctx, cmActor(uidFO), r.ID, 4242)
	if err != nil || got.Status != ReceiptAllocated || got.AllocatedAccountID == nil {
		t.Fatalf("allocate: %v %+v", err, got)
	}
	// Re-allocation is an invalid lifecycle transition.
	if _, err := svc.AllocateReceipt(ctx, cmActor(uidFO), r.ID, 4242); err == nil {
		t.Fatal("re-allocating an ALLOCATED receipt must fail")
	}
	total, n, _ = st.UnidentifiedReceipts(ctx, "USD")
	if !total.IsZero() || n != 0 {
		t.Fatalf("unidentified after alloc=%s n=%d", total, n)
	}
}

// ---------------------------------------------------------------------------
// Task 24.3.11 — daily reconciliation
// ---------------------------------------------------------------------------

func TestDailyReconciliation_BalancedAndSignedOff(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, _, _, _, _, _, _, _ := newCMService(t, st, clk)
	ctx := context.Background()
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "100000")}}
	st.classified["USD"] = map[string]decimal.Decimal{
		ClassClient: cmDec(t, "95000"), ClassMargin: cmDec(t, "5000"),
	}

	rec, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), day, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != ReconBalanced || !rec.Variance.IsZero() {
		t.Fatalf("status=%s variance=%s", rec.Status, rec.Variance)
	}
	if rec.ExternalStatus != ExtUnavailable {
		t.Fatalf("no statement seam → external must be UNAVAILABLE, got %s", rec.ExternalStatus)
	}
	if len(rec.InternalSnapshot) == 0 {
		t.Fatal("internal snapshot not persisted")
	}

	// Idempotent rerun returns the same row.
	again, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), day, "USD")
	if err != nil || again.ID != rec.ID {
		t.Fatalf("rerun not idempotent: %v", err)
	}

	// Sign-off requires a distinct principal.
	if _, err := svc.SignOffReconciliation(ctx, cmActor(uidFO), rec.ID); err == nil {
		t.Fatal("performer signing off own reconciliation must fail")
	}
	signed, err := svc.SignOffReconciliation(ctx, cmActor(uidCO), rec.ID)
	if err != nil || signed.SignedOffBy == nil || *signed.SignedOffBy != uidCO {
		t.Fatalf("sign-off: %v %+v", err, signed)
	}
}

func TestDailyReconciliation_ExternalLeg(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, _, _, _, _, _, _, _ := newCMService(t, st, clk)
	// Wire a statement source with a mismatching closing balance.
	stmts := &cmStmts{byAcct: map[int64]map[string]cmStmtVal{
		7: {"2026-09-20": {bal: cmDec(t, "999"), ref: "STMT-9"}},
	}}
	svc.stmts = stmts
	ctx := context.Background()
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	if _, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindNostro, NostroAccountID: cmPtr64(7),
		Classification: ClassClient, Currency: "USD",
	}); err != nil {
		t.Fatal(err)
	}
	st.nostroBal[7] = cmDec(t, "1000") // internal 1000 vs statement 999 → MISMATCH
	st.classified["USD"] = map[string]decimal.Decimal{ClassClient: cmDec(t, "1000")}
	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "1000")}}

	rec, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), day, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExternalStatus != ExtMismatch {
		t.Fatalf("external=%s want MISMATCH", rec.ExternalStatus)
	}
	if len(rec.ExternalSnapshot) == 0 {
		t.Fatal("external snapshot missing")
	}
	// The statement divergence opens an UNMATCHED break.
	found := false
	for _, b := range st.breaks {
		if b.Kind == BreakUnmatched {
			found = true
		}
	}
	if !found {
		t.Fatal("external mismatch must open an UNMATCHED break")
	}
}

// ---------------------------------------------------------------------------
// Task 24.3.11 — shortfall waterfall
// ---------------------------------------------------------------------------

func TestShortfall_Tier1CoversFully(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, al, po, fu, _, _, _, _ := newCMService(t, st, clk)
	ctx := context.Background()

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "1000")}}
	st.classified["USD"] = map[string]decimal.Decimal{ClassClient: cmDec(t, "600")}
	fu.bal["USD"] = cmDec(t, "5000") // deep fund

	_, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), time.Now().UTC(), "USD")
	if err != nil {
		t.Fatal(err)
	}
	// Tier-1 executed: journal posted, fund debited, break resolved.
	if len(po.journals) != 1 {
		t.Fatalf("expected 1 tier-1 journal, got %d", len(po.journals))
	}
	if len(fu.debits) != 1 || !fu.debits[0].Equal(cmDec(t, "400")) {
		t.Fatalf("fund debits=%v want [400]", fu.debits)
	}
	for _, b := range st.breaks {
		if b.Kind == BreakShortfall && b.Status != BreakResolved {
			t.Fatalf("break %d not resolved: %s", b.ID, b.Status)
		}
	}
	if !al.has(alertClientMoneyShortfall) {
		t.Fatal("P1 CLIENT_MONEY_SHORTFALL alert missing")
	}
}

func TestShortfall_Tier2PendingAndDualControl(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, al, po, fu, ho, _, _, _ := newCMService(t, st, clk)
	ctx := context.Background()

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "1000")}}
	st.classified["USD"] = map[string]decimal.Decimal{ClassClient: cmDec(t, "600")}
	fu.bal["USD"] = cmDec(t, "100")  // fund covers 100, remainder 300
	ho.bal["USD"] = cmDec(t, "9000") // house can cover

	_, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), time.Now().UTC(), "USD")
	if err != nil {
		t.Fatal(err)
	}
	// Find the Tier-2 pending remediation.
	var t2 *Remediation
	for _, r := range st.rems {
		if r.Tier == TierHouse {
			cp := *r
			t2 = &cp
		}
	}
	if t2 == nil || t2.Status != RemPending || !t2.Amount.Equal(cmDec(t, "300")) {
		t.Fatalf("tier-2 remediation: %+v", t2)
	}
	if !al.has(alertTier2ApprovalRequired) {
		t.Fatal("tier-2 approval alert missing")
	}
	// Requester cannot approve own remediation (requested_by=1 system / recon id).
	if _, err := svc.ApproveRemediation(ctx, cmActor(t2.RequestedBy), t2.ID); err == nil {
		t.Fatal("self-approval must fail")
	}
	// A distinct Finance Ops principal approves → journal posts, break resolves.
	done, err := svc.ApproveRemediation(ctx, cmActor(uidFO2), t2.ID)
	if err != nil || done.Status != RemExecuted {
		t.Fatalf("approve: %v %+v", err, done)
	}
	if len(po.journals) != 2 { // tier-1 + tier-2
		t.Fatalf("journals=%d want 2", len(po.journals))
	}
	brk, _ := st.BreakByID(ctx, t2.BreakID)
	if brk.Status != BreakResolved {
		t.Fatalf("break status=%s want RESOLVED", brk.Status)
	}
}

func TestShortfall_ExceedsTier2_RaisesTier3Notice(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, _, _, fu, ho, _, _, no := newCMService(t, st, clk)
	ctx := context.Background()

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "10000")}}
	st.classified["USD"] = map[string]decimal.Decimal{ClassClient: cmDec(t, "1000")}
	fu.bal["USD"] = cmDec(t, "0")
	ho.bal["USD"] = cmDec(t, "2000") // shortfall 9000 exceeds house 2000

	_, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), time.Now().UTC(), "USD")
	if err != nil {
		t.Fatal(err)
	}
	var t3 *Remediation
	for _, r := range st.rems {
		if r.Tier == TierCapitalCall {
			cp := *r
			t3 = &cp
		}
	}
	if t3 == nil || !t3.Amount.Equal(cmDec(t, "7000")) {
		t.Fatalf("tier-3 remediation: %+v", t3)
	}
	// Automated regulator notice (60-minute SLA) dispatched.
	if len(no.sent) == 0 || no.sent[0].Trigger != NoticeTier3 {
		t.Fatalf("tier-3 notice not dispatched: %+v", no.sent)
	}
	if no.sent[0].DeadlineAt.Sub(clk.t) > RegulatorNoticeSLA {
		t.Fatal("notice deadline exceeds 60-minute SLA")
	}
}

func TestShortfall_BlocksMovements_503(t *testing.T) {
	st := newCMStore()
	svc, _, _, fu, _, _, _, _ := newCMService(t, st, &cmClock{t: time.Now().UTC()})
	ctx := context.Background()

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "100")}}
	st.classified["USD"] = map[string]decimal.Decimal{ClassClient: cmDec(t, "50")}
	fu.bal["USD"] = cmDec(t, "0")

	if _, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), time.Now().UTC(), "USD"); err != nil {
		t.Fatal(err)
	}
	err := svc.AssertClientMoneyMovement(ctx, "USD", cmDec(t, "10"))
	if err == nil {
		t.Fatal("outbound movement during shortfall must be refused")
	}
	if code := excerrors.CodeOf(err); code != CodeClientMoneyShortfall {
		t.Fatalf("code=%s want CLIENT_MONEY_SHORTFALL", code)
	}
	// EUR unaffected.
	if err := svc.AssertClientMoneyMovement(ctx, "EUR", cmDec(t, "10")); err != nil {
		t.Fatalf("unrelated currency blocked: %v", err)
	}
}

func TestGuardJournal_ProhibitsHouseUse(t *testing.T) {
	st := newCMStore()
	svc, _, _, _, _, _, _, _ := newCMService(t, st, &cmClock{t: time.Now().UTC()})
	ctx := context.Background()

	// Crediting a client-asset account without a registered op — refused.
	j := ledger.Journal{IdempotencyKey: "op:random",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro("USD"), "USD", cmDec(t, "5"), "x"),
			ledger.CreditLine(ledger.ClientMoneySegregated("USD"), "USD", cmDec(t, "5"), "y"),
		}}
	if err := svc.GuardJournal(ctx, j); err == nil {
		t.Fatal("unguarded credit of client-segregated account must fail")
	}
	// A journal not touching 1100–1149 credits passes.
	j2 := ledger.Journal{IdempotencyKey: "op:x",
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.Nostro("USD"), "USD", cmDec(t, "5"), "x"),
			ledger.CreditLine(ledger.HouseEquity("USD"), "USD", cmDec(t, "5"), "y"),
		}}
	if err := svc.GuardJournal(ctx, j2); err != nil {
		t.Fatalf("house-side journal blocked: %v", err)
	}
	// A remediation-keyed journal referencing a real remediation passes.
	rem, _ := st.InsertRemediation(ctx, Remediation{BreakID: 1, Tier: TierHouse,
		Action: ActHouseTopup, Currency: "USD", Amount: cmDec(t, "5"), Status: RemPending})
	j.IdempotencyKey = fmt.Sprintf("%s%d", idemRemediation, rem.ID)
	if err := svc.GuardJournal(ctx, j); err != nil {
		t.Fatalf("remediation-keyed journal blocked: %v", err)
	}
}

func TestRemoveExcess_DualControl(t *testing.T) {
	st := newCMStore()
	svc, _, po, _, _, _, _, _ := newCMService(t, st, &cmClock{t: time.Now().UTC()})
	ctx := context.Background()

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "100")}}
	st.classified["USD"] = map[string]decimal.Decimal{ClassClient: cmDec(t, "150")}
	rec, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), time.Now().UTC(), "USD")
	if err != nil || rec.Status != ReconExcess {
		t.Fatalf("recon=%s err=%v", rec.Status, err)
	}
	var exBreak *Break
	for _, b := range st.breaks {
		if b.Kind == BreakExcess {
			cp := *b
			exBreak = &cp
		}
	}
	if exBreak == nil {
		t.Fatal("no excess break opened")
	}
	// Missing approver → DUAL_CONTROL_REQUIRED.
	if _, err := svc.RemoveExcess(ctx, cmActor(uidFO), exBreak.ID, cmDec(t, "50")); err == nil {
		t.Fatal("excess removal without approver must fail")
	}
	// Same-principal approver → DUAL_CONTROL_VIOLATION.
	if _, err := svc.RemoveExcess(ctx, cmAct2(uidFO, uidFO), exBreak.ID, cmDec(t, "50")); err == nil {
		t.Fatal("same-principal approval must fail")
	}
	// Over-removal refused.
	if _, err := svc.RemoveExcess(ctx, cmAct2(uidFO, uidFO2), exBreak.ID, cmDec(t, "999")); err == nil {
		t.Fatal("removal beyond excess must fail")
	}
	done, err := svc.RemoveExcess(ctx, cmAct2(uidFO, uidFO2), exBreak.ID, cmDec(t, "50"))
	if err != nil || done.Status != BreakResolved {
		t.Fatalf("remove: %v %+v", err, done)
	}
	if len(po.journals) != 1 {
		t.Fatal("excess-removal journal not posted")
	}
}

// ---------------------------------------------------------------------------
// Task 24.3.11 — stress test & sweep & export
// ---------------------------------------------------------------------------

func TestStressTest_AdequateAndBreach(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, al, _, fu, ho, nb, _, _ := newCMService(t, st, clk)
	ctx := context.Background()
	day := time.Now().UTC()

	fu.bal["USD"] = cmDec(t, "100")
	ho.bal["USD"] = cmDec(t, "100")
	nb.worst["USD"] = cmDec(t, "50") // 200 ≥ 2×50 → adequate
	run, err := svc.RunStressTest(ctx, cmActor(uidFO), day, "USD")
	if err != nil || !run.Adequate {
		t.Fatalf("adequate run failed: %v %+v", err, run)
	}

	nb.worst["EUR"] = cmDec(t, "500")
	fu.bal["EUR"] = cmDec(t, "100")
	ho.bal["EUR"] = cmDec(t, "100") // 200 < 2×500 → breach
	run2, err := svc.RunStressTest(ctx, cmActor(uidFO), day, "EUR")
	if err != nil || run2.Adequate {
		t.Fatalf("breach run: %v %+v", err, run2)
	}
	if !al.has(alertStressBreach) {
		t.Fatal("stress-breach P1 alert missing")
	}
	found := false
	for _, b := range st.breaks {
		if b.Kind == BreakStressBreach && b.Currency == "EUR" {
			found = true
		}
	}
	if !found {
		t.Fatal("STRESS_BREACH break not persisted")
	}
}

func TestSweepDeadlines_EscalationAndTier4(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, al, _, fu, ho, _, su, _ := newCMService(t, st, clk)
	ctx := context.Background()

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "900")}}
	st.classified["USD"] = map[string]decimal.Decimal{ClassClient: cmDec(t, "100")}
	fu.bal["USD"] = cmDec(t, "0")
	ho.bal["USD"] = cmDec(t, "400") // T2 pending 400, remainder → T3

	if _, err := svc.RunDailyReconciliation(ctx, cmActor(uidFO), clk.t, "USD"); err != nil {
		t.Fatal(err)
	}

	// +31min: Tier-2 expired → escalated to Tier-3; shortfall still open.
	clk.t = clk.t.Add(31 * time.Minute)
	if err := svc.SweepDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	t3s, t2exp := 0, 0
	for _, r := range st.rems {
		if r.Tier == TierCapitalCall {
			t3s++
		}
		if r.Tier == TierHouse && r.Status == RemExpired {
			t2exp++
		}
	}
	if t2exp != 1 || t3s != 2 { // original T3 + escalated T3
		t.Fatalf("sweep: t2exp=%d t3=%d", t2exp, t3s)
	}

	// +61min total: CCO escalation + OVER_60M notice.
	clk.t = clk.t.Add(30 * time.Minute)
	if err := svc.SweepDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	var brk Break
	for _, b := range st.breaks {
		if b.Kind == BreakShortfall {
			brk = *b
		}
	}
	if brk.Status != BreakEscalated {
		t.Fatalf("break status=%s want ESCALATED", brk.Status)
	}
	if !al.has(alertCCOEscalation) {
		t.Fatal("CCO escalation P0 missing")
	}
	ex, _ := st.NoticeExists(ctx, brk.ID, NoticeOver60M)
	if !ex {
		t.Fatal("SHORTFALL_OVER_60M regulator notice missing")
	}

	// +4h total: Tier-4 suspension + default declaration.
	clk.t = clk.t.Add(4 * time.Hour)
	if err := svc.SweepDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	hasT4 := false
	for _, r := range st.rems {
		if r.Tier == TierDefault && r.Status == RemExecuted {
			hasT4 = true
		}
	}
	if !hasT4 {
		t.Fatal("tier-4 default remediation not recorded")
	}
	if len(su.calls) != 1 {
		t.Fatal("orderly-suspension trigger not invoked")
	}
	if !al.has(alertTier4Default) {
		t.Fatal("tier-4 default P0 alert missing")
	}
}

func TestExportPoolingPackage(t *testing.T) {
	st := newCMStore()
	svc, _, _, _, _, _, _, _ := newCMService(t, st, &cmClock{t: time.Now().UTC()})
	ctx := context.Background()

	st.entitlements["USD"] = []Entitlement{{AccountID: 1, Currency: "USD", Amount: cmDec(t, "100")}}
	if _, err := svc.ClassifyAccount(ctx, cmActor(uidFO), ClassifyInput{
		AccountKind: KindBank, BankIBAN: "DE89370400440532013000", BankName: "Seg Bank",
		Classification: ClassClient, Currency: "USD",
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"POOLING_EVENT", "WIND_DOWN"} {
		ex, err := svc.ExportPoolingPackage(ctx, cmActor(uidCO), kind)
		if err != nil {
			t.Fatal(err)
		}
		if ex.ExportKind != kind || ex.PackageSHA256 == "" || len(ex.Package) == 0 {
			t.Fatalf("export %+v incomplete", ex)
		}
		var pack map[string]any
		if err := json.Unmarshal(ex.Package, &pack); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"entitlements", "bank_accounts", "unresolved_breaks", "transfer_workflow"} {
			if _, ok := pack[k]; !ok {
				t.Fatalf("package missing %s", k)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Task 24.3.16 — CLS quarantine & real-time segregation
// ---------------------------------------------------------------------------

func TestCLSQuarantine_BlocksOutboundUntilAffirmed(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, al, _, _, _, _, _, _ := newCMService(t, st, clk)
	ctx := context.Background()

	q, err := svc.QuarantineBatch(ctx, cmActor(uidFO), "CLS-BATCH-7",
		json.RawMessage(`{"internal_amount":"100","cls_amount":"99.5"}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.ReasonCode != CodeCLSSettlementMismatch || !q.OutboundBlocked {
		t.Fatalf("quarantine %+v", q)
	}
	if !al.has(alertCLSQuarantine) {
		t.Fatal("CLS quarantine P1 missing")
	}
	// Idempotent: repeat delivery returns the same row.
	again, err := svc.QuarantineBatch(ctx, cmActor(uidFO), "CLS-BATCH-7", json.RawMessage(`{}`))
	if err != nil || again.ID != q.ID {
		t.Fatalf("repeat quarantine not idempotent: %v", err)
	}
	// Outbound blocked under CLS_SETTLEMENT_MISMATCH.
	err = svc.AssertOutboundAllowed(ctx, "CLS-BATCH-7")
	if code := excerrors.CodeOf(err); code != CodeCLSSettlementMismatch {
		t.Fatalf("code=%s want CLS_SETTLEMENT_MISMATCH", code)
	}
	// Affirmation requires a distinct approver.
	if _, err := svc.AffirmQuarantineRelease(ctx, cmActor(uidFO), q.ID); err == nil {
		t.Fatal("release without approver must fail")
	}
	if _, err := svc.AffirmQuarantineRelease(ctx, cmAct2(uidFO, uidFO), q.ID); err == nil {
		t.Fatal("same-principal release must fail")
	}
	rel, err := svc.AffirmQuarantineRelease(ctx, cmAct2(uidFO, uidCO), q.ID)
	if err != nil || rel.Status != QuarantineReleased || rel.OutboundBlocked {
		t.Fatalf("release: %v %+v", err, rel)
	}
	if err := svc.AssertOutboundAllowed(ctx, "CLS-BATCH-7"); err != nil {
		t.Fatalf("released batch still blocked: %v", err)
	}
}

func TestEvaluateSegregation_RealtimeShortfall(t *testing.T) {
	st := newCMStore()
	clk := &cmClock{t: time.Now().UTC()}
	svc, al, _, fu, ho, _, _, _ := newCMService(t, st, clk)
	ctx := context.Background()

	st.entitlements["EUR"] = []Entitlement{{AccountID: 9, Currency: "EUR", Amount: cmDec(t, "800")}}
	st.classified["EUR"] = map[string]decimal.Decimal{
		ClassClient: cmDec(t, "500"), ClassMargin: cmDec(t, "100"),
	}
	fu.bal["EUR"] = cmDec(t, "50")
	ho.bal["EUR"] = cmDec(t, "100")

	status, err := svc.EvaluateSegregation(ctx, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Shortfall || !status.Deficit.Equal(cmDec(t, "200")) {
		t.Fatalf("status %+v", status)
	}
	// Waterfall engaged: T1 50 + T2 pending 100 → remainder 50 T3 + notice.
	var t1, t2, t3 int
	for _, r := range st.rems {
		switch r.Tier {
		case TierInsurance:
			t1++
		case TierHouse:
			t2++
		case TierCapitalCall:
			t3++
		}
	}
	if t1 != 1 || t2 != 1 || t3 != 1 {
		t.Fatalf("waterfall tiers t1=%d t2=%d t3=%d", t1, t2, t3)
	}
	if !al.has(alertClientMoneyShortfall) {
		t.Fatal("shortfall P1 alert missing")
	}
	// Repeat evaluation joins the open break rather than duplicating.
	again, err := svc.EvaluateSegregation(ctx, "EUR")
	if err != nil || again.BreakID != status.BreakID {
		t.Fatalf("re-evaluated break id mismatch: %v", err)
	}
	// Outbound client-money movement is refused 503 while the break stands.
	if code := excerrors.CodeOf(svc.AssertClientMoneyMovement(ctx, "EUR", cmDec(t, "1"))); code != CodeClientMoneyShortfall {
		t.Fatal("movement during shortfall not refused with CLIENT_MONEY_SHORTFALL")
	}
	// 60-minute sweep escalates unresolved shortfalls.
	clk.t = clk.t.Add(61 * time.Minute)
	if err := svc.SweepDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	brk, _ := st.BreakByID(ctx, status.BreakID)
	if brk.Status != BreakEscalated {
		t.Fatalf("break %d status=%s want ESCALATED", brk.ID, brk.Status)
	}
}

// Nil-dep construction fails closed.
func TestConstruction_NilDepsFailClosed(t *testing.T) {
	if _, err := NewClientMoneyService(ClientMoneyDeps{}); err == nil {
		t.Fatal("nil store must fail")
	}
	if _, err := NewClientMoneyService(ClientMoneyDeps{Store: newCMStore()}); err == nil {
		t.Fatal("nil resolver must fail")
	}
}
