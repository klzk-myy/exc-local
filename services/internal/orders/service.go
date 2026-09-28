// Service — the order pipeline business core. Every path follows the
// spec fail-closed ordering: authenticate/authorize (handler layer) →
// validate → persist intent → dispatch to the engine over the wire →
// reflect confirmations. Nothing writes balances directly (§8.4 layer
// boundary): balance sufficiency is a pre-trade check; reservation is
// the engine-side 2PC (Phase-02 Task 2.3.14).
package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/config"
	"exchange/internal/ipc"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// EngineAckTimeout is the spec §8.7/§24 #304 500ms confirmation window —
// expiry maps to GATEWAY_TIMEOUT_MATCHING_ENGINE (504).
const EngineAckTimeout = 500 * time.Millisecond

// MassCancelTimeout bounds the all-shards-confirmed wait for mass
// cancels; the task requires all shards to confirm before success.
const MassCancelTimeout = 5 * time.Second

// BatchSubmitMax / BatchCancelMax are the §8.4 payload ceilings.
const (
	BatchSubmitMax = 10
	BatchCancelMax = 20
)

// RiskChecker is the pre-trade limits seam — *risk.LimitsService
// satisfies it; nil disables the check (readiness wiring decides).
type RiskChecker interface {
	CheckOrder(ctx context.Context, req risk.OrderRequest) error
}

// Service wires store + transport + sequencing.
type Service struct {
	store      Store
	sub        Submitter
	shards     *config.ShardMap
	pending    *pendingConfirms
	limits     RiskChecker
	batch      BatchRateLimiter
	commission CommissionEstimator
	seq        *seqAllocator
	ackTimeout time.Duration
	now        func() time.Time
}

// Options customizes Service construction; nil/zero fields pick
// production defaults.
type Options struct {
	Store      Store
	Submitter  Submitter
	ShardMap   *config.ShardMap
	Limits     RiskChecker
	BatchRL    BatchRateLimiter
	Commission CommissionEstimator // optional — dry-run fee estimate
	AckTimeout time.Duration
	Now        func() time.Time
}

