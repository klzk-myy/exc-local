// Command route-dump emits the central route registry (Task 5.3.7,
// services/internal/gateway/routes_v1.go SeedRoutes) as JSON on stdout.
//
// Task 13.3.5 (penetration-testing prep): the attack-surface document
// docs/security/attack-surface.md is generated from this output by
// scripts/security/gen-attack-surface.sh — keeping the documented
// surface mechanically derived from the real registry instead of a
// hand-maintained copy that drifts.
//
// Usage: route-dump            # JSON array of Route on stdout
//
//	route-dump -summary   # one-line counts by status/tier (stderr-free)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"exchange/internal/gateway"
)

func main() {
	summary := flag.Bool("summary", false, "print count summary instead of the route array")
	flag.Parse()

	routes := gateway.SeedRoutes()
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})

	if *summary {
		byStatus := map[string]int{}
		byTier := map[string]int{}
		for _, r := range routes {
			byStatus[string(r.Status)]++
			byTier[r.RateTier]++
		}
		fmt.Printf("routes=%d status=%v tiers=%v\n", len(routes), byStatus, byTier)
		return
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(routes); err != nil {
		fmt.Fprintf(os.Stderr, "route-dump: %v\n", err)
		os.Exit(1)
	}
}
