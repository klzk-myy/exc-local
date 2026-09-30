// Package-internal wiring adapters for cmd/gateway — small bridges
// between the Phase-05 subsystems (order pipeline, dead-man switch,
// manual liquidation sink) and the seams they plug into.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	goredis "github.com/redis/go-redis/v9"

	"exchange/internal/accounts"
	"exchange/internal/admin"
	"exchange/internal/analytics"
	"exchange/internal/api"
	"exchange/internal/auth"
	"exchange/internal/compliance"
	"exchange/internal/config"
	"exchange/internal/funding"
	"exchange/internal/instruments"
	"exchange/internal/marketapi"
	"exchange/internal/nats"
	"exchange/internal/orders"
	excredis "exchange/internal/redis"
	"exchange/internal/reporting"
	"exchange/internal/settlement"
)

// shardIDs returns the full engine shard universe for the out-ring
// consumer: every statically-mapped shard plus the elastic range
// [elasticBase, elasticBase+elasticCount).
func shardIDs(m *config.ShardMap) []uint16 {
	set := map[int]struct{}{}
	for _, id := range m.Entries() {
		set[id] = struct{}{}
	}
	for i := m.ElasticBase(); i < m.ElasticBase()+m.ElasticCount(); i++ {
		set[i] = struct{}{}
	}
	out := make([]uint16, 0, len(set))
	for id := range set {
		out = append(out, uint16(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// shardIDsAsInt adapts the shard universe for the admin.SessionService
// (Phase-15 Task 15.3.7 keys session:state:{shard_id} on int ids).
func shardIDsAsInt(m *config.ShardMap) []int {
	ids := shardIDs(m)
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		out = append(out, int(id))
	}
	return out
}

// countdownAdapter exposes accounts.DeadManService as the ws surface's
// CountdownController (Task 5.3.33 — countdown_ms 0 disables).
type countdownAdapter struct {
	svc *accounts.DeadManService
}

func (a countdownAdapter) Set(ctx context.Context, accountID, countdownMs int64, renew bool) (serverTime, expiry int64, err error) {
	ack, err := a.svc.Set(ctx, accountID, countdownMs, renew)
	if err != nil {
		return 0, 0, err
	}
	return ack.ServerTime, ack.CountdownExpiry, nil
}

// liquidationSink publishes MANUAL_LIQUIDATION events to the
// margin-events JetStream stream (Task 5.3.30 → Phase-19 auction
// machinery). Subject is account-ordered: margin-events.0.ACCT-{id}.
// A publish failure propagates so the endpoint fails closed.
type liquidationSink struct {
	nc *nats.Client
}

func (s liquidationSink) EmitManualLiquidation(ctx context.Context, ev api.ManualLiquidationEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = s.nc.Publish(ctx, "margin-events", 0,
		"ACCT-"+strconv.FormatInt(ev.AccountID, 10), payload)
	return err
}

// supportAlerter routes support SLA / complaint-escalation alerts to the
// ops alert channel (Task 7.3.7 — COMPLAINT_SLA_BREACH is internal, so
// breach surfacing is an alert, not an API error). NATS publish is
// best-effort; an absent or disconnected bus degrades to logging by
// the caller, never an error — alerts must not wedge ticket writes.
// Implements support.Alerter. (Named supportAlerter — `opsAlerter` is
// the funding.OpsAlerter local in run().)
type supportAlerter struct {
	nc *nats.Client
}

func (a supportAlerter) Raise(_ context.Context, severity, code, message string) error {
	payload, err := json.Marshal(map[string]string{
		"severity": severity, "code": code, "message": message,
		"source": "support",
	})
	if err != nil {
		return err
	}
	if a.nc == nil || !a.nc.Connected() {
		return nil
	}
	return a.nc.Conn().Publish("ops.alerts.support", payload)
}

// vdpAlerter routes VDP SLA-breach / expedite alerts to the ops alert
// channel (Phase-13.5 Task 13.5.3.8 — VDP_SLA_BREACH is an internal ops
// alert per the §23 internal-only list, never an API error). NATS
// publish is best-effort; an absent or disconnected bus degrades to
// logging by the caller, never an error — alerts must not wedge the
// disclosure register. Implements security.Alerter.
type vdpAlerter struct {
	nc *nats.Client
}

func (a vdpAlerter) Raise(_ context.Context, severity, code, message string) error {
	payload, err := json.Marshal(map[string]string{
		"severity": severity, "code": code, "message": message,
		"source": "vdp",
	})
	if err != nil {
		return err
	}
	if a.nc == nil || !a.nc.Connected() {
		return nil
	}
	return a.nc.Conn().Publish("ops.alerts.security", payload)
}

// orderDispatchAdapter binds accounts.OrderDispatcher (dead-man sweeper,
// close-all) to orders.Service — the single dispatch path, never a
// direct engine call.
type orderDispatchAdapter struct {
	svc   *orders.Service
	store *orders.PgStore
}

func (a orderDispatchAdapter) MassCancel(ctx context.Context, scope accounts.MassCancelScope) (*accounts.MassCancelResult, error) {
	res, err := a.svc.MassCancel(ctx, orders.MassCancelScope{
		AccountID:    scope.AccountID,
		InstrumentID: scope.InstrumentID,
		Side:         scope.Side,
		OrderType:    scope.OrderType,
		Reason:       scope.Reason,
	}, "system:"+scope.Reason, "", "")
	if err != nil {
		return nil, err
	}
	return &accounts.MassCancelResult{Cancelled: res.Cancelled}, nil
}

func (a orderDispatchAdapter) SubmitClose(ctx context.Context, req accounts.CloseOrderRequest) (*accounts.OrderAck, error) {
	inst, err := a.store.InstrumentByID(ctx, req.InstrumentID)
	if err != nil || inst == nil {
		return nil, fmt.Errorf("instrument %d not found", req.InstrumentID)
	}
	acct, err := a.store.AccountByID(ctx, req.AccountID)
	if err != nil || acct == nil {
		return nil, fmt.Errorf("account %d not found", req.AccountID)
	}
	qty := req.Quantity
	ack, err := a.svc.Submit(ctx, acct, &orders.SubmitRequest{
		Symbol:        inst.Symbol,
		Side:          string(req.Side),
		OrderType:     orders.TypeMarket,
		Quantity:      &qty,
		ReduceOnly:    req.ReduceOnly,
		ClientOrderID: req.ClientOrderID,
	})
	if err != nil {
		return nil, err
	}
	return &accounts.OrderAck{
		OrderID: ack.OrderID, ClientOrderID: ack.ClientOrderID,
		Accepted: true,
	}, nil
}

// ---------------------------------------------------------------------------
// Phase-11 rails+returns adapters
// ---------------------------------------------------------------------------

// railGate binds the scoped kill-switch halt flags — halt:rail:{RAIL_ID}
// (Phase-11 Task 11.3.8/11.3.12, keys owned by internal/redis.HaltKey).
// A missing key means the rail is available; a Redis error fails closed
// for that rail only (other rails may still serve the instruction).
type railGate struct{ rdb *goredis.Client }

func (g railGate) Available(ctx context.Context, rail funding.RailID) (bool, string, error) {
	if g.rdb == nil {
		return true, "", nil // no coordination instance — no scoped halts possible
	}
	key := excredis.HaltKey(admin.ScopeRail, string(rail))
	v, err := g.rdb.Get(ctx, key).Result()
	if err != nil {
		if err == goredis.Nil {
			return true, "", nil
		}
		return false, "", fmt.Errorf("rail gate read %s: %w", key, err)
	}
	return false, v, nil
}

// ---------------------------------------------------------------------------
// Phase-11 kill-switch adapters (Tasks 11.3.4/11.3.8/11.3.12)
// ---------------------------------------------------------------------------

// killSwitchAnnouncer publishes a public status-page announcement for
// every suspension transition — INCIDENT on SET, GENERAL on CLEAR.
type killSwitchAnnouncer struct {
	store *marketapi.PgStore
}

func (a killSwitchAnnouncer) AnnounceSuspension(ctx context.Context, ev admin.SuspensionEvent) error {
	category, verb := "INCIDENT", "activated"
	if ev.Action == "CLEAR" {
		category, verb = "GENERAL", "cleared"
	}
	target := ev.Scope
	if ev.TargetID != "" {
		target += ":" + ev.TargetID
	}
	_, err := a.store.CreateAnnouncement(ctx, marketapi.Announcement{
		Title: fmt.Sprintf("Trading suspension %s — %s", verb, target),
		Body: fmt.Sprintf(
			"Kill-switch %s for scope %s%s. Reason: %s.",
			verb, ev.Scope, targetSuffixOf(ev.TargetID), ev.Reason),
		Category: category, Status: "PUBLISHED",
		PublishAt: time.Now(), CreatedBy: "kill-switch",
	})
	return err
}

func targetSuffixOf(t string) string {
	if t == "" {
		return ""
	}
	return " target=" + t
}

// ---------------------------------------------------------------------------
// Phase-12 Tasks 12.3.10/12.3.11 — emergency freeze + delegation adapters
// ---------------------------------------------------------------------------

// apiKeyRevoker binds accounts.CredentialRevoker to auth.KeyStore —
// the self-freeze saga revokes every live API key on the account.
type apiKeyRevoker struct {
	ks *auth.KeyStore
}

func (a apiKeyRevoker) RevokeAllKeys(ctx context.Context, accountID int64, reason string) (int, error) {
	keys, err := a.ks.ListByAccount(ctx, accountID)
	if err != nil {
		return 0, err
	}
	revoked := 0
	var firstErr error
	for _, k := range keys {
		if err := a.ks.Revoke(ctx, k.KeyID, reason); err != nil {
			// An already-revoked key is a no-op; anything else is a real
			// failure — keep going (never leave later keys live because
			// an early one raced) and surface the first error.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		revoked++
	}
	return revoked, firstErr
}

// openOrderLister binds accounts.OpenOrderLister to orders.PgStore —
// the still-cancellable set the self-freeze P1 alert carries for manual
// desk cancellation.
type openOrderLister struct {
	store *orders.PgStore
}

func (l openOrderLister) OpenOrderIDs(ctx context.Context, accountID int64) ([]int64, error) {
	rows, err := l.store.OpenOrders(ctx, orders.MassCancelScope{AccountID: accountID})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, o := range rows {
		ids = append(ids, o.ID)
	}
	return ids, nil
}

// delegatedSessionKiller binds delegation.SessionTerminator to the
// killSessions closure (user + every account index).
type delegatedSessionKiller struct {
	kill func(ctx context.Context, userID int64) error
}

func (k delegatedSessionKiller) KillUserSessions(ctx context.Context, userID int64) error {
	return k.kill(ctx, userID)
}

// freezeOpsAlerter implements accounts.FreezeAlerter: the P1 page goes
// to the shared NATS ops-alerts subject AND a durable
// funding_ops_alerts row — the alert survives a pager outage.
type freezeOpsAlerter struct {
	pool *pgxpool.Pool
	page funding.OpsAlerter // nil → durable row only
}

func (a freezeOpsAlerter) Raise(ctx context.Context, al accounts.FreezeAlert) error {
	detail, _ := json.Marshal(al.Details)
	_, derr := a.pool.Exec(ctx, `
		INSERT INTO funding_ops_alerts (code, severity, account_id, summary, detail)
		VALUES ($1, $2, $3, $4, $5)`,
		al.Code, al.Severity, al.AccountID, al.Summary, detail)
	var perr error
	if a.page != nil {
		perr = a.page.Raise(ctx, funding.OpsAlert{
			Severity: al.Severity, Code: al.Code, Summary: al.Summary,
			Err:     al.Details["error"],
			Details: al.Details,
		})
	}
	if derr != nil {
		return derr
	}
	return perr
}

// killSwitchControlPub emits the Task 11.3.12 control message on the
// exchange:control:killswitch topic — the NATS publisher reaches the
// bridge that replicates it onto the Aeron control ring the C++
// matching shards consume.
type killSwitchControlPub struct {
	nc *nats.Client
}

func (p killSwitchControlPub) PublishControl(_ context.Context, topic string, ev admin.SuspensionEvent) error {
	payload, err := ev.Marshal()
	if err != nil {
		return err
	}
	if p.nc == nil || !p.nc.Connected() {
		return fmt.Errorf("control publish %s: nats disconnected", topic)
	}
	return p.nc.Conn().Publish(topic, payload)
}

// restingOrderCanceller runs the SCOPE_COUNTERPARTY resting-order sweep
// through the canonical orders mass-cancel path — never a direct engine
// call; the C++ shards also receive the control message and cancel
// in-core.
type restingOrderCanceller struct {
	svc *orders.Service
}

func (c restingOrderCanceller) CancelAccountOrders(ctx context.Context,
	accountID int64, reason string) (int, error) {
	res, err := c.svc.MassCancel(ctx, orders.MassCancelScope{
		AccountID: accountID, Reason: "admin",
	}, "kill-switch:"+reason, "", "")
	if err != nil {
		return 0, err
	}
	return res.Cancelled, nil
}

// pgLegalNameResolver sources the account's legal-name comparator from
// the verified beneficiary registry — bank_accounts.beneficiary_name is
// "must match KYC legal name" by contract (migration 040). The Phase-14
// KYC document store will supersede this binding when it lands; an
// account with no verified beneficiary resolves "" → the deposit guard
// fail-closes to quarantine (unverifiable name, never an implicit pass).
type pgLegalNameResolver struct{ pool *pgxpool.Pool }

func (r pgLegalNameResolver) LegalName(ctx context.Context, accountID int64) (string, error) {
	var name string
	err := r.pool.QueryRow(ctx, `
		SELECT beneficiary_name FROM bank_accounts
		WHERE account_id = $1 AND status = 'VERIFIED'
		ORDER BY verified_at DESC NULLS LAST, bank_account_id DESC
		LIMIT 1`, accountID).Scan(&name)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("legal name lookup: %w", err)
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// Phase-14 account-lifecycle adapters (Tasks 14.3.9–14.3.12)
// ---------------------------------------------------------------------------

// closureSweeper sends one residual balance through the Phase-11
// withdrawal pipeline: flow.Create applies the same beneficiary /
// sanctions / cooldown gates a client withdrawal sees, inner.Confirm
// consumes the minted email token the create returned, and
// dispatch.Release hands CONFIRMED rows to the banking rails. Amounts
// in the >$50K tier land PENDING_REVIEW — the sweep still counts as
// dispatched (funds locked to the beneficiary in transit, ops reviews
// through the existing admin route); the status lands in sweep_refs.
type closureSweeper struct {
	flow  *funding.FlowService
	inner *funding.WithdrawalService
	disp  *funding.DispatchService
}

func (s closureSweeper) SweepWithdrawal(ctx context.Context,
	req accounts.SweepRequest) (*accounts.SweepResult, error) {
	res, err := s.flow.Create(ctx, funding.CreateWithdrawalRequest{
		AccountID:        req.AccountID,
		UserID:           req.UserID,
		Currency:         req.Currency,
		Amount:           req.Amount,
		ReferenceAccount: req.DestinationRef,
		ConfirmMethod:    "email",
		IdempotencyKey:   req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	conf, err := s.inner.Confirm(ctx, funding.ConfirmWithdrawalRequest{
		WithdrawalID: res.WithdrawalID,
		AccountID:    req.AccountID,
		UserID:       req.UserID,
		Token:        res.ConfirmToken,
	})
	if err != nil {
		return nil, err
	}
	if conf.Status == "CONFIRMED" && s.disp != nil {
		if _, derr := s.disp.Release(ctx, conf.WithdrawalID); derr != nil {
			// The confirmation committed — a failed rail hand-off is
			// picked up by the dispatcher sweep; never mask it as a
			// sweep failure (same contract as FlowService.ConfirmStepUp).
			return &accounts.SweepResult{
				WithdrawalID: conf.WithdrawalID,
				Status:       conf.Status + "/DISPATCH_PENDING",
			}, nil
		}
	}
	return &accounts.SweepResult{
		WithdrawalID: conf.WithdrawalID,
		Status:       conf.Status,
	}, nil
}

// closureBens resolves the verified (unlocked) beneficiary for a
// residual currency — bank_accounts rows past the 24h verification
// hold are the only legal sweep destinations.
type closureBens struct {
	pool *pgxpool.Pool
}

func (b closureBens) VerifiedBeneficiaryFor(ctx context.Context,
	accountID int64, currency string) (string, bool, error) {
	var ref string
	err := b.pool.QueryRow(ctx,
		`SELECT COALESCE(iban, account_number)
		   FROM bank_accounts
		  WHERE account_id = $1 AND currency = $2
		    AND status = 'VERIFIED' AND unlocked_at <= now()
		  ORDER BY bank_account_id LIMIT 1`,
		accountID, currency).Scan(&ref)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("beneficiary lookup: %w", err)
	}
	if ref == "" {
		return "", false, nil
	}
	return ref, true, nil
}

// holdRestingCanceller adapts compliance.RestingCanceller onto the
// existing order dispatcher — the hold placement drains the book via
// the same mass-cancel path every other freeze flow uses.
type holdRestingCanceller struct {
	disp accounts.OrderDispatcher
}

func (c holdRestingCanceller) CancelResting(ctx context.Context,
	accountID int64, reason string) (int, error) {
	res, err := c.disp.MassCancel(ctx, accounts.MassCancelScope{
		AccountID: accountID, Reason: reason})
	if err != nil {
		return 0, err
	}
	return res.Cancelled, nil
}

// instrumentOrderCanceller adapts admin.InstrumentOrderCanceller onto
// the orders dispatcher — the SUSPENDED 5-minute grace sweep
// mass-cancels the instrument's whole resting book through the same
// pipeline every other de-risking flow uses (Task 15.3.1).
type instrumentOrderCanceller struct {
	disp accounts.OrderDispatcher
}

func (c instrumentOrderCanceller) CancelInstrumentOrders(ctx context.Context,
	instrumentID int64, reason string) (int, error) {
	res, err := c.disp.MassCancel(ctx, accounts.MassCancelScope{
		InstrumentID: instrumentID, Reason: reason})
	if err != nil {
		return 0, err
	}
	return res.Cancelled, nil
}

// auctionFreezeGate adapts orders.AuctionGate onto the armed auction
// control key + the per-instrument auction calendar (Phase-16 Task
// 16.3.25): queued MOO/MOC cancel/amend is rejected while frozen —
// frozen = armed CALL/EXTEND key present, or now inside the T-30s
// window before the next matching trigger.
type auctionFreezeGate struct {
	feed admin.RedisStatusFeed
	cal  *instruments.CalendarStore
}

// Frozen implements orders.AuctionGate.
func (g auctionFreezeGate) Frozen(ctx context.Context, inst *orders.Instrument,
	orderType string, now time.Time) (bool, error) {
	armed, err := g.feed.AuctionArmed(ctx, inst.Symbol)
	if err != nil {
		return false, err
	}
	if armed != "" {
		return true, nil // CALL/EXTEND armed — queue is immutable
	}
	trg := g.nextTrigger(ctx, inst.Symbol, orderType, now)
	if trg.IsZero() {
		return false, nil // no resolvable auction — nothing freezes
	}
	// [trigger−30s, trigger+call window]: the post-trigger tail covers
	// the scheduler's arm latency; the armed key takes over once CALL
	// publishes.
	end := trg.Add(instruments.CallAuctionLen)
	return !now.Before(trg.Add(-30*time.Second)) && now.Before(end), nil
}

// nextTrigger resolves the next auction the order type uncrosses at:
// MOC → earliest enabled DAILY_CLOSE/INTRADAY calendar occurrence
// (falling back to the canonical Friday 22:00 UTC weekly close);
// MOO → the weekly session open (Sunday 21:00 UTC).
func (g auctionFreezeGate) nextTrigger(ctx context.Context, symbol,
	orderType string, now time.Time) time.Time {
	switch orderType {
	case orders.TypeMOC:
		var best time.Time
		if g.cal != nil {
			rows, err := g.cal.CalendarFor(ctx, symbol)
			if err == nil {
				for _, e := range rows {
					if !e.Enabled || e.AuctionType == instruments.AuctionFixing {
						continue
					}
					if at, oerr := e.NextOccurrence(now); oerr == nil &&
						(best.IsZero() || at.Before(best)) {
						best = at
					}
				}
			}
		}
		if best.IsZero() {
			best = nextWeekdayUTC(now, time.Friday, 22, 0) // §6.2b weekly close
		}
		return best
	case orders.TypeMOO:
		return nextWeekdayUTC(now, time.Sunday, 21, 0) // §6.2b weekly open
	}
	return time.Time{}
}

// nextWeekdayUTC returns the next wd-at-hh:mm UTC strictly after now.
func nextWeekdayUTC(now time.Time, wd time.Weekday, hh, mm int) time.Time {
	t := now.UTC()
	delta := (int(wd) - int(t.Weekday()) + 7) % 7
	at := time.Date(t.Year(), t.Month(), t.Day()+delta, hh, mm, 0, 0, time.UTC)
	if !at.After(t) {
		at = at.AddDate(0, 0, 7)
	}
	return at
}

// auctionTypeCanceller adapts instruments.AuctionOrderCanceller onto
// the orders dispatcher — the §7.1 close-auction remainder cancel is
// the scoped mass-cancel (instrument × order_type, Task 15.3.13).
type auctionTypeCanceller struct {
	disp accounts.OrderDispatcher
}

func (c auctionTypeCanceller) CancelOrderType(ctx context.Context,
	instrumentID int64, orderType, reason string) (int, error) {
	res, err := c.disp.MassCancel(ctx, accounts.MassCancelScope{
		InstrumentID: instrumentID, OrderType: orderType, Reason: reason})
	if err != nil {
		return 0, err
	}
	return res.Cancelled, nil
}

// holdOpsAlerter bridges compliance.HoldAlerter onto the durable
// funding_ops_alerts + pager seam the freeze flows already use.
type holdOpsAlerter struct {
	inner accounts.FreezeAlerter
}

func (a holdOpsAlerter) RaiseHold(ctx context.Context,
	al compliance.HoldAlert) error {
	return a.inner.Raise(ctx, accounts.FreezeAlert{
		Severity: al.Severity, Code: al.Code, AccountID: al.AccountID,
		Summary: al.Summary, Details: al.Details,
	})
}

// closureEscalation adapts compliance.ClosureEscalation onto the
// four-eyes queue — an escalate-to-closure disposition creates the
// OpAccountClosure request a second officer approves through the
// existing /admin/dual-control/* routes.
type closureEscalation struct {
	dual *admin.DualControlService
}

func (e closureEscalation) RequestForcedClosure(ctx context.Context,
	accountID int64, reason string, requestedBy int64) (int64, error) {
	req, err := e.dual.Submit(ctx, admin.SubmitInput{
		Operation:  admin.OpAccountClosure,
		TargetType: "account",
		TargetID:   strconv.FormatInt(accountID, 10),
		Payload: map[string]any{
			"account_id": accountID, "reason": reason,
		},
		RequiredRole: admin.RoleComplianceOfficer,
		RequestedBy:  requestedBy,
		Reason:       "compliance hold escalate-to-closure: " + reason,
	})
	if err != nil {
		return 0, err
	}
	return req.ID, nil
}

// natsJet returns the JetStream context of the optional NATS client —
// nil-safe (the publisher seam treats a nil JS as "unwired" and the
// admin surfaces log + continue rather than fail open).
func natsJet(nc *nats.Client) jetstream.JetStream {
	if nc == nil {
		return nil
	}
	return nc.JetStream()
}

// maintenanceAlerter bridges admin.MaintenanceAlerter onto the durable
// funding_ops_alerts pager seam — Task 15.3.8 emergency parameter
// changes fire their P1 alert through the same path as the funding/ops
// flows.
type maintenanceAlerter struct{ inner funding.OpsAlerter }

func (a maintenanceAlerter) Alert(ctx context.Context, severity, code,
	message string) error {
	if a.inner == nil {
		return fmt.Errorf("ops alerter unwired")
	}
	return a.inner.Raise(ctx, settlement.OpsAlert{
		Severity: severity, Code: code, Summary: message,
	})
}

// ---------------------------------------------------------------------------
// Phase-20 adapters (analytics & reporting wiring)
// ---------------------------------------------------------------------------

// pgAccountTierLookup implements analytics.AccountTierLookup — the CH
// volume-stats tier join resolves accounts.client_category in-process
// (canonical SQL per the analytics/stats.go package doc; CH cannot reach
// PG). Unresolved accounts are simply absent from the map — the store
// folds them into its unresolved bucket rather than guessing a tier.
type pgAccountTierLookup struct{ pool *pgxpool.Pool }

// ClientCategories implements analytics.AccountTierLookup.
func (l pgAccountTierLookup) ClientCategories(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := l.pool.Query(ctx,
		`SELECT id, client_category::text FROM accounts WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var cat string
		if err := rows.Scan(&id, &cat); err != nil {
			return nil, err
		}
		out[id] = cat
	}
	return out, rows.Err()
}

// analyticsConfirmationGenerator adapts analytics.ConfirmationService
// (the Task 20.3.6 generation seam — row type analytics.Confirmation)
// onto reporting.ConfirmationGenerator (the Task 20.3.8 delivery seam —
// mirror row type). Field-for-field; both row shapes are identical by
// contract.
type analyticsConfirmationGenerator struct {
	svc *analytics.ConfirmationService
}

// Generate implements reporting.ConfirmationGenerator.
func (g analyticsConfirmationGenerator) Generate(ctx context.Context, tradeID int64) ([]reporting.ConfirmationRecord, error) {
	rows, err := g.svc.Generate(ctx, tradeID)
	return confirmationRecords(rows), err
}

// MarkAdjusted implements reporting.ConfirmationGenerator.
func (g analyticsConfirmationGenerator) MarkAdjusted(ctx context.Context, tradeID int64) ([]reporting.ConfirmationRecord, error) {
	rows, err := g.svc.MarkAdjusted(ctx, tradeID)
	return confirmationRecords(rows), err
}

func confirmationRecords(rows []analytics.Confirmation) []reporting.ConfirmationRecord {
	out := make([]reporting.ConfirmationRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, reporting.ConfirmationRecord{
			ConfirmationID: r.ConfirmationID,
			TradeID:        r.TradeID,
			AccountID:      r.AccountID,
			Version:        r.Version,
			Status:         r.Status,
			FileRef:        r.FileRef,
			ContentSHA256:  r.ContentSHA256,
			GeneratedAt:    r.GeneratedAt,
			DeliveredAt:    r.DeliveredAt,
			SupersedesID:   r.SupersedesID,
		})
	}
	return out
}
