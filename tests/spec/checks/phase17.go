package checks

import (
	"context"

	spec "exchange-testspec/spec"
)

// Phase-17 L3 order-level data checkpoints (Tasks 17.3.1–17.3.4). Evidence
// split: the C++ publisher is covered by gtest cases in
// core/tests/test_l3.cpp (ctest target test_l3); the Go distribution,
// snapshot, gap-recovery, and surveillance surfaces are covered by unit +
// PG-gated tests.
const (
	p17Md   = "./internal/marketdata"
	p17Surv = "./internal/surveillance"
	p17Ipc  = "./internal/ipc"
)

func registerPhase17(r *spec.Registry) {
	r.Register("P17-T17.3.1-C1", ckP17L3Generation,
		"L3 order-level data — defined first, validated against spec")
	r.Register("P17-T17.3.2-C1", ckP17L3Distribution,
		"L3 WS distribution — defined first, validated against spec")
	r.Register("P17-T17.3.3-C1", ckP17Surveillance,
		"market-abuse signal generation — defined first, validated against spec")
	r.Register("P17-T17.3.4-C1", ckP17GapRecovery,
		"L3 monotonic gap recovery and buffer overrun throttling (§24 #318) — defined first, validated against spec")
}

// --- Task 17.3.1: L3 generation (C++ engine) ------------------------------------

func ckP17L3Generation(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"core/src/ipc/L3Publisher.cpp",
			"core/tests/test_l3.cpp"),
		structural(env, "core/proto/exchange.fbs",
			"L3OrderEvent"),
		gtest("test_l3",
			"L3.RestingLimitEmitsAdd:L3.FillEmitsTakerAndMakerLegsSharingWalSeq:"+
				"L3.CancelEmitsWithWalCorrelation:"+
				"L3.QtyDownAmendEmitsModifyKeepingPrice:"+
				"L3.PriceChangeAmendEmitsModifyWithNewPrice:"+
				"L3.PerSymbolSeqsAreIndependent:"+
				"L3.AccountHashIsPseudonymAndDeterministic:"+
				"L3.EveryEventWalSeqPointsAtAMatchingRow:"+
				"L3.JournalFreeReplayReproducesIdenticalStream"),
	)
}

// --- Task 17.3.2: L3 WS distribution + snapshot ----------------------------------

func ckP17L3Distribution(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/marketdata/l3.go",
			"services/internal/marketdata/l3_snapshot.go",
			"services/internal/api/handlers_l3.go"),
		structural(env, "services/internal/gateway/routes_v1.go",
			"/ws/v1/l3/{symbol}", "market-data/l3-snapshot"),
		gotest(p17Md, "TestL3Server_FiveSubscriptionCap|TestL3PremiumTierGate|"+
			"TestL3Hub_NoConflationEveryEventForwarded|"+
			"TestL3Hub_ReplayWithinHorizon|TestL3Conn_ReplayGateOrdering|"+
			"TestL3SnapshotReader_RebuildsRestingBook|"+
			"TestL3SnapshotReader_SnapshotPlusTailReplay|"+
			"TestL3SnapshotReader_CursorPagination|"+
			"TestL3SnapshotReader_TooLargeCeiling|"+
			"TestL3SnapshotReader_StalenessGate|TestL3SnapshotReader_FailClosed"),
		gotest(p17Ipc, "TestL3OrderEvent_RoundTrip|"+
			"TestL3OrderEvent_RejectsContractViolations|"+
			"TestL3OrderEvent_MalformedUnion"),
	)
}

// --- Task 17.3.3: market-abuse surveillance --------------------------------------

func ckP17Surveillance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"services/internal/surveillance/signals.go",
			"services/internal/db/migrations/029_create_surveillance_signals.up.sql"),
		structural(env, "services/internal/db/migrations/029_create_surveillance_signals.up.sql",
			"surveillance_signals", "signal_type", "evidence", "dedup_key"),
		gotest(p17Surv, "TestEngine_Spoofing|"+
			"TestEngine_Spoofing_RestedOrdersNotFlagged|TestEngine_Layering|"+
			"TestEngine_WashTrading|TestEngine_MomentumIgnition|"+
			"TestEngine_FrontRunning|TestEngine_MarkingTheClose|"+
			"TestEngine_InsiderDealing|TestEngine_DedupKeyDeterminism|"+
			"TestSurveillanceSignalsPgIntegration"),
	)
}

// --- Task 17.3.4: gap recovery + overrun throttling --------------------------------

func ckP17GapRecovery(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		structural(env, "services/internal/errs/codes.go",
			"L3_SEQUENCE_GAP_DETECTED", "L3_CONSUMER_OVERRUN",
			"L3_SNAPSHOT_TOO_LARGE"),
		gotest(p17Md, "TestL3Hub_ReplayGapTooLarge|"+
			"TestL3Hub_FeedGapMarksMirrorSuspect|"+
			"TestL3Conn_OutboxLagOverrun|TestL3Conn_PendingOverflowOverruns"),
		gtest("test_l3",
			"L3.DropConsumesSeqForGapDetection:"+
				"L3.HiddenOrderEmitsHiddenFlagOnAddAndFill:"+
				"L3.PegRepriceEmitsModify:"+
				"L3.JournalFreeEngineStillPublishesWithVirtualSeqs:"+
				"L3.IocRemainderCancelCarriesReason4"),
	)
}
