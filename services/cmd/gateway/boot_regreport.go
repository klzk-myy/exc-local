package main

// boot_regreport.go — the regulatory-reporting + venue-governance boot
// stage (formerly inline in run()): canonical event store, RTS 22 /
// EMIR REFIT / CFTC 43-45 reporters, APA/ARM/TR/SDR vendor clients,
// the JetStream dispatch pump + durable consumers, Basel EOD, FXGC
// self-assessment, execution-policy consent gate and venue admission.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/accounts"
	"exchange/internal/api"
	"exchange/internal/compliance"
	regreport "exchange/internal/compliance/reporting"
	"exchange/internal/compliance/venue"
	"exchange/internal/funding"
	"exchange/internal/nats"
	"exchange/internal/oracle/rates"
	"exchange/internal/redis"
	"exchange/internal/settlement"
	"exchange/internal/timesync"
)

type regCluster struct {
	regStore      *regreport.PgStore
	regDeps       api.RegReportingDeps
	regClients    map[regreport.Destination]compliance.VendorClient
	regChangeSvc  *compliance.RegChangeService
	baselSvc      *compliance.BaselService
	fxgcSvc       *compliance.FXGCService
	execPolicySvc *compliance.ExecutionPolicyService
	venueSvc      *venue.Service
	venueGate     *venue.AdmissionGate
}

