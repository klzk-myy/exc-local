//go:build race

package risk

// raceDetectorOn is true under -race: instrumentation inflates
// wall-clock timings, so latency budgets relax (they still assert —
// catching gross regressions — just not at production bounds).
const raceDetectorOn = true
