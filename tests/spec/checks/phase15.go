package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-15 market admin & instrument lifecycle checkpoints (Tasks
// 15.3.1–15.3.13). Evidence split: the Go control plane (state machine,
// sessions, listing, bust) is covered by unit + PG/Redis-gated tests; the
// C++ admission gates and reopening auction are covered by gtest cases in
// core/tests/test_lifecycle_auction.cpp (ctest target of the same name).
const (
	p15Admin = "./internal/admin"
	p15Instr = "./internal/instruments"
)

func registerPhase15(r *spec.Registry) {
	r.Register("P15-T15.3.1-C1", ckP15LifecycleStates,
		"instrument lifecycle 7 states including persistent CANCEL_ONLY (§24 #290)")
	r.Register("P15-T15.3.1-C2", ckP15GracePeriods,
		"SUSPENDED 5min cancel-only grace; RESTRICTED 24h; DELISTED 30d")
	r.Register("P15-T15.3.2-C1", ckP15AdminAPI,
		"admin instrument management API")
	r.Register("P15-T15.3.3-C1", ckP15CoreStatus,
		"C++ core respects instrument status")
	r.Register("P15-T15.3.4-C1", ckP15TradingHours,
		"24/5 trading hours (spec §1)")
	r.Register("P15-T15.3.5-C1", ckP15TradeBust,
		"trade bust/price-adjust with dual control + GL reversal (§7.2, §24 #138)")
	r.Register("P15-T15.3.6-C1", ckP15ReopeningAuction,
		"reopening auction on resume + weekly open (§7.1, §24 #142)")
	r.Register("P15-T15.3.7-C1", ckP15SessionLifecycle,
		"24/5 Trading Session Lifecycle (§6.7, §24 #217)")
	r.Register("P15-T15.3.8-C1", ckP15Maintenance,
		"instrument maintenance workflow")
	r.Register("P15-T15.3.9-C1", ckP15CancelOnly,
		"persistent CANCEL_ONLY rejects entry/amend while preserving all cancellation paths (§24 #290)")
	r.Register("P15-T15.3.10-C1", ckP15AuctionFailure,
		"reopening auction clearing failure and crossed-book quarantine fail closed (§24 #316)")
	r.Register("P15-T15.3.11-C1", ckP15InstrumentReference,
		"seeded instrument reference, calendar-correct sessions with value-date blocking, tenor grid, and stated no-corporate-actions rule (§24 #343)")
	r.Register("P15-T15.3.12-C1", ckP15OpsConsole,
		"listing proposals with auto-checks, impact-previewed delisting ladder, and approval-paired ops-board transitions (§24 #352)")
	r.Register("P15-T15.3.13-C1", ckP15AuctionCalendar,
		"daily closing-auction calendar with MOC/FIXING execution (§24 #401)")
}

// --- Task 15.3.1: lifecycle state machine -----------------------------------

func ckP15LifecycleStates(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/admin/instrument_lifecycle.go"),
		structural(env, "services/internal/admin/instrument_lifecycle.go",
			"DRAFT", "ACTIVE", "CANCEL_ONLY", "RESTRICTED", "SUSPENDED",
			"HALTED", "DELISTED", "lifecycleTransitions"),
		structural(env, "services/internal/db/migrations/001_create_instruments.up.sql",
			"CANCEL_ONLY"),
		gotest(p15Admin, "TestLifecycleTransitionMatrix|"+
			"TestLifecycleRoleMatrix|TestInstrumentLifecyclePGTransitions"),
	)
}

func ckP15GracePeriods(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/admin/instrument_lifecycle.go",
			"grace", "sweep"),
		gotest(p15Admin, "TestInstrumentLifecycleSuspendSweepPG|"+
			"TestInstrumentLifecycleDualControlPG"),
	)
}

// --- Task 15.3.2: admin instrument API ---------------------------------------

func ckP15AdminAPI(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/api/admin_instruments.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"instruments/{id}/activate", "instruments/{id}/restrict",
			"instruments/{id}/cancel-only", "instruments/{id}/suspend",
			"instruments/{id}/halt", "instruments/{id}/resume",
			"instruments/{id}/delist"),
		gotest(p15Admin, "TestInstrumentLifecyclePGTransitions|"+
			"TestInstrumentLifecycleDualControlPG|"+
			"TestInstrumentLifecycleUpdatePG|TestInstrumentLifecycleCreateTxPG"),
	)
}

// --- Task 15.3.3/15.3.4: C++ status gates + 24/5 hours -----------------------

func ckP15CoreStatus(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/include/risk/InstrumentFeed.hpp",
			"core/src/risk/InstrumentFeedRefresher.cpp",
			"core/tests/test_lifecycle_auction.cpp"),
		gtest("test_lifecycle_auction",
			"Lifecycle.ParseInstrumentStatus:Lifecycle.UnverifiableFeedFailsClosed:"+
				"Lifecycle.StatusMatrixNewOrders:Lifecycle.RestrictedIsLimitOnly:"+
				"Lifecycle.DelistedReduceOnlyWindow:Lifecycle.CancelsStayOpenAmendsGated"),
	)
}

