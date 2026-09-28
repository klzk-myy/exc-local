// Command contractgen regenerates tests/integration/contracts.json from
// tests/spec/traceability.json (read-only input) + itest.CriterionBindings.
//
// Regenerate after changing bindings or when traceability.json advances:
//
//	cd tests/integration && go run ./cmd/contractgen -write
//
// Without -write it diffs and exits non-zero on drift (CI contract).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"exchange-integration/itest"
)

func main() {
	write := flag.Bool("write", false, "rewrite contracts.json")
	flag.Parse()

	e := itest.DefaultEnv()
	tf, err := itest.LoadTrace(e.TracePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "contractgen:", err)
		os.Exit(2)
	}
	contracts := itest.Contracts(tf, itest.CriterionBindings)

	out := map[string]any{
		"tool":      "exchange-integration/contractgen",
		"version":   "1.0.0",
		"source":    "tests/spec/traceability.json (read-only) + itest/bindings.go",
		"generated": "see coverage report for runtime statuses",
		"contracts": contracts,
		"expected":  tf.ExpectedCriteria,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "contractgen:", err)
		os.Exit(2)
	}
	path := filepath.Join(e.Root, "tests", "integration", "contracts.json")
	if !*write {
		cur, err := os.ReadFile(path)
		if err != nil || string(cur) != string(b) {
			fmt.Fprintln(os.Stderr, "contractgen: contracts.json is stale — run with -write")
			os.Exit(1)
		}
		fmt.Println("contractgen: contracts.json up to date")
		return
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "contractgen:", err)
		os.Exit(2)
	}
	bound, planned := 0, 0
	for _, c := range contracts {
		if len(c.Bindings) > 0 {
			bound++
		} else {
			planned++
		}
	}
	fmt.Printf("contractgen: wrote %d contracts (%d bound/EXECUTABLE, %d PLANNED)\n",
		len(contracts), bound, planned)
}
