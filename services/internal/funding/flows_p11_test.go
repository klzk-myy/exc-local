// Unit tests for the Phase-11 flows cluster (Tasks 11.3.2, 11.3.3,
// 11.3.6, 11.3.10): whitelist mode + timelocks, withdrawal gate +
// cooldown + TOTP step-up, deposit dual-source/tiers/idempotency, and
// nostro-aware dispatch + replenishment dual control. Pure unit level
// (stubTx + flowStore) — live-DB coverage lands with the DSN-gated
// integration test.
package funding

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// flowStore — the fake backing every flows-cluster service seam.
// ---------------------------------------------------------------------------

type flowStore struct {
	fakeStore
	tx             *stubTx
	nextID         int64
	withdrawals    map[int64]*WithdrawalRow
	wdByIdem       map[string]*WithdrawalRow
	confirmations  map[int64]*ConfirmationRow // by withdrawal id
	confSeq        int64
	holdUntil      map[int64]*time.Time
	fundingTx      map[int64]*FundingTxRow
	depByIdem      map[string]*FundingTxRow
	payloadSHA     map[int64]*string
	depConf        map[int64][]DepositConfirmationRow
	depConfTaken   map[string]bool // "ftid|source"
	whitelist      map[int64]*WhitelistSettingsRow
	beneficiaries  map[string]*BeneficiaryRow // "acct|dest"
	destHolds      map[string]*DestinationHoldRow
	lastCompleted  map[string]time.Time
	dispatchQ      map[int64]*DispatchQueueRow // by withdrawal id
	opsAlerts      []FundingOpsAlertRow
	nostro         map[int64]*NostroBalanceRow
	replenishments map[int64]*ReplenishmentRow
}

func newFlowStore() *flowStore {
	return &flowStore{
		fakeStore:      fakeStore{meta: map[int64]*AccountMeta{}},
		tx:             &stubTx{},
		withdrawals:    map[int64]*WithdrawalRow{},
		wdByIdem:       map[string]*WithdrawalRow{},
		confirmations:  map[int64]*ConfirmationRow{},
		holdUntil:      map[int64]*time.Time{},
		fundingTx:      map[int64]*FundingTxRow{},
		depByIdem:      map[string]*FundingTxRow{},
		payloadSHA:     map[int64]*string{},
		depConf:        map[int64][]DepositConfirmationRow{},
		depConfTaken:   map[string]bool{},
		whitelist:      map[int64]*WhitelistSettingsRow{},
		beneficiaries:  map[string]*BeneficiaryRow{},
		destHolds:      map[string]*DestinationHoldRow{},
		lastCompleted:  map[string]time.Time{},
		dispatchQ:      map[int64]*DispatchQueueRow{},
		nostro:         map[int64]*NostroBalanceRow{},
		replenishments: map[int64]*ReplenishmentRow{},
	}
}

func (s *flowStore) BeginTx(context.Context) (pgx.Tx, error) { return s.tx, nil }

func ikey(acct int64, key string) string  { return fmt.Sprintf("%d|%s", acct, key) }
func dkey(acct int64, dest string) string { return fmt.Sprintf("%d|%s", acct, dest) }

// --- withdrawal create/confirm (Store subset) ---

func (s *flowStore) InsertWithdrawal(_ context.Context, _ pgx.Tx,
	w WithdrawalRow) (*WithdrawalRow, error) {
	if w.IdempotencyKey != nil {
		if _, dup := s.wdByIdem[ikey(w.AccountID, *w.IdempotencyKey)]; dup {
			return nil, ErrIdemConflict
		}
	}
	s.nextID++
	cp := w
	cp.ID = s.nextID
	cp.Status = FundingPending
	cp.CreatedAt = time.Now().UTC()
	s.withdrawals[cp.ID] = &cp
	if cp.IdempotencyKey != nil {
		s.wdByIdem[ikey(cp.AccountID, *cp.IdempotencyKey)] = &cp
	}
	return &cp, nil
}

func (s *flowStore) WithdrawalByIdemKey(_ context.Context, accountID int64,
	key string) (*WithdrawalRow, error) {
	if w, ok := s.wdByIdem[ikey(accountID, key)]; ok {
		return w, nil
	}
	return nil, errCode("NOT_FOUND", "withdrawal not found")
}

func (s *flowStore) WithdrawalForUpdate(_ context.Context, _ pgx.Tx,
	id int64) (*WithdrawalRow, error) {
	if w, ok := s.withdrawals[id]; ok {
		return w, nil
	}
	return nil, errCode("NOT_FOUND", "withdrawal not found")
}

func (s *flowStore) WithdrawalByID(_ context.Context, id int64) (*WithdrawalRow, error) {
	if w, ok := s.withdrawals[id]; ok {
		return w, nil
	}
	return nil, nil
}

func (s *flowStore) SetWithdrawalStatus(_ context.Context, _ pgx.Tx, id int64,
	status string, confirmedAt, reviewDeadline *time.Time) error {
	w, ok := s.withdrawals[id]
	if !ok {
		return errCode("NOT_FOUND", "withdrawal not found")
	}
	w.Status = status
	if confirmedAt != nil {
		// WithdrawalRow has no ConfirmedAt field — status is the record.
	}
	if reviewDeadline != nil {
		w.ReviewDeadline = reviewDeadline
	}
	return nil
}

func (s *flowStore) MarkWithdrawalFailed(_ context.Context, id int64) error {
	if w, ok := s.withdrawals[id]; ok {
		w.Status = FundingFailed
		return nil
	}
	return errCode("NOT_FOUND", "withdrawal not found")
}

func (s *flowStore) InsertConfirmation(_ context.Context, _ pgx.Tx,
	c ConfirmationRow) (int64, error) {
	s.confSeq++
	cp := c
	cp.ID = s.confSeq
	cp.Status = "pending"
	s.confirmations[cp.WithdrawalID] = &cp
	return cp.ID, nil
}

func (s *flowStore) ConfirmationForUpdate(_ context.Context, _ pgx.Tx,
	withdrawalID int64) (*ConfirmationRow, error) {
	if c, ok := s.confirmations[withdrawalID]; ok {
		return c, nil
	}
	return nil, errCode("NOT_FOUND", "confirmation not found")
}

func (s *flowStore) SetConfirmationStatus(_ context.Context, _ pgx.Tx, id int64,
	status string, confirmedBy int64, method string) error {
	for _, c := range s.confirmations {
		if c.ID == id {
			c.Status = status
			if confirmedBy != 0 {
				uid := confirmedBy
				c.ConfirmedBy = &uid
			}
			if method != "" {
				m := method
				c.Method = &m
			}
			return nil
		}
	}
	return errCode("NOT_FOUND", "confirmation not found")
}

// --- whitelist / beneficiary / destination holds ---

func (s *flowStore) WhitelistSettings(_ context.Context,
	accountID int64) (*WhitelistSettingsRow, error) {
	if r, ok := s.whitelist[accountID]; ok {
		return r, nil
	}
	return nil, nil
}

