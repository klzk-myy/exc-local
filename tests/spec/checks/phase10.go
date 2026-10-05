package checks

import (
	"context"
	"os"
	"os/exec"
	"strings"

	spec "exchange-testspec/spec"
)

// Phase-10 trader-UI checkpoints.
//
// Frontend tasks bind to the real Vitest suite (component + unit tests
// in src/features/<name>/ and src/lib/<name>/) plus artifact checks
// (scaffold files, CSP headers, bundle budget, generated validators).
// Each checkpoint runs only its owning feature's tests so a failure
// localizes to the task. Browser-e2e (Playwright) smoke is bound into
// the scaffold check; live-backend e2e legs are honest-open AC rows.
func registerPhase10(r *spec.Registry) {
	r.Register("P10-T10.3.1-C1", ckP10Scaffold, "React 18 + TypeScript + Vite scaffold")
	r.Register("P10-T10.3.2-C1", ckP10OrderBook, "virtualized order book")
	r.Register("P10-T10.3.3-C1", ckP10OrderEntry, "order entry with validation")
	r.Register("P10-T10.3.4-C1", ckP10Charts, "TradingView Lightweight Charts")
	r.Register("P10-T10.3.5-C1", ckP10Portfolio, "real-time positions + balances")
	r.Register("P10-T10.3.6-C1", ckP10Admin, "RBAC-gated admin dashboard")
	r.Register("P10-T10.3.7-C1", ckP10AdvancedOrders, "advanced order UI + sub-account switcher")
	r.Register("P10-T10.3.8-C1", ckP10Calculator, "position/margin calculator uses live marks")
	r.Register("P10-T10.3.9-C1", ckP10LitePro, "persistent Lite/Pro modes")
	r.Register("P10-T10.3.10-C1", ckP10QuickActions, "quick actions with projected-execution confirmation")
	r.Register("P10-T10.3.11-C1", ckP10Sliders, "percentage sizing respects free margin + lot filters")
	r.Register("P10-T10.3.12-C1", ckP10DepthChart, "interactive depth chart on canonical L2")
	r.Register("P10-T10.3.13-C1", ckP10ADL, "ADL rank visible with risk explanation")
	r.Register("P10-T10.3.14-C1", ckP10Workspace, "save/restore/reset accessible trading layouts")
	r.Register("P10-T10.3.15-C1", ckP10ChartOverlays, "chart overlays inspect/modify/cancel")
	r.Register("P10-T10.3.16-C1", ckP10Backtesting, "technical indicators + strategy backtesting")
	r.Register("P10-T10.3.17-C1", ckP10Discovery, "market discovery, watchlists, rate alerts")
	r.Register("P10-T10.3.18-C1", ckP10Performance, "client performance dashboard")
	r.Register("P10-T10.3.19-C1", ckP10Reconnect, "network reconnection, stale indicators, optimistic rollback")
	r.Register("P10-T10.3.20-C1", ckP10EnvFleet, "environment switcher, fleet pages, ops board")
	r.Register("P10-T10.3.21-C1", ckP10Auth, "authentication, registration, session screens")
	r.Register("P10-T10.3.22-C1", ckP10Settings, "account settings + security center")
	r.Register("P10-T10.3.23-C1", ckP10Funding, "funding + transfers screens")
	r.Register("P10-T10.3.24-C1", ckP10KYC, "KYC submission + status tracker")
	r.Register("P10-T10.3.25-C1", ckP10Support, "support tickets + help center")
	r.Register("P10-T10.3.26-C1", ckP10CopyGrid, "copy-trading browser + grid-bot wizard")
	r.Register("P10-T10.3.27-C1", ckP10History, "order history, algo mgmt, OPO lists, dead-man, WS channels")
	r.Register("P10-T10.3.28-C1", ckP10Reports, "reports/statements/transparency downloads + announcements")
	r.Register("P10-T10.3.29-C1", ckP10InputHelpers, "centralized input-helper framework")
}