func ckP15TradingHours(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/admin/market_schedule.go",
			"services/internal/db/migrations/221_market_schedule_overrides.up.sql"),
		gtest("test_lifecycle_auction",
			"Lifecycle.ParseMarketHours:Lifecycle.MarketEntryWeek:"+
				"Lifecycle.MarketHoursGate"),
		gotest(p15Admin, "TestScheduleOverrideValidation|"+
			"TestMarketScheduleIntegration"),
	)
}

// --- Task 15.3.5: trade bust & price adjust ----------------------------------

func ckP15TradeBust(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/admin/trade_busts.go",
			"services/internal/db/migrations/051_trade_busts.up.sql"),
		structural(env, "services/internal/admin/trade_busts.go",
			"BUST", "PRICE_ADJUST", "PostJournal"),
		gotest(p15Admin, "TestBustJournalBalanced|TestAdjustJournalDelta|"+
			"TestBustRoleGate|TestBustApproverDistinct|TestBustExecutePG|"+
			"TestBustOfBustedPG|TestBustWindowExpiredPG|TestBustSettledRejectsPG|"+
			"TestBustPendingApprovePG|TestPartialFillBustPG|TestBustAfterHedgePG|"+
			"TestPriceAdjustPG|TestBustInsideBandRejectsPG|TestBustExpirePendingPG"),
	)
}

// --- Task 15.3.6/15.3.10: reopening auction + failure paths ------------------

func ckP15ReopeningAuction(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "core/tests/test_lifecycle_auction.cpp"),
		gtest("test_lifecycle_auction",
			"Auction.CallAccumulatesCrossedBook:Auction.MarketAndIocFokAreParked:"+
				"Auction.AmendRejectedCancelAllowed:Auction.UncrossSinglePriceAndResume:"+
				"Auction.ExtendMovesDeadline:Auction.CompletedDeadlineDoesNotReenter:"+
				"Auction.WalReplayConverges:Lifecycle.ParseAuctionCall"),
		gotest(p15Admin, "TestReopeningAuctionRule"),
	)
}

func ckP15AuctionFailure(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gtest("test_lifecycle_auction",
			"Auction.WithdrawnKeyQuarantinesCrossedBook:"+
				"Auction.StrikeFailAwaitsExtensionThenClears:"+
				"Auction.ReArmClearsQuarantineByUncrossing"),
		structural(env, "services/internal/errs/codes.go",
			"AUCTION_CLEARING_FAILED", "CROSSED_BOOK_DETECTED"),
	)
}

// --- Task 15.3.7: weekly session lifecycle ------------------------------------

func ckP15SessionLifecycle(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "services/internal/admin/session_lifecycle.go"),
		structural(env, "services/internal/admin/session_lifecycle.go",
			"PRE_CLOSE", "PRE_OPEN", "session:state", "RolloverRunner"),
		gotest(p15Admin, "TestSessionStateAt|TestNextBoundary|"+
			"TestLastBoundary|TestSessionEventName|"+
			"TestNewSessionServiceFailClosed|TestSessionLifecycleWeeklyFlow"),
	)
}

// --- Task 15.3.8: maker-checker maintenance ----------------------------------

func ckP15Maintenance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/admin/instrument_maintenance.go",
			"services/internal/db/migrations/219_instrument_change_log.up.sql"),
		structural(env, "services/internal/admin/instrument_maintenance.go",
			"instrument_change_log"),
		gotest(p15Admin, "TestPairSymbolValidation|TestParamValidation|"+
			"TestDefaultNextSessionStart|TestMaintenanceRoleGate|"+
			"TestCreateMakerCheckerChainPG|TestParamChangeScheduledPG|"+
			"TestEmergencyParamChangePG|TestDelistWorkflowPG"),
	)
}

// --- Task 15.3.9: persistent CANCEL_ONLY --------------------------------------

func ckP15CancelOnly(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		gtest("test_lifecycle_auction", "Lifecycle.StatusMatrixNewOrders:Lifecycle.CancelsStayOpenAmendsGated"),
		gotest(p15Admin, "TestInstrumentLifecycleSuspendSweepPG"),
	)
}

// --- Task 15.3.11–15.3.13: reference, ops console, auction calendar ----------

func ckP15InstrumentReference(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/db/migrations/087_instrument_reference.up.sql",
			"services/internal/instruments/reference.go",
			"services/internal/instruments/sessions.go"),
		gotest(p15Instr, "TestWeeklyOpenDST|TestSessionStates|"+
			"TestTradeDayAndCutoff|TestParseTenor|TestTenorValueDates|"+
			"TestTenorHolidaySkip|TestTenorEndOfMonthRule|TestTenorBracket|"+
			"TestIMMStubs|TestValidateValueDate"),
	)
}

func ckP15OpsConsole(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/instruments/listing.go",
			"services/internal/instruments/opsboard.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"admin/listing-proposals", "admin/ops-board"),
		gotest(p15Instr, "TestListingLifecycleIntegration|"+
			"TestDelistLadderIntegration|TestOpsBoardIntegration"),
	)
}

func ckP15AuctionCalendar(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/instruments/auction_calendar.go",
			"services/internal/instruments/fixing_scheduler.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"auction-calendar"),
		gotest(p15Instr, "TestCalendarReplaceIntegration|"+
			"TestFixingSchedulerIntegration"),
	)
}
