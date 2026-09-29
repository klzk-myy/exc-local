// Command exchange is the operator CLI for the exchange suite.
//
// Task 1.3.8 scope: audit hash-chain operations —
//
//	exchange verify-audit --date=YYYY-MM-DD   recompute the chain genesis→end of date
//	exchange merkle --date=YYYY-MM-DD         compute+store that day's Merkle root
//	exchange merkle --run-daily               00:10 UTC job body (previous UTC day)
//	exchange audit-append                     dev/operator fixture driving audit.AppendAuto
//	exchange seal-tax-pii [--restore]         migration-210 PII backfill (seal
//	                                        legacy tin/fields; --restore for down)
//
// Exit codes: 0 clean/success, 1 operational error, 2 integrity violation
// detected by verify-audit (kept distinct so automation can alert on 2
// specifically, per the AUDIT_CHAIN_BROKEN L0 semantics).
package main

import (
	"errors"
	"fmt"
	"os"
)

// errIntegrity marks a verification failure (distinct from an operational
// error): the CLI ran correctly and found a tampered chain.
var errIntegrity = errors.New("integrity violations detected")

func main() {
	if err := rootCmd.Execute(); err != nil {
		if errors.Is(err, errIntegrity) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "exchange: %v\n", err)
		os.Exit(1)
	}
}
