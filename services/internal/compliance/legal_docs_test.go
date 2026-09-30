package compliance

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

// ---- fixture store -----------------------------------------------------

type fakeLegalStore struct {
	agmts []LegalAgreement
	next  int64
	umr   bool
	err   error // store-level failure injection
}

func newFakeLegalStore() *fakeLegalStore { return &fakeLegalStore{next: 1} }

func (s *fakeLegalStore) Insert(_ context.Context, in LegalAgreementInput, actorID int64, now time.Time) (*LegalAgreement, error) {
	if s.err != nil {
		return nil, s.err
	}
	for _, a := range s.agmts {
		if a.AccountID == in.AccountID && a.AgreementType == in.AgreementType &&
			a.Counterparty == in.Counterparty && legalAgreementOpen[a.Status] {
			return nil, excerrors.New("DERIVATIVE_STATE_CONFLICT", "open agreement exists")
		}
	}
	a := LegalAgreement{
		ID: s.next, AccountID: in.AccountID, AgreementType: in.AgreementType,
		Counterparty: in.Counterparty, Status: AgreementStatusPending,
		DocumentURL: in.DocumentURL, ExpiresAt: in.ExpiresAt,
		ReviewedBy: &actorID, CreatedAt: now, UpdatedAt: now,
	}
	s.next++
	s.agmts = append(s.agmts, a)
	cp := a
	return &cp, nil
}

func (s *fakeLegalStore) Transition(_ context.Context, id int64, from map[string]bool,
	to string, actorID int64, docURL string, now time.Time) (*LegalAgreement, error) {
	if s.err != nil {
		return nil, s.err
	}
	for i := range s.agmts {
		if s.agmts[i].ID != id {
			continue
		}
		if !from[s.agmts[i].Status] {
			return nil, excerrors.New("DERIVATIVE_STATE_CONFLICT", "invalid transition")
		}
		s.agmts[i].Status = to
		s.agmts[i].ReviewedBy = &actorID
		s.agmts[i].UpdatedAt = now
		if to == AgreementStatusExecuted {
			s.agmts[i].ExecutedAt = &now
			s.agmts[i].DocumentURL = docURL
		}
		cp := s.agmts[i]
		return &cp, nil
	}
	return nil, excerrors.New("DERIVATIVE_STATE_CONFLICT", "agreement not found")
}

func (s *fakeLegalStore) ExpireDue(_ context.Context, now time.Time) (int64, error) {
	var n int64
	for i := range s.agmts {
		if s.agmts[i].Status == AgreementStatusExecuted &&
			s.agmts[i].ExpiresAt != nil && !s.agmts[i].ExpiresAt.After(now) {
			s.agmts[i].Status = AgreementStatusExpired
			n++
		}
	}
	return n, s.err
}

