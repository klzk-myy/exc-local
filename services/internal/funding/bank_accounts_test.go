// Unit tests for the Task 11.3.7 beneficiary registry: registration
// KYC gate, dual-controlled admin verification, the withdrawal
// allowlist gate, and the 24h new-beneficiary hold. Pure unit level —
// the stub pgx.Tx drives the tx-shaped store calls; live-DB coverage
// sits in the DSN-gated integration test.
package funding

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// In-memory BankAccountStore
// ---------------------------------------------------------------------------

type benStore struct {
	tx      *stubTx
	meta    map[int64]*AccountMeta
	rows    map[int64]*BankAccount
	nextID  int64
	audits  []string
	byOwner map[int64][]int64
}

func newBenStore() *benStore {
	return &benStore{tx: &stubTx{}, meta: map[int64]*AccountMeta{},
		rows: map[int64]*BankAccount{}, byOwner: map[int64][]int64{}}
}

func (s *benStore) BeginTx(context.Context) (pgx.Tx, error) { return s.tx, nil }

func (s *benStore) AccountMeta(_ context.Context, id int64) (*AccountMeta, error) {
	if m, ok := s.meta[id]; ok {
		return m, nil
	}
	return nil, errf("NOT_FOUND", "account %d not found", id)
}

func (s *benStore) InsertBankAccount(_ context.Context, _ pgx.Tx, b *BankAccount) (*BankAccount, error) {
	s.nextID++
	cp := *b
	cp.BankAccountID = s.nextID
	cp.CreatedAt = time.Now().UTC()
	cp.UpdatedAt = cp.CreatedAt
	s.rows[cp.BankAccountID] = &cp
	s.byOwner[cp.AccountID] = append(s.byOwner[cp.AccountID], cp.BankAccountID)
	return &cp, nil
}

func (s *benStore) BankAccount(_ context.Context, id int64) (*BankAccount, error) {
	return s.rows[id], nil
}

func (s *benStore) BankAccountForUpdate(_ context.Context, _ pgx.Tx, id int64) (*BankAccount, error) {
	return s.rows[id], nil
}

func (s *benStore) BankAccountsForAccount(_ context.Context, accountID int64) ([]BankAccount, error) {
	out := []BankAccount{}
	for _, id := range s.byOwner[accountID] {
		out = append(out, *s.rows[id])
	}
	return out, nil
}

