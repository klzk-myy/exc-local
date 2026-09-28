package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	excerrors "exchange/pkg/errors"
)

// fakeLiqStore implements liquidationStore for service tests.
type fakeLiqStore struct {
	params    recordParams
	recErr    error
	markedIDs []int64
}

func (f *fakeLiqStore) RecordLiquidation(_ context.Context, p recordParams) (int64, int64, []PositionSnapshot, error) {
	f.params = p
	if f.recErr != nil {
		return 0, 0, nil, f.recErr
	}
	return 101, 9001, []PositionSnapshot{
		{PositionID: 5, InstrumentID: 1, Symbol: "EURUSD", Side: "LONG", Quantity: "100000"},
	}, nil
}

func (f *fakeLiqStore) MarkEmitFailed(_ context.Context, id int64) {
	f.markedIDs = append(f.markedIDs, id)
}

type fakeSink struct {
	ev  ManualLiquidationEvent
	err error
}

func (f *fakeSink) EmitManualLiquidation(_ context.Context, ev ManualLiquidationEvent) error {
	f.ev = ev
	return f.err
}

func roleMap(m map[int64]string) AdminRoleResolver {
	return func(_ context.Context, id int64) (string, error) {
		r, ok := m[id]
		if !ok {
			return "", errors.New("no role")
		}
		return r, nil
	}
}

func liqReq(acct, approver string, reason string, override bool) ManualLiquidationRequest {
	return ManualLiquidationRequest{
		AccountID:       json.RawMessage(acct),
		Reason:          reason,
		OverrideAuction: override,
		ApproverID:      json.RawMessage(approver),
	}
}

func TestLiquidationRoleGate(t *testing.T) {
	svc := newManualLiquidationService(&fakeLiqStore{},
		roleMap(map[int64]string{1: "Support Agent", 2: "Risk Manager"}),
		&fakeSink{})
	_, err := svc.Execute(context.Background(),
		AdminActor{UserID: 1}, liqReq("10", "2", "margin breach", false))
	assertCode(t, err, "UNAUTHORIZED_ROLE")

	// Nil resolver fails closed.
	svc = newManualLiquidationService(&fakeLiqStore{}, nil, &fakeSink{})
	_, err = svc.Execute(context.Background(),
		AdminActor{UserID: 1}, liqReq("10", "2", "margin breach", false))
	assertCode(t, err, "UNAUTHORIZED_ROLE")

	// Anonymous actor → UNAUTHORIZED.
	_, err = svc.Execute(context.Background(),
		AdminActor{}, liqReq("10", "2", "margin breach", false))
	assertCode(t, err, "UNAUTHORIZED")
}

func TestLiquidationDualControl(t *testing.T) {
	resolver := roleMap(map[int64]string{1: "Risk Manager", 2: "Risk Manager"})
	svc := newManualLiquidationService(&fakeLiqStore{}, resolver, &fakeSink{})

	// Missing approver.
	_, err := svc.Execute(context.Background(), AdminActor{UserID: 1},
		ManualLiquidationRequest{AccountID: json.RawMessage("10"), Reason: "x"})
	assertCode(t, err, "DUAL_CONTROL_REQUIRED")

	// Self-approval prohibited.
	_, err = svc.Execute(context.Background(), AdminActor{UserID: 1},
		liqReq("10", "1", "margin breach", false))
	assertCode(t, err, "DUAL_CONTROL_REQUIRED")

	// Approver without an eligible role.
	svc = newManualLiquidationService(&fakeLiqStore{},
		roleMap(map[int64]string{1: "Risk Manager", 2: "Read-Only Auditor"}),
		&fakeSink{})
	_, err = svc.Execute(context.Background(), AdminActor{UserID: 1},
		liqReq("10", "2", "margin breach", false))
	assertCode(t, err, "UNAUTHORIZED_ROLE")
}