func (s *fakeLegalStore) Get(_ context.Context, id int64) (*LegalAgreement, error) {
	for i := range s.agmts {
		if s.agmts[i].ID == id {
			cp := s.agmts[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *fakeLegalStore) List(_ context.Context, accountID int64) ([]LegalAgreement, error) {
	var out []LegalAgreement
	for _, a := range s.agmts {
		if a.AccountID == accountID {
			out = append(out, a)
		}
	}
	return out, s.err
}

func (s *fakeLegalStore) AgreementsOfType(_ context.Context, accountID int64, agreementType string) ([]LegalAgreement, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []LegalAgreement
	for _, a := range s.agmts {
		if a.AccountID == accountID && a.AgreementType == agreementType {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeLegalStore) UMRInScope(_ context.Context, accountID int64) (bool, error) {
	return s.umr, s.err
}

func legalOfficerRole(context.Context, int64) (string, error) { return "Compliance Officer", nil }
func legalTraderRole(context.Context, int64) (string, error)  { return "Trader", nil }

func newLegalSvc(t *testing.T, store *fakeLegalStore, resolver RoleResolver) *LegalDocService {
	t.Helper()
	svc, err := NewLegalDocService(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// ---- tests ---------------------------------------------------------------

func TestLegalDocs_RegisterExecuteLifecycle(t *testing.T) {
	store := newFakeLegalStore()
	svc := newLegalSvc(t, store, legalOfficerRole)
	ctx := context.Background()

	a, err := svc.Register(ctx, 9001, LegalAgreementInput{
		AccountID: 7, AgreementType: AgreementISDA, DocumentURL: "s3://docs/isda-7.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != AgreementStatusPending {
		t.Fatalf("status %s", a.Status)
	}
	ex, err := svc.Execute(ctx, 9001, a.ID, "s3://docs/isda-7-signed.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Status != AgreementStatusExecuted || ex.ExecutedAt == nil || ex.ReviewedBy == nil {
		t.Fatalf("execute: %+v", ex)
	}
	// Duplicate open registration rejected.
	if _, err := svc.Register(ctx, 9001, LegalAgreementInput{
		AccountID: 7, AgreementType: AgreementISDA}); err == nil {
		t.Fatal("duplicate open agreement accepted")
	}
	// Terminate from EXECUTED.
	term, err := svc.Terminate(ctx, 9001, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if term.Status != AgreementStatusTerminated {
		t.Fatalf("status %s", term.Status)
	}
	// Terminate again → conflict.
	if _, err := svc.Terminate(ctx, 9001, a.ID); excerrors.CodeOf(err) != "DERIVATIVE_STATE_CONFLICT" {
		t.Fatalf("re-terminate: %v", err)
	}
}

func TestLegalDocs_UnknownTypeAndRoleGuards(t *testing.T) {
	store := newFakeLegalStore()
	svc := newLegalSvc(t, store, legalTraderRole)
	ctx := context.Background()
	if _, err := svc.Register(ctx, 1, LegalAgreementInput{
		AccountID: 7, AgreementType: AgreementISDA}); excerrors.CodeOf(err) != "UNAUTHORIZED_ROLE" {
		t.Fatalf("trader allowed to register: %v", err)
	}
	svc = newLegalSvc(t, store, legalOfficerRole)
	if _, err := svc.Register(ctx, 1, LegalAgreementInput{
		AccountID: 7, AgreementType: "GMRA2"}); excerrors.CodeOf(err) != "INVALID_REQUEST" {
		t.Fatalf("bad type accepted: %v", err)
	}
}

func TestLegalDocs_ExpirySweep(t *testing.T) {
	store := newFakeLegalStore()
	svc := newLegalSvc(t, store, legalOfficerRole)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	a, err := svc.Register(ctx, 1, LegalAgreementInput{
		AccountID: 7, AgreementType: AgreementCSA, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(ctx, 1, a.ID, "s3://x"); err != nil {
		t.Fatal(err)
	}
	n, err := svc.ExpireDue(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expire sweep n=%d err=%v", n, err)
	}
	got, _ := svc.Get(ctx, a.ID)
	if got.Status != AgreementStatusExpired {
		t.Fatalf("status %s", got.Status)
	}
}

func TestLegalGate_NDFRequiresISDAAndCSA(t *testing.T) {
	store := newFakeLegalStore()
	svc := newLegalSvc(t, store, legalOfficerRole)
	ctx := context.Background()
	exec := func(agType string) {
		a, err := svc.Register(ctx, 1, LegalAgreementInput{AccountID: 7, AgreementType: agType})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Execute(ctx, 1, a.ID, "s3://doc"); err != nil {
			t.Fatal(err)
		}
	}

	// No docs → reject.
	err := svc.AdmitOrder(ctx, 7, ClassNDF, string(CategoryProfessional), false)
	if excerrors.CodeOf(err) != CodeLegalDocRequired {
		t.Fatalf("expected LEGAL_DOC_REQUIRED, got %v", err)
	}
	// ISDA only → still reject (CSA required for NDF).
	exec(AgreementISDA)
	if err := svc.AdmitOrder(ctx, 7, ClassNDF, string(CategoryProfessional), false); excerrors.CodeOf(err) != CodeLegalDocRequired {
		t.Fatalf("CSA missing should reject, got %v", err)
	}
	exec(AgreementCSA)
	if err := svc.AdmitOrder(ctx, 7, ClassNDF, string(CategoryProfessional), false); err != nil {
		t.Fatalf("NDF with ISDA+CSA rejected: %v", err)
	}
	// OPTION too.
	if err := svc.AdmitOrder(ctx, 7, ClassOption, string(CategoryProfessional), false); err != nil {
		t.Fatalf("OPTION rejected: %v", err)
	}
	// SPOT never gated.
	if err := svc.AdmitOrder(ctx, 7, ClassSpot, string(CategoryRetail), false); err != nil {
		t.Fatalf("SPOT rejected: %v", err)
	}
	// FORWARD needs only ISDA.
	if err := svc.AdmitOrder(ctx, 7, ClassForward, string(CategoryProfessional), false); err != nil {
		t.Fatalf("FORWARD rejected: %v", err)
	}
	// ECP adds the FMSB give-up requirement.
	err = svc.AdmitOrder(ctx, 7, ClassOption, string(CategoryECP), false)
	if excerrors.CodeOf(err) != CodeLegalDocRequired {
		t.Fatalf("ECP without FMSB should reject, got %v", err)
	}
	exec(AgreementFMSBGiveup)
	if err := svc.AdmitOrder(ctx, 7, ClassOption, string(CategoryECP), false); err != nil {
		t.Fatalf("ECP with FMSB rejected: %v", err)
	}
	// reduceOnly bypasses (terminated docs never strand closes).
	if err := svc.AdmitOrder(ctx, 7, ClassNDF, string(CategoryProfessional), true); err != nil {
		t.Fatalf("reduce-only rejected: %v", err)
	}
}

func TestLegalGate_PendingAndExpiredReject(t *testing.T) {
	store := newFakeLegalStore()
	svc := newLegalSvc(t, store, legalOfficerRole)
	ctx := context.Background()
	// PENDING agreement does not satisfy the gate.
	if _, err := svc.Register(ctx, 1, LegalAgreementInput{AccountID: 7, AgreementType: AgreementISDA}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdmitOrder(ctx, 7, ClassForward, "PROFESSIONAL", false); excerrors.CodeOf(err) != CodeLegalDocRequired {
		t.Fatalf("PENDING satisfied the gate: %v", err)
	}
	// Expired agreement doesn't either.
	store2 := newFakeLegalStore()
	svc2 := newLegalSvc(t, store2, legalOfficerRole)
	past := time.Now().Add(-time.Hour)
	a, _ := svc2.Register(ctx, 1, LegalAgreementInput{
		AccountID: 7, AgreementType: AgreementISDA, ExpiresAt: &past})
	if _, err := svc2.Execute(ctx, 1, a.ID, "s3://x"); err != nil {
		t.Fatal(err)
	}
	if err := svc2.AdmitOrder(ctx, 7, ClassForward, "PROFESSIONAL", false); excerrors.CodeOf(err) != CodeLegalDocRequired {
		t.Fatalf("expired agreement satisfied the gate: %v", err)
	}
}

func TestLegalGate_StoreErrorFailsClosed(t *testing.T) {
	store := newFakeLegalStore()
	store.err = stderrors.New("db down")
	svc := newLegalSvc(t, store, legalOfficerRole)
	err := svc.AdmitOrder(context.Background(), 7, ClassNDF, "PROFESSIONAL", false)
	if excerrors.CodeOf(err) != "SERVICE_DEGRADED" {
		t.Fatalf("expected SERVICE_DEGRADED, got %v", err)
	}
}