// vitest runs the frontend suite scoped to the given test paths
// (files or directories, relative to frontend/src or repo paths as
// vitest filters accept).
func vitest(paths ...string) step {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		args := append([]string{"vitest", "run"}, paths...)
		c := exec.CommandContext(ctx, "npx", args...)
		c.Dir = env.Path("frontend")
		c.Env = os.Environ()
		out, err := c.CombinedOutput()
		s := string(out)
		if err != nil {
			return spec.Failf("vitest %s failed: %v\n%s",
				strings.Join(paths, " "), err, tailstr(s, 30))
		}
		if strings.Contains(s, "No test files found") {
			return spec.Failf("vitest %s matched no test files", strings.Join(paths, " "))
		}
		return spec.Pass("vitest " + strings.Join(paths, " ") + " passed")
	}
}

// npmScript runs an npm script inside frontend/.
func npmScript(name string) step {
	return func(ctx context.Context, env *spec.Env) spec.Result {
		c := exec.CommandContext(ctx, "npm", "run", name)
		c.Dir = env.Path("frontend")
		c.Env = os.Environ()
		out, err := c.CombinedOutput()
		if err != nil {
			return spec.Failf("npm run %s failed: %v\n%s", name, err, tailstr(string(out), 30))
		}
		return spec.Pass("npm run " + name + " passed")
	}
}

func ckP10Scaffold(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/package.json",
			"frontend/vite.config.ts",
			"frontend/tsconfig.json",
			"frontend/eslint.config.js",
			"frontend/index.html",
			"frontend/public/_headers",
			"frontend/README.md",
			"frontend/e2e/smoke.spec.ts",
		),
		structural(env, "frontend/tsconfig.json", `"strict"`),
		structural(env, "frontend/index.html", "Content-Security-Policy", `default-src 'self'`),
		structural(env, "frontend/public/_headers", "X-Frame-Options", "Referrer-Policy"),
		structural(env, ".github/workflows/ci.yml", "frontend"),
		vitest("src/app/manifest.test.tsx"),
	)
}

func ckP10OrderBook(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/order-book/OrderBook.tsx",
			"frontend/src/features/order-book/routes.ts",
		),
		vitest("src/features/order-book"),
	)
}

func ckP10OrderEntry(ctx context.Context, env *spec.Env) spec.Result {
	// The standalone order-entry feature was consolidated into the
	// workspace ticket (8a5b156): AdvancedOrderPanel is the single
	// Binance-style ticket covering every mounted kind, orderPayload.ts
	// carries field-level validation, and the workspace owns mount points.
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/advanced-orders/AdvancedOrderPanel.tsx",
			"frontend/src/features/advanced-orders/orderPayload.ts",
			"frontend/src/features/workspace/WorkspacePage.tsx",
		),
		vitest("src/features/advanced-orders"),
	)
}

func ckP10Charts(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/charts/TradingChart.tsx",
			"frontend/src/features/charts/udf.ts",
		),
		structural(env, "frontend/package.json", "lightweight-charts"),
		vitest("src/features/charts"),
	)
}

func ckP10Portfolio(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/portfolio/Portfolio.tsx"),
		vitest("src/features/portfolio"),
	)
}

func ckP10Admin(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/admin/AdminPage.tsx"),
		vitest("src/features/admin"),
	)
}

func ckP10AdvancedOrders(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/advanced-orders/AdvancedOrderPanel.tsx",
			"frontend/src/features/advanced-orders/SubAccountSwitcher.tsx",
		),
		vitest("src/features/advanced-orders/orderPayload.test.ts", "src/features/advanced-orders/components.test.tsx"),
	)
}

func ckP10Calculator(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/calculator/CalculatorPage.tsx"),
		vitest("src/features/calculator"),
	)
}

