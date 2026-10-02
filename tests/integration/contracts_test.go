// Contract-registry integrity — the suite's own guarantees:
//  1. every §24 criterion is present exactly once (live count 419)
//  2. stable test ids survive unchanged from traceability.json
//  3. committed registry carries only PLANNED|EXECUTABLE — never a
//     pre-claimed PASS/FAIL (those live in the coverage report)
//  4. every bound leg names a real test/binary this suite runs
//  5. contracts.json matches what contractgen would emit (freshness)
package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"exchange-integration/itest"
)

func TestContractRegistryIntegrity(t *testing.T) {
	tf, err := itest.LoadTrace(env.TracePath)
	if err != nil {
		t.Fatalf("traceability: %v", err)
	}
	rows := itest.Contracts(tf, itest.CriterionBindings)

	if got, want := len(rows), tf.ExpectedCriteria; got != want {
		t.Fatalf("contract count %d != expected_criteria %d", got, want)
	}
	if tf.ExpectedCriteria != 419 {
		t.Fatalf("expected_criteria=%d — registry source drifted", tf.ExpectedCriteria)
	}

	seen := map[int]bool{}
	var bound, inWindow int
	bindableWindow := 0
	for _, c := range rows {
		if seen[c.CriterionID] {
			t.Fatalf("duplicate criterion %d", c.CriterionID)
		}
		seen[c.CriterionID] = true
		if c.CriterionID < 1 || c.CriterionID > tf.ExpectedCriteria {
			t.Fatalf("criterion id %d out of range", c.CriterionID)
		}
		if c.StableTestContract == "" {
			t.Fatalf("criterion %d: empty stable_test_id", c.CriterionID)
		}
		if len(c.PhaseAC) == 0 {
			t.Fatalf("criterion %d: no phase AC reference", c.CriterionID)
		}
		switch c.Status {
		case "PLANNED", "EXECUTABLE":
		default:
			t.Fatalf("criterion %d: committed status %q — must be PLANNED or EXECUTABLE",
				c.CriterionID, c.Status)
		}
		if itest.PhaseWindow(c.OwnerPhaseTags) {
			bindableWindow++
		}
		if len(c.Bindings) > 0 {
			bound++
			if itest.PhaseWindow(c.OwnerPhaseTags) {
				inWindow++
			}
		}
	}
	t.Logf("contracts=%d bound=%d (phase1-7 bound=%d of %d window criteria)",
		len(rows), bound, inWindow, bindableWindow)

	// Every §24 id 1..419 must be present (contiguity per the trace).
	for i := 1; i <= tf.ExpectedCriteria; i++ {
		if !seen[i] {
			t.Fatalf("criterion %d missing from registry", i)
		}
	}
}