func (s *flowStore) WhitelistSettingsForUpdate(_ context.Context, _ pgx.Tx,
	accountID int64) (*WhitelistSettingsRow, error) {
	return s.WhitelistSettings(context.Background(), accountID)
}

func (s *flowStore) UpsertWhitelistSettings(_ context.Context, _ pgx.Tx,
	r WhitelistSettingsRow) error {
	cp := r
	cp.UpdatedAt = time.Now().UTC()
	s.whitelist[r.AccountID] = &cp
	return nil
}

func (s *flowStore) WhitelistedBeneficiaries(_ context.Context,
	accountID int64) ([]BeneficiaryRow, error) {
	var out []BeneficiaryRow
	for _, b := range s.beneficiaries {
		if b.AccountID == accountID && b.Status == BeneficiaryVerified {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (s *flowStore) BeneficiaryByDestination(_ context.Context, accountID int64,
	destination string) (*BeneficiaryRow, error) {
	if b, ok := s.beneficiaries[dkey(accountID, destination)]; ok {
		return b, nil
	}
	return nil, nil
}

func (s *flowStore) DestinationHold(_ context.Context, _ pgx.Tx, accountID int64,
	destination string) (*DestinationHoldRow, error) {
	if h, ok := s.destHolds[dkey(accountID, destination)]; ok {
		return h, nil
	}
	return nil, nil
}

func (s *flowStore) UpsertDestinationHold(_ context.Context, _ pgx.Tx,
	accountID int64, destination string, unlockedAt time.Time) (*DestinationHoldRow, error) {
	k := dkey(accountID, destination)
	if h, ok := s.destHolds[k]; ok {
		return h, nil
	}
	s.nextID++
	h := &DestinationHoldRow{ID: s.nextID, AccountID: accountID,
		Destination: destination, FirstSeen: time.Now().UTC(), UnlockedAt: unlockedAt}
	s.destHolds[k] = h
	return h, nil
}

func (s *flowStore) LastCompletedWithdrawalAt(_ context.Context, accountID int64,
	destination string) (*time.Time, error) {
	if t, ok := s.lastCompleted[dkey(accountID, destination)]; ok {
		cp := t
		return &cp, nil
	}
	return nil, nil
}

func (s *flowStore) SetWithdrawalHoldUntil(_ context.Context, _ pgx.Tx,
	id int64, until *time.Time) error {
	if _, ok := s.withdrawals[id]; !ok {
		return errCode("NOT_FOUND", "withdrawal not found")
	}
	s.holdUntil[id] = until
	return nil
}

func (s *flowStore) SetWithdrawalReview(_ context.Context, _ pgx.Tx, id int64,
	status string, reviewedBy int64, at time.Time) error {
	w, ok := s.withdrawals[id]
	if !ok {
		return errCode("NOT_FOUND", "withdrawal not found")
	}
	w.Status = status
	return nil
}

// --- dispatch queue / nostro / alerts / replenishment ---

func (s *flowStore) WithdrawalForDispatch(_ context.Context, _ pgx.Tx,
	id int64) (*DispatchableWithdrawal, error) {
	w, ok := s.withdrawals[id]
	if !ok {
		return nil, errCode("NOT_FOUND", "withdrawal not found")
	}
	return &DispatchableWithdrawal{
		ID: w.ID, AccountID: w.AccountID, Currency: w.Currency,
		Amount: w.Amount, Status: w.Status,
		ReferenceAccount: w.ReferenceAccount, BankMethod: w.BankMethod,
		HoldUntil: s.holdUntil[id],
	}, nil
}

func (s *flowStore) ConfirmedForDispatch(_ context.Context,
	limit int) ([]DispatchableWithdrawal, error) {
	var out []DispatchableWithdrawal
	for _, w := range s.withdrawals {
		if w.Status != FundingConfirmed {
			continue
		}
		out = append(out, DispatchableWithdrawal{
			ID: w.ID, AccountID: w.AccountID, Currency: w.Currency,
			Amount: w.Amount, Status: w.Status,
			ReferenceAccount: w.ReferenceAccount, BankMethod: w.BankMethod,
			HoldUntil: s.holdUntil[w.ID],
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *flowStore) SetWithdrawalCompleted(_ context.Context, _ pgx.Tx,
	id int64, at time.Time) error {
	w, ok := s.withdrawals[id]
	if !ok {
		return errCode("NOT_FOUND", "withdrawal not found")
	}
	w.Status = FundingCompleted
	s.lastCompleted[dkey(w.AccountID, strVal(w.ReferenceAccount))] = at
	return nil
}

func (s *flowStore) InsertDispatchQueue(_ context.Context, _ pgx.Tx,
	withdrawalID int64, reason string) (*DispatchQueueRow, error) {
	if q, ok := s.dispatchQ[withdrawalID]; ok {
		q.Reason = reason
		return q, nil
	}
	s.nextID++
	q := &DispatchQueueRow{ID: s.nextID, WithdrawalID: withdrawalID,
		Status: QueueQueued, Reason: reason, QueuedAt: time.Now().UTC()}
	s.dispatchQ[withdrawalID] = q
	return q, nil
}

func (s *flowStore) DispatchQueueForUpdate(_ context.Context, _ pgx.Tx,
	withdrawalID int64) (*DispatchQueueRow, error) {
	if q, ok := s.dispatchQ[withdrawalID]; ok {
		return q, nil
	}
	return nil, nil
}

func (s *flowStore) SetDispatchQueueStatus(_ context.Context, _ pgx.Tx,
	id int64, status string, lastErr *string, dispatchedAt *time.Time) error {
	for _, q := range s.dispatchQ {
		if q.ID == id {
			q.Status = status
			q.Attempts++
			q.LastError = lastErr
			if dispatchedAt != nil {
				q.DispatchedAt = dispatchedAt
			}
			return nil
		}
	}
	return errCode("NOT_FOUND", "dispatch queue row not found")
}

func (s *flowStore) InsertFundingOpsAlert(_ context.Context, _ pgx.Tx,
	a FundingOpsAlertRow) (int64, error) {
	s.nextID++
	a.ID = s.nextID
	a.CreatedAt = time.Now().UTC()
	s.opsAlerts = append(s.opsAlerts, a)
	return a.ID, nil
}

func (s *flowStore) ListFundingOpsAlerts(_ context.Context,
	limit int) ([]FundingOpsAlertRow, error) {
	if len(s.opsAlerts) > limit {
		return s.opsAlerts[:limit], nil
	}
	return s.opsAlerts, nil
}

func (s *flowStore) NostroBalances(_ context.Context,
	currency string) ([]NostroBalanceRow, error) {
	var out []NostroBalanceRow
	for _, n := range s.nostro {
		if currency == "" || n.Currency == currency {
			out = append(out, *n)
		}
	}
	return out, nil
}

func (s *flowStore) NostroForUpdate(_ context.Context, _ pgx.Tx,
	id int64) (*NostroBalanceRow, error) {
	if n, ok := s.nostro[id]; ok {
		return n, nil
	}
	return nil, errf("NOT_FOUND", "nostro account %d not found", id)
}

func (s *flowStore) AdjustNostroBalance(_ context.Context, _ pgx.Tx,
	id int64, delta decimal.Decimal) (*NostroBalanceRow, error) {
	n, ok := s.nostro[id]
	if !ok {
		return nil, errf("NOT_FOUND", "nostro account %d not found", id)
	}
	next := n.Balance.Add(delta)
	if next.IsNegative() {
		return nil, errCode("INSUFFICIENT_BALANCE", "nostro overdraft refused")
	}
	n.Balance = next
	return n, nil
}

func (s *flowStore) InsertReplenishmentRequest(_ context.Context, _ pgx.Tx,
	r ReplenishmentRow) (*ReplenishmentRow, error) {
	s.nextID++
	cp := r
	cp.ID = s.nextID
	cp.Status = ReplenPending
	cp.CreatedAt = time.Now().UTC()
	s.replenishments[cp.ID] = &cp
	return &cp, nil
}

func (s *flowStore) ReplenishmentForUpdate(_ context.Context, _ pgx.Tx,
	id int64) (*ReplenishmentRow, error) {
	if r, ok := s.replenishments[id]; ok {
		return r, nil
	}
	return nil, errf("NOT_FOUND", "replenishment %d not found", id)
}

func (s *flowStore) SetReplenishmentStatus(_ context.Context, _ pgx.Tx,
	id int64, status string, approvedBy int64, note *string, at time.Time) error {
	r, ok := s.replenishments[id]
	if !ok {
		return errCode("NOT_FOUND", "replenishment not found")
	}
	r.Status = status
	r.ApprovedBy = &approvedBy
	r.DecisionNote = note
	r.DecidedAt = &at
	return nil
}

func (s *flowStore) ListReplenishments(_ context.Context,
	limit int) ([]ReplenishmentRow, error) {
	var out []ReplenishmentRow
	for _, r := range s.replenishments {
		out = append(out, *r)
	}
	return out, nil
}

// --- deposit seams ---

func (s *flowStore) InsertDepositPending(_ context.Context, _ pgx.Tx,
	d DepositRow) (*DepositRow, error) {
	if d.IdempotencyKey != nil {
		if _, dup := s.depByIdem[ikey(d.AccountID, *d.IdempotencyKey)]; dup {
			return nil, ErrIdemConflict
		}
	}
	s.nextID++
	cp := d
	cp.ID = s.nextID
	cp.CreatedAt = time.Now().UTC()
	if cp.Status == "" {
		cp.Status = FundingPending
	}
	fx := &FundingTxRow{
		ID: cp.ID, AccountID: cp.AccountID, Currency: cp.Currency,
		Type: "DEPOSIT", Amount: cp.Amount, Status: cp.Status,
		Reference: cp.Reference, BankMethod: cp.BankMethod,
		ReferenceAccount: cp.ReferenceAccount,
		USDAmount:        cp.USDAmount, ReviewTier: cp.ReviewTier,
		CreatedAt: cp.CreatedAt,
	}
	s.fundingTx[cp.ID] = fx
	s.payloadSHA[cp.ID] = cp.PayloadSHA256
	if cp.IdempotencyKey != nil {
		s.depByIdem[ikey(cp.AccountID, *cp.IdempotencyKey)] = fx
	}
	return &cp, nil
}

func (s *flowStore) DepositByIdemKey(_ context.Context, accountID int64,
	key string) (*FundingTxRow, error) {
	if r, ok := s.depByIdem[ikey(accountID, key)]; ok {
		return r, nil
	}
	return nil, errCode("NOT_FOUND", "deposit not found")
}

func (s *flowStore) DepositByReference(_ context.Context, accountID int64,
	reference string) (*FundingTxRow, error) {
	for _, r := range s.fundingTx {
		if r.AccountID == accountID && r.Type == "DEPOSIT" &&
			r.Status == FundingPending && r.Reference != nil &&
			*r.Reference == reference {
			return r, nil
		}
	}
	return nil, errCode("NOT_FOUND", "deposit intent not found")
}

func (s *flowStore) DepositByBankRef(_ context.Context, accountID int64,
	reference string) (*FundingTxRow, error) {
	for _, r := range s.fundingTx {
		if r.AccountID == accountID && r.Type == "DEPOSIT" &&
			r.Reference != nil && *r.Reference == reference {
			return r, nil
		}
	}
	return nil, errCode("NOT_FOUND", "deposit not found")
}

func (s *flowStore) StampDepositBankRef(_ context.Context, _ pgx.Tx,
	id int64, bankRef, reviewTier string) error {
	r, ok := s.fundingTx[id]
	if !ok || r.Type != "DEPOSIT" || r.Status != FundingPending {
		return errf("INVALID_LIFECYCLE_TRANSITION", "deposit %d not adoptable", id)
	}
	r.Reference = &bankRef
	r.ReviewTier = &reviewTier
	return nil
}

func (s *flowStore) FundingTxForUpdate(_ context.Context, _ pgx.Tx,
	id int64) (*FundingTxRow, error) {
	if r, ok := s.fundingTx[id]; ok {
		return r, nil
	}
	return nil, errCode("NOT_FOUND", "funding transaction not found")
}

func (s *flowStore) FundingTxPayloadSHA(_ context.Context, _ pgx.Tx,
	id int64) (*string, error) {
	if ph, ok := s.payloadSHA[id]; ok {
		return ph, nil
	}
	return nil, errCode("NOT_FOUND", "funding transaction not found")
}

func (s *flowStore) InsertDepositConfirmation(_ context.Context, _ pgx.Tx,
	c DepositConfirmationRow) (*DepositConfirmationRow, error) {
	k := fmt.Sprintf("%d|%s", c.FundingTransactionID, c.Source)
	if s.depConfTaken[k] {
		return nil, ErrIdemConflict
	}
	s.nextID++
	cp := c
	cp.ID = s.nextID
	cp.CreatedAt = time.Now().UTC()
	s.depConf[cp.FundingTransactionID] = append(s.depConf[cp.FundingTransactionID], cp)
	s.depConfTaken[k] = true
	return &cp, nil
}

func (s *flowStore) DepositConfirmations(_ context.Context, _ pgx.Tx,
	fundingTxID int64) ([]DepositConfirmationRow, error) {
	return s.depConf[fundingTxID], nil
}

func (s *flowStore) DepositVelocity(_ context.Context, accountID int64,
	since time.Time) (int64, decimal.Decimal, error) {
	var n int64
	sum := decimal.Zero
	for _, r := range s.fundingTx {
		if r.AccountID != accountID || r.Type != "DEPOSIT" ||
			!r.CreatedAt.After(since) {
			continue
		}
		switch r.Status {
		case FundingPending, FundingConfirmed, FundingPendingReview, FundingCompleted:
			n++
			sum = sum.Add(r.Amount)
		}
	}
	return n, sum, nil
}

func (s *flowStore) SetDepositReview(_ context.Context, _ pgx.Tx, id int64,
	status string, deadline *time.Time, reviewedBy *int64, at time.Time) error {
	r, ok := s.fundingTx[id]
	if !ok || r.Type != "DEPOSIT" {
		return errf("NOT_FOUND", "deposit %d not found", id)
	}
	r.Status = status
	if deadline != nil {
		r.ReviewDeadline = deadline
	}
	return nil
}

func (s *flowStore) SetDepositCompleted(_ context.Context, _ pgx.Tx,
	id int64, at time.Time) error {
	r, ok := s.fundingTx[id]
	if !ok || r.Type != "DEPOSIT" {
		return errf("NOT_FOUND", "deposit %d not found", id)
	}
	r.Status = FundingCompleted
	r.CompletedAt = &at
	return nil
}

func (s *flowStore) SetFundingTxStatus(_ context.Context, _ pgx.Tx, id int64,
	status string, completedAt *time.Time) error {
	r, ok := s.fundingTx[id]
	if !ok {
		return errCode("NOT_FOUND", "funding transaction not found")
	}
	r.Status = status
	r.CompletedAt = completedAt
	return nil
}

func (s *flowStore) ExpiredReviewDeadlines(_ context.Context,
	limit int) ([]int64, error) {
	var out []int64
	for _, r := range s.fundingTx {
		if r.Status == FundingPendingReview && r.ReviewDeadline != nil &&
			r.ReviewDeadline.Before(time.Now()) {
			out = append(out, r.ID)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *flowStore) BumpReviewDeadline(_ context.Context, _ pgx.Tx,
	id int64, deadline time.Time) error {
	r, ok := s.fundingTx[id]
	if !ok || r.Status != FundingPendingReview {
		return errCode("INVALID_LIFECYCLE_TRANSITION", "not PENDING_REVIEW")
	}
	r.ReviewDeadline = &deadline
	return nil
}

// compile-time seam assertions.
var (
	_ WhitelistStore = (*flowStore)(nil)
	_ GateStore      = (*flowStore)(nil)
	_ DepositStore   = (*flowStore)(nil)
	_ NostroStore    = (*flowStore)(nil)
	_ FlowStore      = (*flowStore)(nil)
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// dec() is shared with rails_p11_test.go (same package).

func timePtr(t time.Time) *time.Time { return &t }

type staticTOTPProvider struct{ secret string }

func (p staticTOTPProvider) TOTPSecretForAccount(context.Context, int64) (string, error) {
	return p.secret, nil
}

func alwaysVerify(string, string, time.Time) bool { return true }
func neverVerify(string, string, time.Time) bool  { return false }

type recordingDispatcher struct{ released []int64 }

func (d *recordingDispatcher) Release(_ context.Context, id int64) (*ReleaseResult, error) {
	d.released = append(d.released, id)
	return &ReleaseResult{WithdrawalID: id, Disposition: "SKIPPED"}, nil
}

// usdIdentity prices any currency 1:1 with USD for tier tests.
type usdIdentity struct{}

func (usdIdentity) ToUSD(_ context.Context, _ string,
	amount decimal.Decimal) (decimal.Decimal, error) {
	return amount, nil
}

// flowFixture wires a WithdrawalService + FlowService over the fake.
func flowFixture(t *testing.T) (*FlowService, *flowStore, *fakePoster, *recordingDispatcher) {
	t.Helper()
	st := newFlowStore()
	poster := &fakePoster{}
	checker := fakeChecker{status: map[int64]string{7: "ACTIVE"}}
	inner, err := NewWithdrawalService(st, poster, checker)
	if err != nil {
		t.Fatalf("withdrawal service: %v", err)
	}
	inner.WithUSDConverter(usdIdentity{})
	flow, err := NewFlowService(inner, st)
	if err != nil {
		t.Fatalf("flow service: %v", err)
	}
	disp := &recordingDispatcher{}
	flow.WithDispatcher(disp).
		WithTOTP(staticTOTPProvider{secret: "JBSWY3DPEHPK3PXP"}, alwaysVerify)
	return flow, st, poster, disp
}

// ---------------------------------------------------------------------------
// Task 11.3.10 — whitelist gate
// ---------------------------------------------------------------------------

func TestGateWhitelistOnlyRejectsUnverified(t *testing.T) {
	st := newFlowStore()
	now := time.Now().UTC()
	st.whitelist[7] = &WhitelistSettingsRow{AccountID: 7, Mode: WhitelistModeOnly}
	_, err := CheckWithdrawalGate(context.Background(), st, now, 7, "DE89370400440532013000")
	requireErrCode(t, err, CodeWithdrawalWhitelistOnly)
}

func TestGateWhitelistOnlyAdmitsVerifiedUnlocked(t *testing.T) {
	st := newFlowStore()
	now := time.Now().UTC()
	st.whitelist[7] = &WhitelistSettingsRow{AccountID: 7, Mode: WhitelistModeOnly}
	unlocked := now.Add(-time.Hour)
	st.beneficiaries[dkey(7, "DE89370400440532013000")] = &BeneficiaryRow{
		BankAccountID: 11, AccountID: 7, Status: BeneficiaryVerified,
		UnlockedAt: &unlocked,
	}
	out, err := CheckWithdrawalGate(context.Background(), st, now, 7, "DE89370400440532013000")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if out.Unverified || out.Beneficiary == nil {
		t.Fatalf("expected verified beneficiary admission, got %+v", out)
	}
}

func TestGateBeneficiaryTimelock(t *testing.T) {
	st := newFlowStore()
	now := time.Now().UTC()
	st.whitelist[7] = &WhitelistSettingsRow{AccountID: 7, Mode: WhitelistModeOnly}
	locked := now.Add(23 * time.Hour) // verified 1h ago → 23h remaining
	st.beneficiaries[dkey(7, "IBAN1")] = &BeneficiaryRow{
		BankAccountID: 11, AccountID: 7, Status: BeneficiaryVerified,
		UnlockedAt: &locked,
	}
	_, err := CheckWithdrawalGate(context.Background(), st, now, 7, "IBAN1")
	requireErrCode(t, err, CodeBeneficiaryHoldActive)

	// Under ALLOW_ALL the same locked beneficiary is admitted with a
	// carry-over LockedUntil (the hold rides on the withdrawal).
	st.whitelist[7].Mode = WhitelistModeAllowAll
	out, err := CheckWithdrawalGate(context.Background(), st, now, 7, "IBAN1")
	if err != nil {
		t.Fatalf("allow-all gate: %v", err)
	}
	if out.LockedUntil == nil || !out.LockedUntil.Equal(locked) {
		t.Fatalf("expected LockedUntil %s, got %+v", locked, out)
	}
}

func TestGateDeactivationLockBlocks(t *testing.T) {
	st := newFlowStore()
	now := time.Now().UTC()
	lock := now.Add(20 * time.Hour)
	st.whitelist[7] = &WhitelistSettingsRow{AccountID: 7,
		Mode: WhitelistModeAllowAll, WithdrawalLockUntil: &lock}
	_, err := CheckWithdrawalGate(context.Background(), st, now, 7, "IBAN1")
	requireErrCode(t, err, CodeWithdrawalWhitelistLocked)
}

func TestGateAllowAllUnverified(t *testing.T) {
	st := newFlowStore()
	out, err := CheckWithdrawalGate(context.Background(), st,
		time.Now().UTC(), 7, "NEWDEST")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if !out.Unverified {
		t.Fatalf("expected Unverified outcome, got %+v", out)
	}
}

// ---------------------------------------------------------------------------
// Task 11.3.10 — WhitelistService enable/disable lifecycle
// ---------------------------------------------------------------------------

func TestWhitelistEnableDisableLatch(t *testing.T) {
	st := newFlowStore()
	svc, err := NewWhitelistService(st)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	ctx := context.Background()

	v, err := svc.Enable(ctx, 7, 42)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if v.Mode != WhitelistModeOnly || !v.WhitelistOnly {
		t.Fatalf("expected WHITELIST_ONLY, got %+v", v)
	}

	v, err = svc.Disable(ctx, 7, 42)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if v.Mode != WhitelistModeAllowAll {
		t.Fatalf("expected ALLOW_ALL, got %+v", v)
	}
	if !v.WithdrawalsLocked || !v.ReenableLocked {
		t.Fatalf("expected both latches set, got %+v", v)
	}
	if v.WithdrawalLockUntil == nil ||
		!v.WithdrawalLockUntil.Equal(now.Add(WhitelistLockDuration)) {
		t.Fatalf("egress lock must be now+24h, got %v", v.WithdrawalLockUntil)
	}

	// Re-enable inside the latch → WHITELIST_CHANGE_LOCKED.
	_, err = svc.Enable(ctx, 7, 42)
	requireErrCode(t, err, CodeWhitelistChangeLocked)

	// After the latch lapses re-enable succeeds and clears the egress lock.
	later := now.Add(WhitelistLockDuration + time.Minute)
	svc.WithClock(func() time.Time { return later })
	v, err = svc.Enable(ctx, 7, 42)
	if err != nil {
		t.Fatalf("re-enable after latch: %v", err)
	}
	if v.Mode != WhitelistModeOnly || v.WithdrawalsLocked {
		t.Fatalf("expected clean WHITELIST_ONLY, got %+v", v)
	}
}

func TestWhitelistDisableIdempotent(t *testing.T) {
	st := newFlowStore()
	svc, _ := NewWhitelistService(st)
	v, err := svc.Disable(context.Background(), 7, 42)
	if err != nil {
		t.Fatalf("disable on default: %v", err)
	}
	if v.Mode != WhitelistModeAllowAll || v.WithdrawalsLocked {
		t.Fatalf("disable on ALLOW_ALL must be a no-op replay, got %+v", v)
	}
}

// ---------------------------------------------------------------------------
// Task 11.3.2 — create gates + TOTP confirm
// ---------------------------------------------------------------------------

func TestFlowCreateCooldownReject(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	st.lastCompleted[dkey(7, "IBAN9")] = time.Now().UTC().Add(-10 * time.Minute)
	_, err := flow.Create(context.Background(), CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "100",
		ReferenceAccount: "IBAN9", IdempotencyKey: "cd-1",
	})
	requireErrCode(t, err, "WITHDRAWAL_COOLDOWN_ACTIVE")
}

func TestFlowCreateUnverifiedDestinationHold(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	res, err := flow.Create(context.Background(), CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "100",
		ReferenceAccount: "NEWIBAN", IdempotencyKey: "uv-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Status != FundingPending {
		t.Fatalf("expected PENDING, got %s", res.Status)
	}
	hu := st.holdUntil[res.WithdrawalID]
	if hu == nil {
		t.Fatalf("unverified destination must stamp hold_until")
	}
	if d := time.Until(*hu); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("hold must be ~24h, got %s", d)
	}
	if _, ok := st.destHolds[dkey(7, "NEWIBAN")]; !ok {
		t.Fatalf("first-seen destination hold row missing")
	}
}

func TestFlowCreateWhitelistLockedRejects(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	lock := time.Now().UTC().Add(20 * time.Hour)
	st.whitelist[7] = &WhitelistSettingsRow{AccountID: 7,
		Mode: WhitelistModeAllowAll, WithdrawalLockUntil: &lock}
	_, err := flow.Create(context.Background(), CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "100",
		ReferenceAccount: "IBAN9", IdempotencyKey: "wl-1",
	})
	requireErrCode(t, err, CodeWithdrawalWhitelistLocked)
}

func TestFlowConfirmRequiresTOTP(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	res, err := flow.Create(context.Background(), CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "100",
		ReferenceAccount: "IBAN9", IdempotencyKey: "cf-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := StepUpConfirmRequest{
		ConfirmWithdrawalRequest: ConfirmWithdrawalRequest{
			WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 1,
			Token: res.ConfirmToken,
		},
	}
	// No X-2FA-Token and no session elevation → TWO_FACTOR_REQUIRED.
	_, err = flow.ConfirmStepUp(context.Background(), req)
	requireErrCode(t, err, "TWO_FACTOR_REQUIRED")

	// A failing verifier rejects a presented code.
	flow.verify = neverVerify
	req.TOTPToken = "000000"
	_, err = flow.ConfirmStepUp(context.Background(), req)
	requireErrCode(t, err, "TWO_FACTOR_REQUIRED")

	// A passing verifier admits; the AUTO tier lands CONFIRMED and the
	// dispatcher hand-off fires.
	flow.verify = alwaysVerify
	out, err := flow.ConfirmStepUp(context.Background(), req)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingConfirmed {
		t.Fatalf("expected CONFIRMED, got %s", out.Status)
	}
	if len(flow.dispatch.(*recordingDispatcher).released) == 0 {
		t.Fatalf("confirmed withdrawal must hand off to the dispatcher")
	}
}

func TestFlowConfirmSessionElevationSkipsToken(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	res, err := flow.Create(context.Background(), CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "100",
		ReferenceAccount: "IBAN9", IdempotencyKey: "cf-2",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := flow.ConfirmStepUp(context.Background(), StepUpConfirmRequest{
		ConfirmWithdrawalRequest: ConfirmWithdrawalRequest{
			WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 1,
			Token: res.ConfirmToken,
		},
		SessionTwoFactor: true,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingConfirmed {
		t.Fatalf("expected CONFIRMED, got %s", out.Status)
	}
}

func TestFlowConfirmWithoutTOTPProviderFailsClosed(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	flow.totp = nil
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	res, err := flow.Create(context.Background(), CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "100",
		ReferenceAccount: "IBAN9", IdempotencyKey: "cf-3",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = flow.ConfirmStepUp(context.Background(), StepUpConfirmRequest{
		ConfirmWithdrawalRequest: ConfirmWithdrawalRequest{
			WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 1,
			Token: res.ConfirmToken,
		},
		TOTPToken: "123456",
	})
	requireErrCode(t, err, "TWO_FACTOR_REQUIRED")
}

func TestFlowAdminReviewFourEyes(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	// >$50K → PENDING_REVIEW on confirm (usdIdentity prices 1:1).
	res, err := flow.Create(context.Background(), CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "60000",
		ReferenceAccount: "IBAN9", IdempotencyKey: "rv-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := flow.ConfirmStepUp(context.Background(), StepUpConfirmRequest{
		ConfirmWithdrawalRequest: ConfirmWithdrawalRequest{
			WithdrawalID: res.WithdrawalID, AccountID: 7, UserID: 1,
			Token: res.ConfirmToken,
		},
		SessionTwoFactor: true,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingPendingReview {
		t.Fatalf("expected PENDING_REVIEW for $60K, got %s", out.Status)
	}
	// Same-principal approve violates four-eyes.
	_, err = flow.AdminReview(context.Background(), 5, 5, res.WithdrawalID, true, "")
	requireErrCode(t, err, "DUAL_CONTROL_VIOLATION")
	// Missing approver → DUAL_CONTROL_REQUIRED.
	_, err = flow.AdminReview(context.Background(), 5, 0, res.WithdrawalID, true, "")
	requireErrCode(t, err, "DUAL_CONTROL_REQUIRED")
	// Distinct approver → CONFIRMED + dispatch hand-off.
	rv, err := flow.AdminReview(context.Background(), 5, 9, res.WithdrawalID, true, "ok")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if rv.Status != FundingConfirmed {
		t.Fatalf("expected CONFIRMED, got %s", rv.Status)
	}
}

// ---------------------------------------------------------------------------
// Task 11.3.3 — deposit lifecycle
// ---------------------------------------------------------------------------

func depositFixture(t *testing.T) (*DepositService, *flowStore, *fakePoster) {
	t.Helper()
	st := newFlowStore()
	poster := &fakePoster{}
	checker := fakeChecker{status: map[int64]string{7: "ACTIVE"}}
	svc, err := NewDepositService(st, poster, checker)
	if err != nil {
		t.Fatalf("deposit service: %v", err)
	}
	svc.WithUSDConverter(usdIdentity{})
	return svc, st, poster
}

func TestDepositIntentIdempotency(t *testing.T) {
	svc, _, _ := depositFixture(t)
	ctx := context.Background()
	req := CreateDepositIntentRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "5000",
		Reference: "WIRE-1", IdempotencyKey: "k1",
	}
	r1, err := svc.CreateIntent(ctx, req)
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	if r1.Status != FundingPending {
		t.Fatalf("expected PENDING intent, got %s", r1.Status)
	}
	// Same key + same payload → replay.
	r2, err := svc.CreateIntent(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !r2.Replayed || r2.DepositID != r1.DepositID {
		t.Fatalf("expected replay of deposit %d, got %+v", r1.DepositID, r2)
	}
	// Same key + different payload → IDEMPOTENCY_KEY_MISMATCH.
	req.Amount = "6000"
	_, err = svc.CreateIntent(ctx, req)
	requireErrCode(t, err, "IDEMPOTENCY_KEY_MISMATCH")
	// Missing key → INVALID_REQUEST.
	req.Amount = "5000"
	req.IdempotencyKey = ""
	_, err = svc.CreateIntent(ctx, req)
	requireErrCode(t, err, "INVALID_REQUEST")
}

func TestDepositDualSourceAutoCredit(t *testing.T) {
	svc, _, poster := depositFixture(t)
	ctx := context.Background()
	res, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 7, Currency: "USD", Amount: "5000",
		Reference: "BNK-TX-1", Source: "STATEMENT", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Status != FundingPending || res.Confirmations != 1 {
		t.Fatalf("expected PENDING + 1 confirmation, got %+v", res)
	}
	if len(poster.journals) != 1 {
		t.Fatalf("detection must post the hold journal")
	}
	out, err := svc.Confirm(ctx, DepositConfirmRequest{
		DepositID: res.DepositID, Source: "WEBHOOK",
		SenderName: "Alice Example", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingCompleted {
		t.Fatalf("<$10K auto tier must credit, got %s (flags %v)", out.Status, out.Flags)
	}
	if len(poster.journals) != 2 {
		t.Fatalf("credit journal must post after dual-source")
	}
}

func TestDepositSameSourceReplayAndMismatch(t *testing.T) {
	svc, st, _ := depositFixture(t)
	ctx := context.Background()
	res, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 7, Currency: "USD", Amount: "5000",
		Reference: "BNK-TX-2", Source: "STATEMENT", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Same source, same payload hash → idempotent replay.
	stored := st.depConf[res.DepositID][0]
	out, err := svc.Confirm(ctx, DepositConfirmRequest{
		DepositID: res.DepositID, Source: "STATEMENT",
		PayloadSHA: strVal(stored.PayloadSHA256), ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("same-source replay: %v", err)
	}
	if !out.Replayed || out.Confirmations != 1 {
		t.Fatalf("expected replay with 1 confirmation, got %+v", out)
	}
	// Same source, different payload → IDEMPOTENCY_KEY_MISMATCH.
	_, err = svc.Confirm(ctx, DepositConfirmRequest{
		DepositID: res.DepositID, Source: "STATEMENT",
		PayloadSHA: "deadbeef", ReceivedBy: 99,
	})
	requireErrCode(t, err, "IDEMPOTENCY_KEY_MISMATCH")
}

func TestDepositStandardTierSanctionsGate(t *testing.T) {
	svc, _, _ := depositFixture(t)
	ctx := context.Background()
	// $20K → STANDARD tier; no sanctions screener wired → fail closed
	// into PENDING_REVIEW (SANCTIONS_UNAVAILABLE flag).
	res, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 7, Currency: "USD", Amount: "20000",
		Reference: "BNK-TX-3", Source: "CAMT054", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	out, err := svc.Confirm(ctx, DepositConfirmRequest{
		DepositID: res.DepositID, Source: "STATEMENT",
		SenderName: "Alice Example", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingPendingReview {
		t.Fatalf("standard tier without screener must review, got %s", out.Status)
	}
	found := false
	for _, f := range out.Flags {
		if f == "SANCTIONS_UNAVAILABLE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected SANCTIONS_UNAVAILABLE flag, got %v", out.Flags)
	}
}

type passScreener struct{}

func (passScreener) ScreenDeposit(context.Context, int64, string, string) (bool, error) {
	return false, nil
}

func TestDepositStandardTierCreditsWithScreener(t *testing.T) {
	svc, _, poster := depositFixture(t)
	svc.WithSanctions(passScreener{})
	ctx := context.Background()
	res, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 7, Currency: "USD", Amount: "20000",
		Reference: "BNK-TX-4", Source: "MT103", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	out, err := svc.Confirm(ctx, DepositConfirmRequest{
		DepositID: res.DepositID, Source: "STATEMENT",
		SenderName: "Alice Example", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingCompleted {
		t.Fatalf("standard tier with clean checks must credit, got %s (%v)", out.Status, out.Flags)
	}
	if len(poster.journals) != 2 {
		t.Fatalf("hold + credit journals expected, got %d", len(poster.journals))
	}
}

func TestDepositPendingReviewTierAndAdmin(t *testing.T) {
	svc, st, _ := depositFixture(t)
	ctx := context.Background()
	res, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 7, Currency: "USD", Amount: "60000",
		Reference: "BNK-TX-5", Source: "STATEMENT", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	out, err := svc.Confirm(ctx, DepositConfirmRequest{
		DepositID: res.DepositID, Source: "WEBHOOK",
		SenderName: "Alice Example", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if out.Status != FundingPendingReview {
		t.Fatalf(">$50K must land PENDING_REVIEW, got %s", out.Status)
	}
	if out.ReviewDeadline == nil {
		t.Fatalf("4h review deadline must be stamped")
	}
	if len(st.opsAlerts) == 0 {
		t.Fatalf("PENDING_REVIEW must raise a durable ops alert")
	}
	// Four-eyes reject path.
	_, err = svc.AdminReview(ctx, 5, 5, res.DepositID, false, "")
	requireErrCode(t, err, "DUAL_CONTROL_VIOLATION")
	rv, err := svc.AdminReview(ctx, 5, 9, res.DepositID, false, "SOF unresolved")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rv.Status != FundingFailed {
		t.Fatalf("expected FAILED, got %s", rv.Status)
	}
}

func TestDepositIntentAdoptedByDetection(t *testing.T) {
	svc, _, _ := depositFixture(t)
	ctx := context.Background()
	intent, err := svc.CreateIntent(ctx, CreateDepositIntentRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "5000",
		Reference: "CLIENT-REF-9", IdempotencyKey: "k9",
	})
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	res, err := svc.IngestDetected(ctx, IngestDepositRequest{
		AccountID: 7, Currency: "USD", Amount: "5000",
		Reference: "BNK-TX-9", Source: "STATEMENT",
		IntentReference: "CLIENT-REF-9", ReceivedBy: 99,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.DepositID != intent.DepositID {
		t.Fatalf("detection must adopt intent %d, created %d",
			intent.DepositID, res.DepositID)
	}
}

// ---------------------------------------------------------------------------
// Task 11.3.6 — nostro-aware dispatch
// ---------------------------------------------------------------------------

func dispatchFixture(t *testing.T) (*DispatchService, *flowStore, *fakePoster) {
	t.Helper()
	st := newFlowStore()
	poster := &fakePoster{}
	svc, err := NewDispatchService(st, poster)
	if err != nil {
		t.Fatalf("dispatch service: %v", err)
	}
	return svc, st, poster
}

func seedWithdrawal(st *flowStore, id int64, amount string, status string) {
	st.withdrawals[id] = &WithdrawalRow{
		ID: id, AccountID: 7, Currency: "USD",
		Amount: dec(amount), Status: status,
		ReferenceAccount: strPtrOrNil("IBAN9"),
	}
	if id > st.nextID {
		st.nextID = id
	}
}

func TestDispatchSufficientNostro(t *testing.T) {
	svc, st, poster := dispatchFixture(t)
	seedWithdrawal(st, 101, "5000", FundingConfirmed)
	st.nostro[1] = &NostroBalanceRow{ID: 1, Currency: "USD",
		BankName: "Ops Bank", Balance: dec("10000"), Status: "ACTIVE"}
	res, err := svc.Release(context.Background(), 101)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Disposition != "DISPATCHED" {
		t.Fatalf("expected DISPATCHED, got %+v", res)
	}
	if st.withdrawals[101].Status != FundingCompleted {
		t.Fatalf("withdrawal must land COMPLETED, got %s", st.withdrawals[101].Status)
	}
	if len(poster.journals) != 1 {
		t.Fatalf("dispatch must post the consuming journal")
	}
	j := poster.journals[0]
	if j.IdempotencyKey != "withdrawal-dispatch:101" {
		t.Fatalf("dispatch journal key must be idempotent, got %s", j.IdempotencyKey)
	}
}

func TestDispatchInsufficientNostroQueues(t *testing.T) {
	svc, st, _ := dispatchFixture(t)
	seedWithdrawal(st, 102, "5000", FundingConfirmed)
	st.nostro[1] = &NostroBalanceRow{ID: 1, Currency: "USD",
		Balance: dec("1000"), Status: "ACTIVE"}
	st.nostro[2] = &NostroBalanceRow{ID: 2, Currency: "USD",
		Balance: dec("2000"), Status: "ACTIVE"}
	res, err := svc.Release(context.Background(), 102)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Disposition != "QUEUED" || res.Reason != QueueReasonNostroInsufficient {
		t.Fatalf("expected QUEUED/NOSTRO_INSUFFICIENT, got %+v", res)
	}
	if res.Shortfall == "" {
		t.Fatalf("shortfall must be reported")
	}
	if st.withdrawals[102].Status != FundingConfirmed {
		t.Fatalf("queued withdrawal stays CONFIRMED, got %s", st.withdrawals[102].Status)
	}
	q := st.dispatchQ[102]
	if q == nil || q.Status != QueueQueued {
		t.Fatalf("durable queue row missing")
	}
	if len(st.opsAlerts) == 0 || st.opsAlerts[0].Code != CodeNostroInsufficientFunds {
		t.Fatalf("NOSTRO_INSUFFICIENT_FUNDS alert row missing")
	}
	// ≥2 ACTIVE nostro accounts → auto-replenishment request opened.
	if len(st.replenishments) != 1 {
		t.Fatalf("auto-replenishment request must be opened, got %d", len(st.replenishments))
	}
	var r *ReplenishmentRow
	for _, rr := range st.replenishments {
		r = rr
	}
	if r.SourceNostroID != 2 || r.TargetNostroID != 1 {
		t.Fatalf("replenishment must flow reserve(2)→operating(1), got %d→%d",
			r.SourceNostroID, r.TargetNostroID)
	}
}

func TestDispatchHeldUntilLapses(t *testing.T) {
	svc, st, _ := dispatchFixture(t)
	seedWithdrawal(st, 103, "5000", FundingConfirmed)
	st.holdUntil[103] = timePtr(time.Now().UTC().Add(10 * time.Hour))
	st.nostro[1] = &NostroBalanceRow{ID: 1, Currency: "USD",
		Balance: dec("10000"), Status: "ACTIVE"}
	res, err := svc.Release(context.Background(), 103)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Disposition != "HELD" || res.Reason != QueueReasonDestinationHold {
		t.Fatalf("expected HELD, got %+v", res)
	}
}

func TestDispatchNonConfirmedSkips(t *testing.T) {
	svc, st, _ := dispatchFixture(t)
	seedWithdrawal(st, 104, "5000", FundingPending)
	res, err := svc.Release(context.Background(), 104)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Disposition != "SKIPPED" {
		t.Fatalf("expected SKIPPED, got %+v", res)
	}
}

func TestDispatchSweepRetries(t *testing.T) {
	svc, st, _ := dispatchFixture(t)
	seedWithdrawal(st, 105, "4000", FundingConfirmed)
	st.nostro[1] = &NostroBalanceRow{ID: 1, Currency: "USD",
		Balance: dec("100"), Status: "ACTIVE"}
	// First pass queues on insufficient nostro.
	n, err := svc.SweepDue(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	if st.dispatchQ[105] == nil {
		t.Fatalf("first sweep must queue the withdrawal")
	}
	// Replenish → next sweep dispatches.
	st.nostro[1].Balance = dec("9000")
	n, err = svc.SweepDue(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("sweep2: n=%d err=%v", n, err)
	}
	if st.withdrawals[105].Status != FundingCompleted {
		t.Fatalf("replenished withdrawal must dispatch, got %s",
			st.withdrawals[105].Status)
	}
	if st.dispatchQ[105].Status != QueueDispatched {
		t.Fatalf("queue row must close DISPATCHED, got %s", st.dispatchQ[105].Status)
	}
}

func TestReplenishmentDualControl(t *testing.T) {
	svc, st, _ := dispatchFixture(t)
	ctx := context.Background()
	st.nostro[1] = &NostroBalanceRow{ID: 1, Currency: "USD",
		Balance: dec("1000"), Status: "ACTIVE"}
	st.nostro[2] = &NostroBalanceRow{ID: 2, Currency: "USD",
		Balance: dec("9000"), Status: "ACTIVE"}

	r, err := svc.RequestReplenishment(ctx, 5, "USD", dec("500"), 2, 1)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if r.Status != ReplenPending {
		t.Fatalf("expected PENDING_APPROVAL, got %s", r.Status)
	}
	// Same-principal approval → DUAL_CONTROL_VIOLATION.
	_, err = svc.DecideReplenishment(ctx, 5, r.ID, true, "")
	requireErrCode(t, err, "DUAL_CONTROL_VIOLATION")
	// Distinct approver executes the movement.
	out, err := svc.DecideReplenishment(ctx, 9, r.ID, true, "go")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if out.Status != ReplenExecuted {
		t.Fatalf("expected EXECUTED, got %s", out.Status)
	}
	if !st.nostro[1].Balance.Equal(dec("1500")) ||
		!st.nostro[2].Balance.Equal(dec("8500")) {
		t.Fatalf("two-sided movement wrong: %s / %s",
			st.nostro[1].Balance, st.nostro[2].Balance)
	}
	// A decided request is immutable.
	_, err = svc.DecideReplenishment(ctx, 9, r.ID, false, "")
	requireErrCode(t, err, "INVALID_LIFECYCLE_TRANSITION")
}

func TestReplenishmentOverdraftRefuses(t *testing.T) {
	svc, st, _ := dispatchFixture(t)
	ctx := context.Background()
	st.nostro[1] = &NostroBalanceRow{ID: 1, Currency: "USD",
		Balance: dec("100"), Status: "ACTIVE"}
	st.nostro[2] = &NostroBalanceRow{ID: 2, Currency: "USD",
		Balance: dec("200"), Status: "ACTIVE"}
	r, err := svc.RequestReplenishment(ctx, 5, "USD", dec("500"), 1, 2)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, err = svc.DecideReplenishment(ctx, 9, r.ID, true, "")
	requireErrCode(t, err, "INSUFFICIENT_BALANCE")
	if st.replenishments[r.ID].Status != ReplenPending {
		t.Fatalf("failed execution must not mark the request executed")
	}
}

func TestNostroCoverageDeficit(t *testing.T) {
	svc, st, _ := dispatchFixture(t)
	seedWithdrawal(st, 106, "8000", FundingConfirmed)
	st.nostro[1] = &NostroBalanceRow{ID: 1, Currency: "USD",
		Balance: dec("5000"), Status: "ACTIVE"}
	rows, err := svc.Coverage(context.Background())
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if len(rows) != 1 || rows[0].Currency != "USD" {
		t.Fatalf("expected USD row, got %+v", rows)
	}
	if !rows[0].Deficit || rows[0].Queued != 1 {
		t.Fatalf("expected deficit + 1 queued, got %+v", rows[0])
	}
}

// ---------------------------------------------------------------------------
// Review SLA sweep (4h ops deadline)
// ---------------------------------------------------------------------------

func TestSweepReviewSLAEscalates(t *testing.T) {
	svc, st, _ := depositFixture(t)
	st.fundingTx[50] = &FundingTxRow{
		ID: 50, AccountID: 7, Currency: "USD", Type: "DEPOSIT",
		Amount: dec("60000"), Status: FundingPendingReview,
		ReviewDeadline: timePtr(time.Now().UTC().Add(-time.Hour)),
	}
	n, err := svc.SweepReviewSLA(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	if len(st.opsAlerts) != 1 || st.opsAlerts[0].Code != "REVIEW_SLA_BREACH" {
		t.Fatalf("breach alert missing: %+v", st.opsAlerts)
	}
	if st.fundingTx[50].ReviewDeadline == nil ||
		!st.fundingTx[50].ReviewDeadline.After(time.Now().UTC()) {
		t.Fatalf("deadline must roll one window forward")
	}
	// Second pass inside the new window: nothing breaches.
	n, err = svc.SweepReviewSLA(context.Background(), 10)
	if err != nil || n != 0 {
		t.Fatalf("re-sweep must not double-alert: n=%d err=%v", n, err)
	}
}

// ---------------------------------------------------------------------------
// Idempotent withdrawal create (flow-level, account-scoped §8.8)
// ---------------------------------------------------------------------------

func TestFlowCreateIdempotentReplay(t *testing.T) {
	flow, st, _, _ := flowFixture(t)
	st.meta[7] = &AccountMeta{ID: 7, KYCTier: "T2"}
	unlocked := time.Now().UTC().Add(-time.Hour)
	st.beneficiaries[dkey(7, "IBAN9")] = &BeneficiaryRow{
		BankAccountID: 11, AccountID: 7, Status: BeneficiaryVerified,
		UnlockedAt: &unlocked,
	}
	req := CreateWithdrawalRequest{
		AccountID: 7, UserID: 1, Currency: "USD", Amount: "100",
		ReferenceAccount: "IBAN9", IdempotencyKey: "same-key",
	}
	r1, err := flow.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r2, err := flow.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !r2.Replayed || r2.WithdrawalID != r1.WithdrawalID {
		t.Fatalf("expected replay, got %+v", r2)
	}
	// Different payload on the same key → mismatch.
	req.Amount = "200"
	_, err = flow.Create(context.Background(), req)
	requireErrCode(t, err, "IDEMPOTENCY_KEY_MISMATCH")
}

var _ = stderrors.Is // keep the import if unused paths change