func TestLiquidationValidation(t *testing.T) {
	resolver := roleMap(map[int64]string{1: "Risk Manager", 2: "Super Admin"})
	svc := newManualLiquidationService(&fakeLiqStore{}, resolver, &fakeSink{})
	actor := AdminActor{UserID: 1}

	_, err := svc.Execute(context.Background(), actor,
		liqReq("", "2", "margin breach", false))
	assertCode(t, err, "INVALID_REQUEST") // missing account_id

	_, err = svc.Execute(context.Background(), actor,
		liqReq("10", "2", "  ", false))
	assertCode(t, err, "INVALID_REQUEST") // blank reason

	// override_auction needs a substantive justification.
	_, err = svc.Execute(context.Background(), actor,
		liqReq("10", "2", "short", true))
	assertCode(t, err, "INVALID_REQUEST")

	// Bad account_id form.
	_, err = svc.Execute(context.Background(), actor,
		liqReq(`"abc"`, "2", "margin breach", false))
	assertCode(t, err, "INVALID_REQUEST")
}

func TestLiquidationHappyPath(t *testing.T) {
	store := &fakeLiqStore{}
	sink := &fakeSink{}
	svc := newManualLiquidationService(store,
		roleMap(map[int64]string{1: "Risk Manager", 2: "Super Admin"}), sink)

	res, err := svc.Execute(context.Background(),
		AdminActor{UserID: 1, ClientIP: "198.51.100.9"},
		ManualLiquidationRequest{
			AccountID:       json.RawMessage("10"),
			InstrumentID:    json.RawMessage(`"7"`),
			Reason:          "counterparty credit event — documented",
			OverrideAuction: true,
			ApproverID:      json.RawMessage(`"2"`),
		})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.LiquidationID != 101 || res.Status != "DISPATCHED" ||
		res.AuditSeq != 9001 || res.Source != "MANUAL" {
		t.Fatalf("result: %+v", res)
	}
	if store.params.AccountID != 10 || store.params.InitiatedBy != 1 ||
		store.params.ApprovedBy != 2 || !store.params.OverrideAuction ||
		store.params.ClientIP != "198.51.100.9" ||
		store.params.InstrumentID == nil || *store.params.InstrumentID != 7 {
		t.Fatalf("record params: %+v", store.params)
	}
	// Event emitted with the positions + dual-control ids.
	if sink.ev.Event != "MANUAL_LIQUIDATION" || sink.ev.LiquidationID != 101 ||
		sink.ev.ApprovedBy != 2 || len(sink.ev.Positions) != 1 ||
		!sink.ev.OverrideAuction {
		t.Fatalf("event: %+v", sink.ev)
	}
	if len(store.markedIDs) != 0 {
		t.Fatal("emit succeeded — no EMIT_FAILED mark expected")
	}
}

func TestLiquidationEmitFailureMarksRecord(t *testing.T) {
	store := &fakeLiqStore{}
	svc := newManualLiquidationService(store,
		roleMap(map[int64]string{1: "Risk Manager", 2: "Risk Manager"}),
		&fakeSink{err: errors.New("jetstream down")})

	_, err := svc.Execute(context.Background(), AdminActor{UserID: 1},
		liqReq("10", "2", "margin breach", false))
	assertCode(t, err, "SERVICE_DEGRADED")
	if len(store.markedIDs) != 1 || store.markedIDs[0] != 101 {
		t.Fatalf("EMIT_FAILED mark: %v", store.markedIDs)
	}
}

func TestLiquidationNilSinkFailsClosed(t *testing.T) {
	svc := newManualLiquidationService(&fakeLiqStore{},
		roleMap(map[int64]string{1: "Risk Manager"}), nil)
	_, err := svc.Execute(context.Background(), AdminActor{UserID: 1},
		liqReq("10", "2", "margin breach", false))
	assertCode(t, err, "SERVICE_DEGRADED")
}

func TestLiquidationStoreErrorPropagates(t *testing.T) {
	svc := newManualLiquidationService(
		&fakeLiqStore{recErr: excerrors.New("NOT_FOUND", "account 10 not found")},
		roleMap(map[int64]string{1: "Risk Manager", 2: "Risk Manager"}),
		&fakeSink{})
	_, err := svc.Execute(context.Background(), AdminActor{UserID: 1},
		liqReq("10", "2", "margin breach", false))
	assertCode(t, err, "NOT_FOUND")
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *excerrors.Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("want code %s, got %v", want, err)
	}
}