// TestBindingReferences verifies each bound leg resolves to something
// this suite can actually execute — e2e legs must name a real test,
// gtest binaries must exist, gotest packages must exist on disk.
func TestBindingReferences(t *testing.T) {
	e2eTests := map[string]bool{
		"TestE2E_StackReady": true, "TestE2E_HMACAuth": true,
		"TestE2E_APIKeyIPAllowlist": true, "TestE2E_OrderPipeline": true,
		"TestE2E_IdempotentSubmit": true, "TestE2E_OrderAmendAudit": true,
		"TestE2E_MassCancel": true, "TestE2E_BatchOrders": true,
		"TestE2E_CancelReplace": true, "TestE2E_RateLimit": true,
		"TestE2E_RateLimitHeaders": true, "TestE2E_IPBanEscalation": true,
		"TestE2E_DegradationModes": true, "TestE2E_FrozenAccount": true,
		"TestE2E_TestReset": true, "TestE2E_Instruments": true,
		"TestE2E_OpenAPI": true, "TestE2E_DeveloperPortal": true,
		"TestE2E_InternalTransfer": true, "TestE2E_ErrorEnvelope": true,
		"TestE2E_ListEnvelope": true, "TestE2E_ServerTime": true,
		"TestE2E_VenueInfo": true, "TestE2E_Announcements": true,
		"TestE2E_RBAC": true, "TestE2E_ManualLiquidation": true,
		"TestE2E_SupportTickets": true, "TestE2E_DeprecationHeaders": true,
		"TestE2E_AuditVerify": true, "TestE2E_RestLatency": true,
		"TestE2E_WSAuth": true, "TestE2E_OrderValidation": true,
		"TestE2E_Ed25519Auth": true, "TestE2E_QuoteMarketOrders": true,
		"TestE2E_EngineCrashRecovery": true,
	}
	for id, bs := range itest.CriterionBindings {
		for _, b := range bs {
			switch b.Kind {
			case itest.BindE2E:
				if !e2eTests[b.Test] {
					t.Errorf("criterion %d: e2e binding names unknown test %q", id, b.Test)
				}
			case itest.BindGTest:
				p := filepath.Join(env.CoreBuild, b.Binary)
				if _, err := os.Stat(p); err != nil {
					t.Errorf("criterion %d: gtest binary %s absent", id, b.Binary)
				}
			case itest.BindGoTest:
				p := filepath.Join(env.ServicesDir, filepath.Clean(b.Pkg))
				if _, err := os.Stat(p); err != nil {
					t.Errorf("criterion %d: gotest package %s absent", id, b.Pkg)
				}
			case itest.BindBin:
				switch b.Binary {
				case "faultinject":
					if _, err := os.Stat(filepath.Join(env.Root, "ci", "fault-injection", "run.sh")); err != nil {
						t.Errorf("criterion %d: faultinject harness absent", id)
					}
				case "snapbench":
					if _, err := os.Stat(filepath.Join(env.CoreBuild, "snapbench")); err != nil {
						t.Errorf("criterion %d: snapbench absent", id)
					}
				case "dr-drill-runner":
					if _, err := os.Stat(filepath.Join(env.Root, "scripts", "ops", "dr_drill_runner.sh")); err != nil {
						t.Errorf("criterion %d: dr_drill_runner.sh absent", id)
					}
				case "bluegreen-gate":
					if _, err := os.Stat(filepath.Join(env.Root, "deploy", "scripts", "tests", "bluegreen_gate_test.sh")); err != nil {
						t.Errorf("criterion %d: bluegreen_gate_test.sh absent", id)
					}
				case "pentest":
					if _, err := os.Stat(filepath.Join(env.Root, "tests", "pentest", "run.sh")); err != nil {
						t.Errorf("criterion %d: pentest run.sh absent", id)
					}
				default:
					t.Errorf("criterion %d: unhandled bin binding %q", id, b.Binary)
				}
			case itest.BindVitest:
				p := filepath.Join(env.FrontendDir, filepath.Clean(b.Pkg))
				if _, err := os.Stat(p); err != nil {
					t.Errorf("criterion %d: vitest spec %s absent", id, b.Pkg)
				}
			case itest.BindCTest, itest.BindCompose:
				// ctest resolves at run time; compose legs are env-gated.
			default:
				t.Errorf("criterion %d: unknown binding kind %q", id, b.Kind)
			}
		}
	}
}

// TestContractsJSONFresh regenerates the registry and compares it to the
// committed file — drift means contractgen wasn't run after an edit.
func TestContractsJSONFresh(t *testing.T) {
	tf, err := itest.LoadTrace(env.TracePath)
	if err != nil {
		t.Fatalf("traceability: %v", err)
	}
	want := itest.Contracts(tf, itest.CriterionBindings)
	wantJSON, err := json.MarshalIndent(map[string]any{
		"tool":      "exchange-integration/contractgen",
		"version":   "1.0.0",
		"source":    "tests/spec/traceability.json (read-only) + itest/bindings.go",
		"generated": "see coverage report for runtime statuses",
		"contracts": want,
		"expected":  tf.ExpectedCriteria,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(env.Root, "tests", "integration", "contracts.json"))
	if err != nil {
		t.Fatalf("contracts.json unreadable — run `go run ./cmd/contractgen -write`: %v", err)
	}
	if string(got) != string(wantJSON) {
		t.Fatal("contracts.json is stale — run `go run ./cmd/contractgen -write`")
	}
}

// TestStableIDsMatchTrace pins every contract's stable test id to its
// source row — the §24 trace must survive the projection verbatim.
func TestStableIDsMatchTrace(t *testing.T) {
	tf, err := itest.LoadTrace(env.TracePath)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int]string{}
	for _, c := range tf.Criteria {
		byID[c.ID] = c.StableTestContract
	}
	for _, c := range itest.Contracts(tf, itest.CriterionBindings) {
		if byID[c.CriterionID] != c.StableTestContract {
			t.Fatalf("criterion %d: stable id %q != trace %q",
				c.CriterionID, c.StableTestContract, byID[c.CriterionID])
		}
	}
	var ids []int
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	t.Logf("verified %d stable test ids", len(ids))
}