func (s *benStore) ListBankAccounts(_ context.Context, status string, _ int) ([]BankAccount, error) {
	out := []BankAccount{}
	for _, r := range s.rows {
		if status == "" || r.Status == status {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *benStore) DeleteBankAccount(_ context.Context, accountID, id int64) (bool, error) {
	r, ok := s.rows[id]
	if !ok || r.AccountID != accountID {
		return false, nil
	}
	delete(s.rows, id)
	ids := s.byOwner[accountID]
	for i, x := range ids {
		if x == id {
			s.byOwner[accountID] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	return true, nil
}

func (s *benStore) FindBeneficiary(_ context.Context, accountID int64, reference string) (*BankAccount, error) {
	ref := normalizeRef(reference)
	for _, id := range s.byOwner[accountID] {
		r := s.rows[id]
		if r.IBAN != nil && normalizeRef(*r.IBAN) == ref {
			return r, nil
		}
		if r.AccountNumber != nil && normalizeRef(*r.AccountNumber) == ref {
			return r, nil
		}
	}
	return nil, nil
}

func (s *benStore) SetBankAccountStatus(_ context.Context, _ pgx.Tx, id int64,
	status string, verifiedBy *int64, method *string, reason *string,
	verifiedAt, unlockedAt *time.Time) error {
	r := s.rows[id]
	if r == nil {
		return errf("NOT_FOUND", "beneficiary %d not found", id)
	}
	r.Status = status
	r.VerifiedBy = verifiedBy
	r.VerificationMethod = method
	r.RejectionReason = reason
	r.VerifiedAt = verifiedAt
	r.UnlockedAt = unlockedAt
	return nil
}

func (s *benStore) AdminAuditTx(_ context.Context, _ pgx.Tx, _ int64,
	action, _ string, _ int64, _, _ []byte, _ string) error {
	s.audits = append(s.audits, action)
	return nil
}

// roleOf returns a resolver pinned to the given role per admin id.
func roleOf(roles map[int64]string) BeneficiaryRoleResolver {
	return func(_ context.Context, id int64) (string, error) {
		if r, ok := roles[id]; ok {
			return r, nil
		}
		return "", fmt.Errorf("no role binding for %d", id)
	}
}

func benSvc(t *testing.T, st *benStore, roles BeneficiaryRoleResolver) *BankAccountService {
	t.Helper()
	svc, err := NewBankAccountService(st, roles)
	if err != nil {
		t.Fatalf("NewBankAccountService: %v", err)
	}
	return svc
}

func benInput(iban string) BankAccountInput {
	return BankAccountInput{
		Currency: "USD", IBAN: &iban, BankName: "Test Bank",
		BeneficiaryName: "Jane Q Trader", Rail: "SEPA",
	}
}

// ---------------------------------------------------------------------------

func TestBeneficiaryRegister_KYCGate(t *testing.T) {
	ctx := context.Background()
	st := newBenStore()
	svc := benSvc(t, st, roleOf(map[int64]string{}))

	st.meta[1] = &AccountMeta{ID: 1, Status: "ACTIVE", KYCTier: "T0"}
	if _, err := svc.Register(ctx, 1, benInput("DE89370400440532013000")); codeOf(err) != "KYC_REQUIRED" {
		t.Fatalf("T0 registration must reject KYC_REQUIRED, got %v", err)
	}

	st.meta[2] = &AccountMeta{ID: 2, Status: "SUSPENDED", KYCTier: "T2"}
	if _, err := svc.Register(ctx, 2, benInput("DE89370400440532013000")); codeOf(err) != "FORBIDDEN" {
		t.Fatalf("suspended account must reject FORBIDDEN, got %v", err)
	}

	st.meta[3] = &AccountMeta{ID: 3, Status: "ACTIVE", KYCTier: "T1"}
	b, err := svc.Register(ctx, 3, benInput("de89 3704 0044 0532 0130 00"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if b.Status != BankAcctPending {
		t.Fatalf("new beneficiary must be PENDING_VERIFICATION, got %s", b.Status)
	}
	if *b.IBAN != "DE89370400440532013000" {
		t.Fatalf("iban must normalize uppercase/unspaced, got %q", *b.IBAN)
	}

	// Validation: no destination at all.
	if _, err := svc.Register(ctx, 3, BankAccountInput{
		Currency: "USD", BankName: "X", BeneficiaryName: "Y", Rail: "WIRE",
	}); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("missing destination must reject, got %v", err)
	}
}

func TestBeneficiaryVerify_DualControl(t *testing.T) {
	ctx := context.Background()
	st := newBenStore()
	st.meta[1] = &AccountMeta{ID: 1, Status: "ACTIVE", KYCTier: "T1"}
	roles := roleOf(map[int64]string{
		10: "Finance Ops", 11: "Finance Ops", 12: "Support Agent"})
	svc := benSvc(t, st, roles)

	b, err := svc.Register(ctx, 1, benInput("DE89370400440532013000"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Missing approver → DUAL_CONTROL_REQUIRED.
	if _, err := svc.Verify(ctx, 10, 0, b.BankAccountID, "", ""); codeOf(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("no approver: %v", err)
	}
	// Same admin → DUAL_CONTROL_REQUIRED.
	if _, err := svc.Verify(ctx, 10, 10, b.BankAccountID, "", ""); codeOf(err) != "DUAL_CONTROL_REQUIRED" {
		t.Fatalf("self-approve: %v", err)
	}
	// Ineligible approver → UNAUTHORIZED_ROLE.
	if _, err := svc.Verify(ctx, 10, 12, b.BankAccountID, "", ""); codeOf(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("ineligible approver: %v", err)
	}
	// Ineligible initiator → UNAUTHORIZED_ROLE.
	if _, err := svc.Verify(ctx, 12, 10, b.BankAccountID, "", ""); codeOf(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("ineligible admin: %v", err)
	}

	// Happy path: verified, audited, 24h hold stamped.
	out, err := svc.Verify(ctx, 10, 11, b.BankAccountID, VerifyMethodBankStatement, "10.0.0.1")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if out.Status != BankAcctVerified || out.VerifiedBy == nil || *out.VerifiedBy != 10 {
		t.Fatalf("verified row: %+v", out)
	}
	if out.UnlockedAt == nil || !out.UnlockedAt.Equal(out.VerifiedAt.Add(BeneficiaryHoldWindow)) {
		t.Fatalf("24h hold must be stamped: %+v", out.UnlockedAt)
	}
	if len(st.audits) != 1 || st.audits[0] != "funding.beneficiary.verify" {
		t.Fatalf("verify must audit in-tx: %v", st.audits)
	}

	// Re-verify of a non-pending row rejects.
	if _, err := svc.Verify(ctx, 10, 11, b.BankAccountID, "", ""); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("re-verify must reject, got %v", err)
	}
}

func TestBeneficiaryAssertWithdrawable(t *testing.T) {
	ctx := context.Background()
	st := newBenStore()
	st.meta[1] = &AccountMeta{ID: 1, Status: "ACTIVE", KYCTier: "T1"}
	roles := roleOf(map[int64]string{10: "Finance Ops", 11: "Finance Ops"})
	svc := benSvc(t, st, roles)

	// Unregistered destination → BANK_ACCOUNT_NOT_VERIFIED.
	if err := svc.AssertWithdrawable(ctx, 1, "DE89370400440532013000"); codeOf(err) != "BANK_ACCOUNT_NOT_VERIFIED" {
		t.Fatalf("unregistered: %v", err)
	}

	b, _ := svc.Register(ctx, 1, benInput("DE89370400440532013000"))
	// PENDING destination → BANK_ACCOUNT_NOT_VERIFIED.
	if err := svc.AssertWithdrawable(ctx, 1, "DE89370400440532013000"); codeOf(err) != "BANK_ACCOUNT_NOT_VERIFIED" {
		t.Fatalf("pending: %v", err)
	}

	// Verified but inside the 24h hold → BENEFICIARY_HOLD_ACTIVE.
	now := time.Now().UTC()
	svc.WithClock(func() time.Time { return now })
	if _, err := svc.Verify(ctx, 10, 11, b.BankAccountID, "", ""); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := svc.AssertWithdrawable(ctx, 1, "DE89370400440532013000"); codeOf(err) != "BENEFICIARY_HOLD_ACTIVE" {
		t.Fatalf("inside hold: %v", err)
	}

	// Past the hold → open; case/space-insensitive match.
	svc.WithClock(func() time.Time { return now.Add(25 * time.Hour) })
	if err := svc.AssertWithdrawable(ctx, 1, "de89 3704 0044 0532 0130 00"); err != nil {
		t.Fatalf("verified + hold-lapsed must pass: %v", err)
	}
	// Wrong owner → not verified for THIS account.
	if err := svc.AssertWithdrawable(ctx, 9, "DE89370400440532013000"); codeOf(err) != "BANK_ACCOUNT_NOT_VERIFIED" {
		t.Fatalf("foreign beneficiary must not satisfy account 9: %v", err)
	}
}

func TestBeneficiaryRejectAndDelete(t *testing.T) {
	ctx := context.Background()
	st := newBenStore()
	st.meta[1] = &AccountMeta{ID: 1, Status: "ACTIVE", KYCTier: "T1"}
	svc := benSvc(t, st, roleOf(map[int64]string{10: "Finance Ops"}))

	b, _ := svc.Register(ctx, 1, benInput("DE89370400440532013000"))

	if _, err := svc.Reject(ctx, 10, b.BankAccountID, "", ""); codeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("reject needs a reason: %v", err)
	}
	out, err := svc.Reject(ctx, 10, b.BankAccountID, "docs unreadable", "")
	if err != nil || out.Status != BankAcctRejected {
		t.Fatalf("reject: %v %+v", err, out)
	}

	// Delete is owner-scoped.
	if err := svc.DeleteMine(ctx, 9, b.BankAccountID); codeOf(err) != "NOT_FOUND" {
		t.Fatalf("foreign delete: %v", err)
	}
	if err := svc.DeleteMine(ctx, 1, b.BankAccountID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
}

func TestBankAccountService_NilResolverFailsClosed(t *testing.T) {
	if _, err := NewBankAccountService(newBenStore(), nil); err == nil {
		t.Fatal("nil role resolver must reject construction")
	}
}