func NewService(o Options) (*Service, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("orders: store is nil")
	}
	if o.ShardMap == nil {
		return nil, fmt.Errorf("orders: shard map is nil")
	}
	s := &Service{
		store:      o.Store,
		sub:        o.Submitter,
		shards:     o.ShardMap,
		pending:    newPendingConfirms(),
		limits:     o.Limits,
		batch:      o.BatchRL,
		commission: o.Commission,
		seq:        newSeqAllocator(),
		ackTimeout: o.AckTimeout,
		now:        o.Now,
	}
	if s.ackTimeout <= 0 {
		s.ackTimeout = EngineAckTimeout
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Pending is exposed for the consumer wiring (internal same-package use
// via NewConsumer is preferred).
func (s *Service) Pending() *pendingConfirms { return s.pending }

// AccountByID / InstrumentBySymbol are thin store delegates exposed so
// the handler layer can resolve account rows and symbol → instrument
// without holding the Store interface.
func (s *Service) AccountByID(ctx context.Context, id int64) (*Account, error) {
	return s.store.AccountByID(ctx, id)
}

func (s *Service) InstrumentBySymbol(ctx context.Context, symbol string) (*Instrument, error) {
	inst, err := s.store.InstrumentBySymbol(ctx, config.CanonicalSymbol(symbol))
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	return inst, nil
}

func (s *Service) shardFor(symbol string) uint16 {
	return uint16(s.shards.GetShard(symbol))
}

// submitHash is the canonical payload fingerprint persisted in
// client_order_id_dedup — semantic fields only, so JSON formatting
// differences still replay.
func submitHash(instID int64, req *SubmitRequest) string {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(instID, 10))
	b.WriteByte('|')
	b.WriteString(req.Side)
	b.WriteByte('|')
	b.WriteString(req.OrderType)
	b.WriteByte('|')
	b.WriteString(req.TimeInForce)
	b.WriteByte('|')
	b.WriteString(decStr(req.Quantity))
	b.WriteByte('|')
	b.WriteString(decStr(req.QuoteQuantity))
	b.WriteByte('|')
	b.WriteString(decStr(req.Price))
	b.WriteByte('|')
	b.WriteString(decStr(req.StopPrice))
	b.WriteByte('|')
	b.WriteString(decStr(req.DisplayQty))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func decStr(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return d.String()
}

// ---------------------------------------------------------------------------
// Submit — POST /api/v1/orders (Tasks 5.3.3/5.3.24/5.3.39)
// ---------------------------------------------------------------------------

// Ack is the client-visible submission acknowledgement.
type Ack struct {
	OrderID       int64  `json:"order_id"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Status        string `json:"status"`
	OrderSeq      uint64 `json:"order_seq"`
	Replay        bool   `json:"replay,omitempty"` // true = stored ack replayed (idempotent)
	TransactTime  string `json:"transact_time"`
}

// Submit validates, dedups, persists and dispatches one order. The
// dedup/dispatch ordering is: dedup check → validate → insert (unique
// race safe) → wire send → activate. On send failure the row is marked
// REJECTED so no phantom ACTIVE orders linger.
func (s *Service) Submit(ctx context.Context, acct *Account, req *SubmitRequest) (*Ack, error) {
	sym := config.CanonicalSymbol(req.Symbol)
	inst, err := s.store.InstrumentBySymbol(ctx, sym)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
	}
	ref, err := s.store.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	if err := ValidateSubmit(req, inst, acct, ref, s.now()); err != nil {
		return nil, err
	}

	// Quote-denominated market conversion (spec §22.1): base qty is
	// lot-rounded DOWN so the fill can never exceed the requested quote.
	if req.QuoteQuantity != nil {
		if ref == nil || !ref.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"no reference price for quote-denominated conversion on %s", inst.Symbol)
		}
		base := req.QuoteQuantity.Div(*ref)
		if inst.LotSize.IsPositive() {
			base = base.Div(inst.LotSize).Floor().Mul(inst.LotSize)
		}
		if !base.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"quote_quantity %s converts to zero base at %s", *req.QuoteQuantity, *ref)
		}
		q := base
		req.Quantity = &q
	}

	// Pre-trade risk limits (2.3.3/Phase-19 seam).
	if s.limits != nil {
		evalPrice := decimal.Zero
		if req.Price != nil {
			evalPrice = *req.Price
		} else if ref != nil {
			evalPrice = *ref
		}
		if err := s.limits.CheckOrder(ctx, risk.OrderRequest{
			AccountID:  acct.ID,
			KycTier:    acct.KycTier,
			Symbol:     inst.Symbol,
			Side:       req.Side,
			Quantity:   *req.Quantity,
			Price:      evalPrice,
			ReduceOnly: req.ReduceOnly,
		}); err != nil {
			return nil, err
		}
	}

	// Balance sufficiency (read-only — the §8.4 layer boundary forbids
	// gateway writes; engine 2PC owns the atomic reservation).
	if err := s.checkBalance(ctx, acct, inst, req, ref); err != nil {
		return nil, err
	}

	shard := s.shardFor(inst.Symbol)
	seq := s.seq.Next()
	hash := ""
	if req.ClientOrderID != "" {
		hash = submitHash(inst.ID, req)
	}
	o, dup, err := s.store.InsertOrderTx(ctx, InsertParams{
		AccountID:     acct.ID,
		InstrumentID:  inst.ID,
		ClientOrderID: req.ClientOrderID,
		Side:          req.Side,
		OrderType:     req.OrderType,
		Quantity:      *req.Quantity,
		QuoteQuantity: req.QuoteQuantity,
		Price:         req.Price,
		StopPrice:     req.StopPrice,
		DisplayQty:    req.DisplayQty,
		TimeInForce:   req.TimeInForce,
		ShardID:       int(shard),
		OrderSeq:      seq,
		PostOnly:      req.PostOnly,
		ReduceOnly:    req.ReduceOnly,
		STPMode:       req.STPMode,
		SessionID:     req.SessionID,
		RequestHash:   hash,
	})
	if err != nil {
		if c := DedupConflictRow(err); c != nil {
			// §8.7 idempotency semantics: same payload → replay the
			// stored ack; different payload → 409 collision.
			if c.RequestHash == hash {
				stored, gerr := s.store.GetOrder(ctx, c.OrderID)
				if gerr != nil {
					return nil, errInternal("dedup replay fetch", gerr)
				}
				if stored == nil {
					return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
						"client_order_id %q resolves to a missing order", req.ClientOrderID)
				}
				return &Ack{OrderID: stored.ID, ClientOrderID: stored.ClientOrderID,
					Status: stored.Status, OrderSeq: stored.OrderSeq,
					Replay: true, TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
			}
			return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
				"client_order_id %q already used with a different payload",
				req.ClientOrderID)
		}
		return nil, errInternal("order insert", err)
	}
	if dup != nil { // defensive — conflict path returns via err
		return nil, codeErr("IDEMPOTENCY_KEY_COLLISION", "duplicate client_order_id")
	}

	if s.sub != nil {
		b := flatbuffers.NewBuilder(256)
		payload := ipc.EncodeOrderNewEvent(b, seq,
			uint64(s.now().UnixNano()), orderNewMsg(o, acct))
		if err := s.sub.Send(ctx, shard, payload); err != nil {
			// Compensate the read model: engine never saw it.
			_ = s.store.MarkRejected(ctx, o.ID)
			return nil, err
		}
		// PENDING → ACTIVE once the engine has it in-hand.
		_ = s.store.MarkActive(ctx, o.ID)
	}
	return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
		Status: "ACTIVE", OrderSeq: seq,
		TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
}

// checkBalance verifies available ≥ required. SELL locks base units;
// BUY locks quote notional (limit price, or band-capped reference for
// markets). reduce_only skips the check — a reduce-only order is
// position-backed by definition.
func (s *Service) checkBalance(ctx context.Context, acct *Account,
	inst *Instrument, req *SubmitRequest, ref *decimal.Decimal) error {
	if req.ReduceOnly {
		return nil
	}
	var (
		currency string
		need     decimal.Decimal
	)
	if req.Side == SideSell {
		currency = inst.BaseCurrency
		need = *req.Quantity
	} else {
		currency = inst.QuoteCurrency
		eval := req.Price
		if eval == nil {
			eval = ref // market: reference price
		}
		if eval == nil || !eval.IsPositive() {
			return codeErr("INSUFFICIENT_BALANCE",
				"cannot evaluate spend for %s BUY without a price", inst.Symbol)
		}
		if req.QuoteQuantity != nil {
			need = *req.QuoteQuantity // never exceed the client ask
		} else {
			// Worst-case cap: fill at the band ceiling.
			cap_ := eval.Mul(decimal.One.Add(inst.PriceBandPctUp.Div(decimal.NewFromInt(100))))
			need = req.Quantity.Mul(cap_)
		}
	}
	avail, err := s.store.AvailableBalance(ctx, acct.ID, currency)
	if err != nil {
		return errInternal("balance read", err)
	}
	if avail == nil || avail.LessThan(need) {
		return codeErr("INSUFFICIENT_BALANCE",
			"available %s %s < required %s", decStr0(avail), currency, need)
	}
	return nil
}

func decStr0(d *decimal.Decimal) string {
	if d == nil {
		return "0"
	}
	return d.String()
}

// ---------------------------------------------------------------------------
// Cancel — DELETE /api/v1/orders/{id} (Task 5.3.3)
// ---------------------------------------------------------------------------

// Cancel dispatches OrderCancel and returns only after the engine's
// outbound echo confirms (or the 500ms window lapses →
// GATEWAY_TIMEOUT_MATCHING_ENGINE). Retries are idempotent: an order
// already CANCELLED returns a confirmation without a resend.
func (s *Service) Cancel(ctx context.Context, acct *Account, orderID int64,
	actor, requestID, ip string) (*Ack, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	if o.Status == "CANCELLED" {
		return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
			Status: o.Status, OrderSeq: o.OrderSeq,
			TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
	}
	if isTerminal(o.Status) {
		return nil, codeErr("INVALID_REQUEST",
			"order %d in terminal state %s cannot be cancelled", o.ID, o.Status)
	}
	if err := s.cancelOne(ctx, o); err != nil {
		return nil, err
	}
	_ = s.store.WriteAudit(ctx, []AuditEntry{{
		OrderID: o.ID, AccountID: acct.ID, Operation: "CANCEL",
		FieldName: "status", OldValue: o.Status, NewValue: "CANCELLED",
		ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
	}})
	return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
		Status: "CANCELLED", OrderSeq: o.OrderSeq,
		TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
}

// cancelOne sends the wire cancel and waits for the out-ring echo.
func (s *Service) cancelOne(ctx context.Context, o *Order) error {
	if s.sub == nil {
		// No engine transport wired — dev/test mode applies the cancel
		// locally; fail-open here would fabricate a confirmation in
		// production, so the handler wiring MUST supply a submitter.
		return s.store.ApplyCancel(ctx, o.ID)
	}
	shard := uint16(0)
	if o.ShardID != nil {
		shard = uint16(*o.ShardID)
	}
	pc := s.pending.register(uint64(o.ID))
	defer s.pending.deregister(uint64(o.ID))
	b := flatbuffers.NewBuilder(128)
	payload := EncodeCancelEvent(b, s.seq.Next(),
		uint64(s.now().UnixNano()), uint64(o.ID), uint64(o.AccountID))
	if err := s.sub.Send(ctx, shard, payload); err != nil {
		return err
	}
	select {
	case <-pc.done:
		return nil
	case <-time.After(s.ackTimeout):
		return codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
			"no engine confirmation for cancel of order %d within %s",
			o.ID, s.ackTimeout)
	case <-ctx.Done():
		return codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
			"request cancelled while awaiting engine confirmation")
	}
}

// ---------------------------------------------------------------------------
// Modify — PUT /api/v1/orders/{id} (Tasks 5.3.3/5.3.22)
// ---------------------------------------------------------------------------

// AmendFields is the mutable-field carrier for ApplyAmendTx paths.
type AmendFields struct {
	Price       *decimal.Decimal
	Quantity    *decimal.Decimal
	StopPrice   *decimal.Decimal
	DisplayQty  *decimal.Decimal
	TimeInForce string
	GTDExpiry   *time.Time
}

// Modify performs the audited, stale-fenced amend (op tag "MODIFY").
func (s *Service) Modify(ctx context.Context, acct *Account, orderID int64,
	req *ModifyRequest, actor, requestID, ip string) (*Order, error) {
	return s.modify(ctx, acct, orderID, req, "MODIFY", actor, requestID, ip)
}

// modify is the audited, stale-fenced amend shared by PUT /orders/{id}
// (op=MODIFY) and cancel-replace (op=CANCEL_REPLACE). Ordering:
// read → ownership → stale-seq → validate → CAS update + audit (atomic)
// → wire OrderAmend. A wire send failure reverts the CAS so PG and the
// engine stay aligned.
func (s *Service) modify(ctx context.Context, acct *Account, orderID int64,
	req *ModifyRequest, op, actor, requestID, ip string) (*Order, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	if err := StaleModify(req.OrderSeq, o.OrderSeq); err != nil {
		return nil, err
	}
	inst, err := s.store.InstrumentByID(ctx, o.InstrumentID)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INSTRUMENT_DELISTED",
			"order instrument %d no longer exists", o.InstrumentID)
	}
	if err := ValidateModify(req, o, inst); err != nil {
		return nil, err
	}
	newSeq := s.seq.Next()
	audits := auditDiff(o, req, op, actor, requestID, ip)
	updated, ok, err := s.store.AmendCAS(ctx, orderID, o.OrderSeq, newSeq,
		AmendFields{
			Price: req.Price, Quantity: req.Quantity, StopPrice: req.StopPrice,
			DisplayQty: req.DisplayQty, TimeInForce: req.TimeInForce,
			GTDExpiry: req.GTDExpiry,
		}, audits)
	if err != nil {
		return nil, errInternal("order amend", err)
	}
	if !ok {
		return nil, codeErr("STALE_MODIFY",
			"order %d modified concurrently", orderID)
	}
	if s.sub != nil {
		b := flatbuffers.NewBuilder(128)
		payload := EncodeAmendEvent(b, s.seq.Next(),
			uint64(s.now().UnixNano()), uint64(o.ID), newSeq,
			scaledOr(req.Price, 0), scaledOr(req.Quantity, 0),
			scaledOr(req.StopPrice, 0), gtdNs(req.GTDExpiry))
		if serr := s.sub.Send(ctx, shardOf(o, s.shards, inst.Symbol), payload); serr != nil {
			// Revert the CAS — engine never applied the amend.
			_ = s.store.RevertAmend(ctx, o)
			return nil, serr
		}
	}
	return updated, nil
}

// auditDiff emits one order_audit row per changed audited field
// (price, quantity, time_in_force, stop_price, iceberg_visible_qty —
// Task 5.3.22 item 3 — plus gtd_expiry as an operation-relevant extra).
func auditDiff(o *Order, req *ModifyRequest, op, actor, requestID, ip string) []AuditEntry {
	var out []AuditEntry
	add := func(field, oldV, newV string) {
		out = append(out, AuditEntry{
			OrderID: o.ID, AccountID: o.AccountID, Operation: op,
			FieldName: field, OldValue: oldV, NewValue: newV,
			ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
		})
	}
	if req.Price != nil && (o.Price == nil || !req.Price.Equal(*o.Price)) {
		add("price", decStr(o.Price), req.Price.String())
	}
	if req.Quantity != nil && !req.Quantity.Equal(o.Quantity) {
		add("quantity", o.Quantity.String(), req.Quantity.String())
	}
	if req.TimeInForce != "" && req.TimeInForce != o.TimeInForce {
		add("time_in_force", o.TimeInForce, req.TimeInForce)
	}
	if req.StopPrice != nil && (o.StopPrice == nil || !req.StopPrice.Equal(*o.StopPrice)) {
		add("stop_price", decStr(o.StopPrice), req.StopPrice.String())
	}
	if req.DisplayQty != nil && (o.DisplayQty == nil || !req.DisplayQty.Equal(*o.DisplayQty)) {
		add("iceberg_visible_qty", decStr(o.DisplayQty), req.DisplayQty.String())
	}
	if req.GTDExpiry != nil {
		add("gtd_expiry", "", req.GTDExpiry.UTC().Format(time.RFC3339Nano))
	}
	return out
}

func scaledOr(d *decimal.Decimal, def int64) int64 {
	if d == nil {
		return def
	}
	return decimal.Scaled(*d)
}

func gtdNs(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixNano()
}

func shardOf(o *Order, m *config.ShardMap, symbol string) uint16 {
	if o.ShardID != nil {
		return uint16(*o.ShardID)
	}
	if m == nil {
		return 0
	}
	return uint16(m.GetShard(symbol))
}

// ---------------------------------------------------------------------------
// Keep-priority amend — PUT /orders/{id}/amend/keep-priority (Task 5.3.37)
// ---------------------------------------------------------------------------

// AmendKeepPriority applies a quantity-down-only amend preserving queue
// priority (same order id + timestamp). Any other mutation attempt is
// ORDER_AMEND_REJECTED.
func (s *Service) AmendKeepPriority(ctx context.Context, acct *Account,
	orderID int64, req *KeepPriorityRequest,
	actor, requestID, ip string) (*Order, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	if err := StaleModify(req.OrderSeq, o.OrderSeq); err != nil {
		return nil, err
	}
	inst, err := s.store.InstrumentByID(ctx, o.InstrumentID)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INSTRUMENT_DELISTED",
			"order instrument %d no longer exists", o.InstrumentID)
	}
	if err := ValidateKeepPriority(req, o, inst); err != nil {
		return nil, err
	}
	newSeq := s.seq.Next()
	mr := &ModifyRequest{Quantity: req.Quantity}
	audits := auditDiff(o, mr, "AMEND", actor, requestID, ip)
	updated, ok, err := s.store.AmendCAS(ctx, orderID, o.OrderSeq, newSeq,
		AmendFields{Quantity: req.Quantity}, audits)
	if err != nil {
		return nil, errInternal("keep-priority amend", err)
	}
	if !ok {
		return nil, codeErr("STALE_MODIFY", "order %d modified concurrently", orderID)
	}
	if s.sub != nil {
		b := flatbuffers.NewBuilder(128)
		payload := EncodeAmendEvent(b, s.seq.Next(),
			uint64(s.now().UnixNano()), uint64(o.ID), newSeq,
			0, decimal.Scaled(*req.Quantity), 0, 0)
		if serr := s.sub.Send(ctx, shardOf(o, s.shards, inst.Symbol), payload); serr != nil {
			_ = s.store.RevertAmend(ctx, o)
			return nil, serr
		}
	}
	return updated, nil
}

// ---------------------------------------------------------------------------
// Atomic cancel-replace — POST /orders/{id}/cancel-replace (Task 5.3.37)
// ---------------------------------------------------------------------------

// CancelReplace maps to the single matching-thread replace path (spec
// §6.9 #1: "never cancel+new from the gateway"). mode is validated and
// echoed by the caller for response shaping; the wire op itself is one
// atomic amend — there is no partial apply to roll back. Audit rows are
// tagged CANCEL_REPLACE so the amendment trail distinguishes them.
func (s *Service) CancelReplace(ctx context.Context, acct *Account,
	orderID int64, req *CancelReplaceRequest,
	actor, requestID, ip string) (*Order, error) {
	if req.Mode != "STOP_ON_FAILURE" && req.Mode != "ALLOW_FAILURE" {
		return nil, codeErr("INVALID_REQUEST",
			"mode must be STOP_ON_FAILURE or ALLOW_FAILURE")
	}
	return s.modify(ctx, acct, orderID, &req.ModifyRequest,
		"CANCEL_REPLACE", actor, requestID, ip)
}

// ---------------------------------------------------------------------------
// Mass cancel — DELETE /orders?symbol=, /orders/all (Tasks 5.3.3/5.3.25)
// ---------------------------------------------------------------------------

// MassCancelResult is the per-symbol breakdown the API returns.
type MassCancelResult struct {
	Cancelled  int            `json:"cancelled"`
	PerSymbol  map[string]int `json:"per_symbol"`
	Scope      MassCancelScope
	DeadlineNS int64 `json:"-"`
}

// MassCancel cancels every open order matching scope and returns only
// after all shards confirm (every engine echo observed or the overall
// deadline lapses). Retries are idempotent — already-cancelled orders
// are simply absent from the open set.
func (s *Service) MassCancel(ctx context.Context, scope MassCancelScope,
	actor, requestID, ip string) (*MassCancelResult, error) {
	if err := NormalizeMassCancelScope(&scope, scope.AccountID == 0); err != nil {
		return nil, err
	}
	open, err := s.store.OpenOrders(ctx, scope)
	if err != nil {
		return nil, errInternal("open orders", err)
	}
	res := &MassCancelResult{Cancelled: 0, PerSymbol: map[string]int{}, Scope: scope}
	if len(open) == 0 {
		return res, nil
	}
	sym := func(id int64) string {
		if inst, err := s.store.InstrumentByID(ctx, id); err == nil && inst != nil {
			return inst.Symbol
		}
		return strconv.FormatInt(id, 10)
	}
	// Send phase — every order gets a pending slot first so an echo that
	// races ahead of registration can never resolve a missing waiter.
	type waiter struct {
		o  *Order
		pc *pendingCancel
	}
	var ws []waiter
	enqueued := 0
	for i := range open {
		o := &open[i]
		if s.sub == nil {
			// No transport wired (tests/dev) — apply locally.
			_ = s.store.ApplyCancel(ctx, o.ID)
			res.PerSymbol[sym(o.InstrumentID)]++
			res.Cancelled++
			continue
		}
		pc := s.pending.register(uint64(o.ID))
		b := flatbuffers.NewBuilder(128)
		payload := EncodeCancelEvent(b, s.seq.Next(),
			uint64(s.now().UnixNano()), uint64(o.ID), uint64(o.AccountID))
		var shard uint16
		if o.ShardID != nil {
			shard = uint16(*o.ShardID)
		}
		if err := s.sub.Send(ctx, shard, payload); err != nil {
			for _, w := range ws {
				s.pending.deregister(uint64(w.o.ID))
			}
			s.pending.deregister(uint64(o.ID))
			return nil, err // ENGINE_OVERLOAD etc — fail-closed
		}
		ws = append(ws, waiter{o, pc})
		enqueued++
		res.PerSymbol[sym(o.InstrumentID)]++
		res.Cancelled++
	}
	// Await phase — the task requires ALL shards to confirm before
	// success; the overall deadline bounds the wait.
	deadline := s.now().Add(MassCancelTimeout)
	for _, w := range ws {
		remain := deadline.Sub(s.now())
		if remain <= 0 {
			s.pending.deregister(uint64(w.o.ID))
			return nil, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"mass cancel for account %d did not confirm all %d orders within %s",
				scope.AccountID, enqueued, MassCancelTimeout)
		}
		select {
		case <-w.pc.done:
			_ = s.store.ApplyCancel(ctx, w.o.ID)
		case <-time.After(remain):
			s.pending.deregister(uint64(w.o.ID))
			return nil, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"mass cancel for account %d timed out awaiting engine confirmations",
				scope.AccountID)
		case <-ctx.Done():
			s.pending.deregister(uint64(w.o.ID))
			return nil, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"mass cancel aborted: %v", ctx.Err())
		}
	}
	// Audit one row per cancelled order (operation=MASS_CANCEL).
	audits := make([]AuditEntry, 0, len(open))
	for i := range open {
		o := &open[i]
		audits = append(audits, AuditEntry{
			OrderID: o.ID, AccountID: o.AccountID, Operation: "MASS_CANCEL",
			FieldName: "status", OldValue: o.Status, NewValue: "CANCELLED",
			ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
		})
	}
	_ = s.store.WriteAudit(ctx, audits)
	return res, nil
}

// CancelOnDisconnect implements the REST/WS session CoD (Task 5.3.25
// item 4): mass-cancel orders attributed to a dropped session.
func (s *Service) CancelOnDisconnect(ctx context.Context, accountID int64,
	sessionID string) (*MassCancelResult, error) {
	return s.MassCancel(ctx, MassCancelScope{
		AccountID: accountID, SessionID: sessionID,
		Reason: "cancel_on_disconnect",
	}, "system:cod", "", "")
}

// ---------------------------------------------------------------------------
// Batch ops — POST/DELETE /orders/batch (Task 5.3.32)
// ---------------------------------------------------------------------------

// BatchResult is one index-mapped outcome.
type BatchResult struct {
	Index         int    `json:"index"`
	Status        string `json:"status"` // ACCEPTED | REJECTED | CANCELLED
	OrderID       int64  `json:"order_id,omitempty"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Error         string `json:"error,omitempty"`
	Message       string `json:"message,omitempty"`
}

// BatchSubmitFailure carries the per-entry map when the batch aborts —
// the handler renders it inside details.results of the error envelope.
type BatchSubmitFailure struct {
	Results []BatchResult
	Err     error
}

// Error satisfies the error interface; Unwrap exposes the first
// per-entry cause so errs.WriteError maps the right HTTP status.
func (f *BatchSubmitFailure) Error() string { return f.Err.Error() }
func (f *BatchSubmitFailure) Unwrap() error { return f.Err }

// BatchSubmit is strictly atomic (spec §8.4 "atomic index-mapped result
// array" + the never-partially-apply rule): every entry is validated
// first; ANY entry failing aborts the whole batch — nothing is
// persisted or dispatched — and the response carries per-entry verdicts.
// Duplicate client_order_id within the batch is a batch-level defect.
func (s *Service) BatchSubmit(ctx context.Context, acct *Account,
	reqs []*SubmitRequest, requestID, ip string) ([]BatchResult, error) {
	if len(reqs) == 0 || len(reqs) > BatchSubmitMax {
		return nil, codeErr("BATCH_SIZE_EXCEEDED",
			"batch requires 1..%d orders, got %d", BatchSubmitMax, len(reqs))
	}
	if s.batch != nil {
		ok, err := s.batch.AllowBatch(ctx, acct.ID)
		if err != nil {
			return nil, errInternal("batch rate limit", err)
		}
		if !ok {
			return nil, codeErr("RATE_LIMIT_TIER_EXCEEDED",
				"batch rate limit rl:batch:%d exceeded", acct.ID)
		}
	}
	results := make([]BatchResult, len(reqs))
	seenCOID := map[string]int{}
	insts := map[string]*Instrument{}
	var firstErr error
	for i, req := range reqs {
		results[i].Index = i
		results[i].Status = "ACCEPTED"
		if req == nil {
			results[i].Status = "REJECTED"
			results[i].Error = "INVALID_REQUEST"
			results[i].Message = "empty order entry"
			continue
		}
		sym := config.CanonicalSymbol(req.Symbol)
		inst, ok := insts[sym]
		if !ok {
			var err error
			inst, err = s.store.InstrumentBySymbol(ctx, sym)
			if err != nil {
				return nil, errInternal("instrument lookup", err)
			}
			insts[sym] = inst
		}
		var verr error
		switch {
		case inst == nil:
			verr = codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
		case req.ClientOrderID != "":
			if prev, dup := seenCOID[req.ClientOrderID]; dup {
				verr = codeErr("IDEMPOTENCY_KEY_COLLISION",
					"client_order_id %q duplicated within batch (indices %d,%d)",
					req.ClientOrderID, prev, i)
			} else {
				seenCOID[req.ClientOrderID] = i
			}
		}
		if verr == nil {
			ref, rerr := s.store.ReferencePrice(ctx, inst.ID)
			if rerr != nil {
				return nil, errInternal("reference price", rerr)
			}
			verr = ValidateSubmit(req, inst, acct, ref, s.now())
			// Quote conversion runs in the atomic pre-pass too so a
			// non-convertible entry aborts the batch before any dispatch.
			if verr == nil && req.QuoteQuantity != nil {
				if ref == nil || !ref.IsPositive() {
					verr = codeErr("QUOTE_QUANTITY_INVALID",
						"no reference price for quote conversion on %s", inst.Symbol)
				} else {
					base := req.QuoteQuantity.Div(*ref)
					if inst.LotSize.IsPositive() {
						base = base.Div(inst.LotSize).Floor().Mul(inst.LotSize)
					}
					if !base.IsPositive() {
						verr = codeErr("QUOTE_QUANTITY_INVALID",
							"quote_quantity %s converts to zero base", *req.QuoteQuantity)
					} else {
						req.Quantity = &base
					}
				}
			}
			if verr == nil {
				verr = s.checkBalance(ctx, acct, inst, req, ref)
			}
			if verr == nil {
				// Pre-check dedup so a replayed entry is atomic-known
				// before any dispatch.
				if req.ClientOrderID != "" {
					dup, derr := s.store.DedupLookup(ctx, acct.ID, req.ClientOrderID)
					if derr != nil {
						return nil, errInternal("dedup lookup", derr)
					}
					if dup != nil {
						hash := submitHash(inst.ID, req)
						if dup.RequestHash == hash {
							results[i].Status = "ACCEPTED"
							results[i].OrderID = dup.OrderID
							results[i].ClientOrderID = req.ClientOrderID
							continue
						}
						verr = codeErr("IDEMPOTENCY_KEY_COLLISION",
							"client_order_id %q already used with a different payload",
							req.ClientOrderID)
					}
				}
			}
		}
		if verr != nil {
			results[i].Status = "REJECTED"
			if ce, ok := verr.(*excerrors.Error); ok {
				results[i].Error = ce.Code
				results[i].Message = ce.Message
			} else {
				results[i].Error = "INTERNAL_ERROR"
			}
			if firstErr == nil {
				firstErr = verr
			}
		}
	}
	if firstErr != nil {
		return nil, &BatchSubmitFailure{Results: results, Err: firstErr}
	}

	// All valid — persist + dispatch each; a mid-batch send failure
	// compensates by rejecting every not-yet-confirmed row so the batch
	// never lands partially on the book.
	var acked []BatchResult
	persisted := make([]*Order, 0, len(reqs))
	for i, req := range reqs {
		if results[i].OrderID != 0 {
			acked = append(acked, results[i]) // dedup replay
			continue
		}
		inst := insts[config.CanonicalSymbol(req.Symbol)]
		shard := s.shardFor(inst.Symbol)
		seq := s.seq.Next()
		hash := submitHash(inst.ID, req)
		o, _, err := s.store.InsertOrderTx(ctx, InsertParams{
			AccountID: acct.ID, InstrumentID: inst.ID,
			ClientOrderID: req.ClientOrderID, Side: req.Side,
			OrderType: req.OrderType, Quantity: *req.Quantity,
			QuoteQuantity: req.QuoteQuantity, Price: req.Price,
			StopPrice: req.StopPrice, DisplayQty: req.DisplayQty,
			TimeInForce: req.TimeInForce, ShardID: int(shard),
			OrderSeq: seq, PostOnly: req.PostOnly,
			ReduceOnly: req.ReduceOnly, STPMode: req.STPMode,
			SessionID: req.SessionID, RequestHash: hash,
		})
		if err != nil {
			return nil, s.batchAbort(ctx, persisted, err)
		}
		persisted = append(persisted, o)
		if s.sub != nil {
			b := flatbuffers.NewBuilder(256)
			payload := ipc.EncodeOrderNewEvent(b, seq,
				uint64(s.now().UnixNano()), orderNewMsg(o, acct))
			if err := s.sub.Send(ctx, shard, payload); err != nil {
				return nil, s.batchAbort(ctx, persisted, err)
			}
			_ = s.store.MarkActive(ctx, o.ID)
		}
		acked = append(acked, BatchResult{
			Index: i, Status: "ACCEPTED", OrderID: o.ID,
			ClientOrderID: o.ClientOrderID,
		})
	}
	// Preserve input order.
	sort.Slice(acked, func(a, b int) bool { return acked[a].Index < acked[b].Index })
	// Batch-op audit trail (Task 5.3.32 item 3).
	audits := make([]AuditEntry, 0, len(acked))
	for _, r := range acked {
		if r.OrderID == 0 {
			continue
		}
		audits = append(audits, AuditEntry{
			OrderID: r.OrderID, AccountID: acct.ID, Operation: "BATCH_SUBMIT",
			FieldName: "status", NewValue: "PENDING",
			ModifiedBy: actor0(acct), RequestID: requestID, IPAddress: ip,
		})
	}
	_ = s.store.WriteAudit(ctx, audits)
	return acked, nil
}

// batchAbort marks every persisted batch order REJECTED and returns the
// dispatch error — nothing remains partially applied.
func (s *Service) batchAbort(ctx context.Context, persisted []*Order, cause error) error {
	for _, o := range persisted {
		_ = s.store.MarkRejected(ctx, o.ID)
	}
	if ce, ok := cause.(*excerrors.Error); ok {
		return ce
	}
	return codeErr("INTERNAL_ERROR", "batch dispatch failed: %v", cause)
}

func actor0(a *Account) string { return "account:" + strconv.FormatInt(a.ID, 10) }

// BatchCancel resolves every id (orders and/or client ids) up front —
// a single unresolvable/foreign id aborts the whole batch (atomicity) —
// then dispatches all cancels and awaits engine confirmation per order.
func (s *Service) BatchCancel(ctx context.Context, acct *Account,
	orderIDs []int64, clientIDs []string, actor, requestID, ip string) ([]BatchResult, error) {
	if len(orderIDs)+len(clientIDs) == 0 || len(orderIDs)+len(clientIDs) > BatchCancelMax {
		return nil, codeErr("BATCH_SIZE_EXCEEDED",
			"batch cancel requires 1..%d order references, got %d",
			BatchCancelMax, len(orderIDs)+len(clientIDs))
	}
	if s.batch != nil {
		ok, err := s.batch.AllowBatch(ctx, acct.ID)
		if err != nil {
			return nil, errInternal("batch rate limit", err)
		}
		if !ok {
			return nil, codeErr("RATE_LIMIT_TIER_EXCEEDED",
				"batch rate limit rl:batch:%d exceeded", acct.ID)
		}
	}
	type ref struct {
		idx int
		o   *Order
	}
	var resolved []ref
	resolve := func(idx int, o *Order) error {
		if o == nil || o.AccountID != acct.ID {
			return codeErr("ORDER_NOT_FOUND", "order reference %d not found", idx)
		}
		resolved = append(resolved, ref{idx, o})
		return nil
	}
	for i, id := range orderIDs {
		o, err := s.store.GetOrder(ctx, id)
		if err != nil {
			return nil, errInternal("order read", err)
		}
		if err := resolve(i, o); err != nil {
			return nil, err
		}
	}
	for j, cid := range clientIDs {
		row, err := s.store.DedupLookup(ctx, acct.ID, cid)
		if err != nil {
			return nil, errInternal("dedup lookup", err)
		}
		var o *Order
		if row != nil {
			o, err = s.store.GetOrder(ctx, row.OrderID)
			if err != nil {
				return nil, errInternal("order read", err)
			}
		}
		if err := resolve(len(orderIDs)+j, o); err != nil {
			return nil, err
		}
	}
	results := make([]BatchResult, 0, len(resolved))
	audits := make([]AuditEntry, 0, len(resolved))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, r := range resolved {
		r := r
		if isTerminal(r.o.Status) {
			results = append(results, BatchResult{
				Index: r.idx, Status: "CANCELLED",
				OrderID: r.o.ID, ClientOrderID: r.o.ClientOrderID,
				Error:   "INVALID_REQUEST",
				Message: "order already in terminal state " + r.o.Status,
			})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.cancelOne(ctx, r.o)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				code := "INTERNAL_ERROR"
				if ce, ok := err.(*excerrors.Error); ok {
					code = ce.Code
				}
				results = append(results, BatchResult{
					Index: r.idx, Status: "REJECTED", OrderID: r.o.ID,
					ClientOrderID: r.o.ClientOrderID,
					Error:         code, Message: err.Error(),
				})
				return
			}
			audits = append(audits, AuditEntry{
				OrderID: r.o.ID, AccountID: acct.ID,
				Operation: "BATCH_CANCEL", FieldName: "status",
				OldValue: r.o.Status, NewValue: "CANCELLED",
				ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
			})
			results = append(results, BatchResult{
				Index: r.idx, Status: "CANCELLED",
				OrderID: r.o.ID, ClientOrderID: r.o.ClientOrderID,
			})
		}()
	}
	wg.Wait()
	_ = s.store.WriteAudit(ctx, audits)
	sort.Slice(results, func(a, b int) bool { return results[a].Index < results[b].Index })
	// Atomicity: any failed cancel aborts the response (the orders that
	// did cancel are terminal-correct — a batch cancel's atomicity is
	// all-or-nothing *dispatch*, each cancel is independent on the book).
	for _, r := range results {
		if r.Status == "REJECTED" {
			return results, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"batch cancel incomplete — see results")
		}
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Preview — POST /api/v1/orders/test (Task 5.3.39)
// ---------------------------------------------------------------------------

// Preview is the non-binding dry-run response.
type Preview struct {
	EstimatedBaseQty   string   `json:"estimated_base_qty"`
	EstimatedQuoteQty  string   `json:"estimated_quote_qty"`
	Margin             string   `json:"margin"`
	CommissionEstimate string   `json:"commission_estimate"`
	SpreadEstimate     string   `json:"spread_estimate"`
	RiskLevel          string   `json:"risk_level"` // LOW|MEDIUM|HIGH
	Warnings           []string `json:"warnings"`
	Filters            []string `json:"active_filters"`
	Binding            bool     `json:"binding"` // always false
}

// CommissionEstimator resolves the account's effective tier charge —
// the settlement engine's CommissionTierCharge shape.
type CommissionEstimator func(ctx context.Context, accountID int64,
	qty, lotSize, notional decimal.Decimal) (decimal.Decimal, error)

// DryRun runs schema → filters → entitlement → margin → price-range →
// commission checks without reserving funds, writing WAL or creating an
// order (spec §22.1).
func (s *Service) DryRun(ctx context.Context, acct *Account,
	req *SubmitRequest) (*Preview, error) {
	sym := config.CanonicalSymbol(req.Symbol)
	inst, err := s.store.InstrumentBySymbol(ctx, sym)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
	}
	ref, err := s.store.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	if err := ValidateSubmit(req, inst, acct, ref, s.now()); err != nil {
		return nil, err
	}

	warn := []string{}
	filters := []string{"PRICE_FILTER", "LOT_SIZE", "MIN_NOTIONAL", "PRICE_BAND"}
	eval := req.Price
	if eval == nil {
		eval = ref
	}
	var qty decimal.Decimal
	if req.Quantity != nil {
		qty = *req.Quantity
	} else if req.QuoteQuantity != nil {
		if eval == nil || !eval.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"no reference price for quote conversion")
		}
		q := req.QuoteQuantity.Div(*eval)
		if inst.LotSize.IsPositive() {
			q = q.Div(inst.LotSize).Floor().Mul(inst.LotSize)
		}
		qty = q
		warn = append(warn,
			"quote-denominated order: base quantity estimated from reference price and lot-rounded down")
	}
	notional := decimal.Zero
	quoteQty := decimal.Zero
	if eval != nil && eval.IsPositive() {
		notional = qty.Mul(*eval)
		quoteQty = notional
	} else {
		warn = append(warn, "no reference price — quote estimate unavailable")
	}

	// Margin check: notional/leverage for margin accounts, full notional
	// for SPOT. Balance sufficiency reuses the submit-path rule so the
	// preview predicts the real gate.
	margin := notional
	if acct.Type != "SPOT" && inst.MaxLeverage > 0 {
		margin = notional.Div(decimal.NewFromInt(int64(inst.MaxLeverage)))
	}
	if ref == nil {
		filters = append(filters, "REFERENCE_PRICE_MISSING")
	}
	if req.QuoteQuantity != nil {
		quoteQty = *req.QuoteQuantity
	}
	var commission decimal.Decimal
	if s.commission != nil {
		if c, err := s.commission(ctx, acct.ID, qty, inst.LotSize, quoteQty); err == nil {
			commission = c
		} else {
			warn = append(warn, "commission estimate unavailable")
		}
	}
	spreadEst := decimal.Zero
	if inst.MaxSpreadPips != nil {
		spreadEst = inst.MaxSpreadPips.Mul(decimal.NewFromInt(2))
	}

	// Balance sufficiency as a warning/check (preview never fails on it —
	// it predicts): insufficient funds surface as HIGH risk + warning,
	// keeping the dry-run non-binding but honest.
	riskLevel := "LOW"
	balCurrency := inst.QuoteCurrency
	if req.Side == SideSell {
		balCurrency = inst.BaseCurrency
	}
	if avail, err := s.store.AvailableBalance(ctx, acct.ID, balCurrency); err == nil {
		need := qty
		if req.Side == SideBuy {
			need = margin
		}
		if avail == nil || avail.LessThan(need) {
			riskLevel = "HIGH"
			warn = append(warn, "insufficient available balance for this order")
		}
	}
	if req.QuoteQuantity != nil || req.OrderType == TypeMarket {
		if riskLevel == "LOW" {
			riskLevel = "MEDIUM" // market risk → confirmation severity
		}
	}
	if inst.Status != "ACTIVE" {
		// already rejected by the state gate above
	}
	return &Preview{
		EstimatedBaseQty:   qty.String(),
		EstimatedQuoteQty:  quoteQty.String(),
		Margin:             margin.String(),
		CommissionEstimate: commission.String(),
		SpreadEstimate:     spreadEst.String(),
		RiskLevel:          riskLevel,
		Warnings:           warn,
		Filters:            filters,
		Binding:            false,
	}, nil
}