func ckP10LitePro(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/workspace/ModeToggle.tsx",
			"frontend/src/features/workspace/LiteDashboard.tsx",
			"frontend/src/features/workspace/liteMode.ts",
		),
		vitest("src/features/workspace"),
	)
}

func ckP10QuickActions(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/advanced-orders/ConfirmExecutionModal.tsx",
			"frontend/src/features/advanced-orders/PositionsPanel.tsx",
		),
		vitest("src/features/advanced-orders/PositionsPanel.test.tsx"),
	)
}

func ckP10Sliders(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/advanced-orders/PercentSlider.tsx"),
		vitest("src/features/advanced-orders/sizing.test.ts"),
	)
}

func ckP10DepthChart(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/depth-chart/DepthChart.tsx"),
		vitest("src/features/depth-chart"),
	)
}

func ckP10ADL(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/advanced-orders/AdlIndicator.tsx"),
		vitest("src/features/advanced-orders/components.test.tsx"),
	)
}

func ckP10Workspace(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/workspace/WorkspacePage.tsx",
			"frontend/src/features/workspace/layouts.ts",
		),
		vitest("src/features/workspace"),
	)
}

func ckP10ChartOverlays(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/charts/TradingChart.tsx",
			"frontend/src/features/charts/ChartOverlays.tsx",
			"frontend/src/features/advanced-orders/OrderInspectModal.tsx",
		),
		vitest("src/features/charts/TradingChart.test.tsx"),
	)
}

func ckP10Backtesting(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/backtesting/BacktestPage.tsx"),
		vitest(
			"src/lib/indicators",
			"src/lib/backtest",
			"src/features/backtesting",
		),
	)
}

func ckP10Discovery(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/discovery/DiscoveryPage.tsx"),
		vitest("src/features/discovery", "src/lib/alerts"),
	)
}

func ckP10Performance(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/performance/PerformancePage.tsx"),
		vitest("src/features/performance"),
	)
}

func ckP10Reconnect(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/lib/ws/machine.ts",
			"frontend/src/lib/ws/client.ts",
			"frontend/src/components/ConnectionBanner.tsx",
		),
		structural(env, "frontend/src/lib/ws/constants.ts", "RECONNECT_SCHEDULE"),
		vitest("src/lib/ws", "src/lib/market/pending.test.ts"),
	)
}

func ckP10EnvFleet(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/ops/OpsBoardPage.tsx"),
		vitest("src/features/ops", "src/lib/env"),
	)
}

func ckP10Auth(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/features/auth/LoginPage.tsx",
			"frontend/src/lib/auth/session.ts",
		),
		vitest("src/features/auth", "src/lib/auth"),
	)
}

func ckP10Settings(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/settings/ApiKeysPanel.tsx"),
		vitest("src/features/settings"),
	)
}

func ckP10Funding(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/funding/FundingPage.tsx"),
		vitest("src/features/funding"),
	)
}

func ckP10KYC(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/kyc/KycPage.tsx"),
		vitest("src/features/kyc"),
	)
}

func ckP10Support(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/support/SupportPage.tsx"),
		vitest("src/features/support"),
	)
}

func ckP10CopyGrid(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/copy-grid/CopyGridPage.tsx"),
		vitest("src/features/copy-grid"),
	)
}

func ckP10History(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/history/HistoryPage.tsx"),
		vitest("src/features/history"),
	)
}

func ckP10Reports(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env, "frontend/src/features/reports/DownloadCenter.tsx"),
		vitest("src/features/reports"),
	)
}

func ckP10InputHelpers(ctx context.Context, env *spec.Env) spec.Result {
	return seqf(ctx, env,
		files(env,
			"frontend/src/lib/input-helpers/fields.tsx",
			"frontend/src/lib/input-helpers/generated/route-contracts.ts",
			"frontend/scripts/gen-validators.mjs",
		),
		vitest("src/lib/input-helpers"),
		npmScript("gen:validators:check"),
	)
}