func bootRegCluster(sweepCtx context.Context, pool *pgxpool.Pool,
	rdb *redis.Client, natsClient *nats.Client,
	opsAlerter funding.OpsAlerter,
	adminRoleResolver func(context.Context, int64) (string, error),
	productGate *accounts.ProductGateService,
	usdConv *settlement.RedisUsdConverter,
	log *slog.Logger) (*regCluster, error) {
	// Canonical event store + version-pinned schema registry (migration
	// 054, spec §5.32/§14.1a), transport ledger (059, §5.36), the MiFID
	// II RTS 22 / EMIR REFIT / CFTC Parts 43-45 regime adapters and the
	// APA/ARM/TR/SDR dispatch pump. Reportable events arrive from the
	// `trades` and `settlements` JetStream streams via the independent
	// durable consumers provisioned below (LimitsPolicy fan-out,
	// explicit-ack at-least-once — never WorkQueue).
	regStore, err := regreport.NewPgStore(pool)
	if err != nil {
		return nil, fmt.Errorf("regulatory reporting store: %w", err)
	}
	regCfg := regreport.ConfigFromEnv()
	regCfg.Alerter = regreport.AlertFunc(func(ctx context.Context, sev, code, summary string) error {
		if opsAlerter == nil {
			return nil
		}
		return opsAlerter.Raise(ctx, settlement.OpsAlert{
			Severity: sev, Code: code, Summary: summary})
	})
	regSvc, err := regreport.NewService(regStore, regCfg)
	if err != nil {
		return nil, fmt.Errorf("regulatory reporting service: %w", err)
	}
	mifidRep, err := compliance.NewMiFIDReporter(regSvc)
	if err != nil {
		return nil, fmt.Errorf("mifid reporter: %w", err)
	}
	emirRep, err := compliance.NewEMIRReporter(regSvc,
		compliance.ForwardPointsOracle(rates.NewStore(rdb.Client).SwapPointFor))
	if err != nil {
		return nil, fmt.Errorf("emir reporter: %w", err)
	}
	// EMIR REFIT NEWTs carry required valuation/margin/notional — the
	// enrich hook prices them off the Phase-19.5 forward-points oracle.
	// A stale/missing quote fails closed (consumer NAK → redelivery);
	// nothing is fabricated.
	regSvc.Cfg.DerivativeEnrich = emirRep.EnrichNEWT
	dfRep, err := compliance.NewDoddFrankReporter(regSvc)
	if err != nil {
		return nil, fmt.Errorf("dodd-frank reporter: %w", err)
	}
	regLedger, err := compliance.NewSubmissionsLedger(pool)
	if err != nil {
		return nil, fmt.Errorf("regulatory submissions ledger: %w", err)
	}
	// Vendor endpoints come from env (EXC_APA_URL / EXC_ARM_URL /
	// EXC_TR_URL / EXC_SDR_URL). An absent client is fail-closed: the
	// dispatcher ledger-marks ENDPOINT_UNCONFIGURED and pages P1 — a
	// report is never silently dropped or claimed sent.
	regClients := map[regreport.Destination]compliance.VendorClient{}
	if c, cerr := compliance.APAClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationAPA] = c
	} else {
		log.Warn("APA endpoint unconfigured — transparency submissions park", "err", cerr)
	}
	if c, cerr := compliance.ARMClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationARM] = c
	} else {
		log.Warn("ARM endpoint unconfigured — RTS 22 submissions park", "err", cerr)
	}
	if c, cerr := compliance.TRClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationTR] = c
	} else {
		log.Warn("TR endpoint unconfigured — EMIR submissions park", "err", cerr)
	}
	if c, cerr := compliance.SDRClientFromEnv(); cerr == nil {
		regClients[regreport.DestinationSDR] = c
	} else {
		log.Warn("SDR endpoint unconfigured — CFTC submissions park", "err", cerr)
	}
	regDispatch := &compliance.SubmissionDispatcher{
		Svc: regSvc, Ledger: regLedger, Clients: regClients,
		Alert: regCfg.Alerter,
	}
	// Dispatch sweep every 5s + daily reconciliation passes for the
	// derivative regimes (spec §14.1a — zero unexplained divergence is
	// the acceptance state; breaks surface in the repair queue).
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if n, derr := regDispatch.DispatchOnce(sweepCtx, 100); derr != nil {
					log.Warn("regulatory dispatch sweep failed", "err", derr)
				} else if n > 0 {
					log.Info("regulatory artifacts dispatched", "count", n)
				}
			}
		}
	}()
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				for _, regime := range []regreport.Regime{
					regreport.RegimeEMIRREFIT, regreport.RegimeCFTCP45} {
					if rep, rerr := regSvc.Reconcile(sweepCtx, regime); rerr != nil {
						log.Warn("regulatory reconcile failed", "regime", regime, "err", rerr)
					} else {
						log.Info("regulatory reconcile pass", "regime", rep.Regime,
							"checked", rep.CheckedEvents, "open", rep.OpenInternal,
							"breaks_opened", len(rep.BreaksOpened))
					}
				}
			}
		}
	}()
	// Durable consumers — independent cursors on `trades` and
	// `settlements` (at-least-once; replay is absorbed by UTI
	// idempotency + the (uti, regime, report_seq) collision key).
	if natsClient != nil {
		execCons, errc := regreport.NewExecutionConsumer(regSvc)
		if errc == nil {
			if cons, cerr := natsClient.EnsureConsumer(context.Background(), "trades",
				regreport.DurableTrades, nats.WithFilterSubject("trades.>")); cerr != nil {
				log.Warn("regulatory trades consumer unavailable", "err", cerr)
			} else {
				go func() {
					if cerr := execCons.Consume(sweepCtx, cons); cerr != nil {
						log.Error("regulatory trades consumer stopped", "err", cerr)
					}
				}()
				log.Info("regulatory reporting consuming", "stream", "trades",
					"durable", regreport.DurableTrades)
			}
		}
		settleCons, errs := regreport.NewSettlementConsumer(regSvc)
		if errs == nil {
			if cons, cerr := natsClient.EnsureConsumer(context.Background(), "settlements",
				regreport.DurableSettlements, nats.WithFilterSubject("settlements.>")); cerr != nil {
				log.Warn("regulatory settlements consumer unavailable", "err", cerr)
			} else {
				go func() {
					if cerr := settleCons.Consume(sweepCtx, cons); cerr != nil {
						log.Error("regulatory settlements consumer stopped", "err", cerr)
					}
				}()
				log.Info("regulatory reporting consuming", "stream", "settlements",
					"durable", regreport.DurableSettlements)
			}
		}
	} else {
		log.Warn("NATS unavailable — regulatory reporting consumers deferred")
	}
	regDeps := api.RegReportingDeps{
		Svc: regSvc, Ledger: regLedger,
		MiFID: mifidRep, EMIR: emirRep, DoddFrank: dfRep,
		TrustProxy: true,
	}
	// ---- end Phase-21 regulatory reporting ----

	// ---- Phase-21 wave-2 governance cluster: Basel III capital pack
	// (21.3.13), FX Global Code 55-principle review (21.3.17),
	// regulatory-change watch register (21.3.25), execution-policy
	// lifecycle + order-entry consent gate (21.3.28) ----
	baselSvc, err := compliance.NewBaselService(pool)
	if err != nil {
		return nil, fmt.Errorf("basel service: %w", err)
	}
	baselSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}}).
		WithRateConverter(func(ctx context.Context, ccy string) (decimal.Decimal, error) {
			return usdConv.ToUSD(ctx, ccy, decimal.NewFromInt(1))
		})

	fxgcSvc, err := compliance.NewFXGCService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return nil, fmt.Errorf("fx global code service: %w", err)
	}
	fxgcSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}}).
		WithClockEvidence(func(ctx context.Context) (int64, bool, error) {
			// P10 probe reads the deploy-role PTP status file —
			// grandmaster-synced AND |offset| ≤ 1µs is adherence.
			rd, err := timesync.StatsFileReader{
				Path: timesync.DefaultPTPStatusPath}.Read(ctx)
			if err != nil {
				return 0, false, err
			}
			off := rd.OffsetNs
			if off < 0 {
				off = -off
			}
			return off, rd.Synced, nil
		})

	regChangeSvc, err := compliance.NewRegChangeService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return nil, fmt.Errorf("regulatory change service: %w", err)
	}
	regChangeSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	// The 10-business-day triage SLA counts on the venue's core
	// regulatory calendar — the Fed / TARGET2 / BoE settlement holiday
	// union already maintained for Task 3.3.8.
	if hrows, herr := pool.Query(context.Background(), `
		SELECT DISTINCT holiday_date FROM currency_holidays
		WHERE currency IN ('USD','EUR','GBP')`); herr == nil {
		var hols []time.Time
		for hrows.Next() {
			var d time.Time
			if hrows.Scan(&d) == nil {
				hols = append(hols, d)
			}
		}
		hrows.Close()
		regChangeSvc.WithHolidays(hols)
	} else {
		log.Warn("regulatory holiday calendar unavailable — weekday-only SLA", "err", herr)
	}

	execPolicySvc, err := compliance.NewExecutionPolicyService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return nil, fmt.Errorf("execution policy service: %w", err)
	}
	execPolicySvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})

	// Task 21.3.28 — the consent gate composes behind the profile /
	// target-market gate: every non-reduce-only admission also needs a
	// consent row for the ACTIVE policy version (PRODUCT_NOT_PERMITTED
	// refusal; probe failure fails closed).
	consentGate := &compliance.PolicyConsentGate{
		Inner: productGate, Policy: execPolicySvc}

	// Basel III EOD snapshot — daily 03:00 UTC (after the Phase-20
	// 02:00 RTS28 slot), idempotent on 'eod:{period}'; breach pages
	// fire inside Snapshot → raiseBreaches.
	runDailyUTC(sweepCtx, log, "basel-eod", 180, func(ctx context.Context) {
		if rep, created, err := baselSvc.RunEOD(ctx); err != nil {
			log.Warn("basel EOD snapshot failed", "err", err)
		} else if created {
			log.Info("basel EOD snapshot stored",
				"period", rep.Period.Format("2006-01-02"),
				"car", rep.CAR.String(), "leverage", rep.LeverageRatio.String(),
				"car_breach", rep.CARBreach, "lev_breach", rep.LeverageBreach,
				"inputs_complete", rep.InputsComplete)
		}
	})
	// Regulatory-change SLA + execution-policy review: hourly — the
	// 10-business-day triage breach (RULEBOOK_VERSION_STALE P1), the
	// 90-day effective-date window (REGULATORY_DEADLINE_APPROACHING)
	// and the overdue annual-review freeze each dedup on WORM audit
	// markers, so rerunning is safe.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				if sla, dl, err := regChangeSvc.Sweep(sweepCtx); err != nil {
					log.Warn("regulatory change sweep failed", "err", err)
				} else if sla+dl > 0 {
					log.Info("regulatory change alerts raised",
						"triage_sla", sla, "deadline", dl)
				}
				if n, err := execPolicySvc.SweepOverdueReview(sweepCtx); err != nil {
					log.Warn("execution-policy review sweep failed", "err", err)
				} else if n > 0 {
					log.Info("execution-policy overdue review paged", "flagged", n)
				}
			}
		}
	}()
	// Task 21.3.15 — regulated-venue governance: member/DEA register +
	// admission gate, rulebook/product versioning, market-control record,
	// cases/conflicts, self-assessment/CCO report, launch prerequisites.
	// The admission gate wraps the product/consent chain — a member's
	// trading access fails closed on absent due diligence, agreements,
	// admission, review currency or jurisdiction licensing.
	venueSvc, err := venue.NewService(pool,
		compliance.HoldRoleResolver(adminRoleResolver))
	if err != nil {
		return nil, fmt.Errorf("venue governance service: %w", err)
	}
	venueSvc.WithAlerter(holdOpsAlerter{inner: freezeOpsAlerter{pool: pool, page: opsAlerter}})
	venueGate := &venue.AdmissionGate{Inner: consentGate, Members: venueSvc}
	// ---- end Phase-21 wave-2 governance cluster ----
	return &regCluster{
		regStore:      regStore,
		regDeps:       regDeps,
		regClients:    regClients,
		regChangeSvc:  regChangeSvc,
		baselSvc:      baselSvc,
		fxgcSvc:       fxgcSvc,
		execPolicySvc: execPolicySvc,
		venueSvc:      venueSvc,
		venueGate:     venueGate,
	}, nil
}