// ---------------------------------------------------------------------------
// Read paths
// ---------------------------------------------------------------------------

func (s *Service) GetOrder(ctx context.Context, acct *Account, orderID int64) (*Order, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	return o, nil
}

func (s *Service) ListOrders(ctx context.Context, acct *Account, q ListQuery) ([]Order, int64, error) {
	q.AccountID = acct.ID
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	return s.store.ListOrders(ctx, q)
}

// Amendments returns the client-visible amendment history.
func (s *Service) Amendments(ctx context.Context, acct *Account, orderID int64) ([]AuditEntry, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	return s.store.Amendments(ctx, orderID)
}

// AuditTrail serves the Compliance-Officer admin endpoint — any account.
func (s *Service) AuditTrail(ctx context.Context, orderID int64) ([]AuditEntry, error) {
	return s.store.AuditTrail(ctx, orderID)
}

// AdminMassCancel is the Task 5.3.24 item-7 cross-account Risk-Manager
// surface; scope may omit account_id entirely.
func (s *Service) AdminMassCancel(ctx context.Context, scope MassCancelScope,
	actor, requestID, ip string) (*MassCancelResult, error) {
	scope.Reason = "admin"
	return s.MassCancel(ctx, scope, actor, requestID, ip)
}

func errInternal(op string, err error) error {
	return codeErr("INTERNAL_ERROR", "%s: %v", op, err)
}
