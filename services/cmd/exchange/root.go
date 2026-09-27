package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"exchange/internal/config"
	"exchange/internal/db"
)

var rootCmd = &cobra.Command{
	Use:   "exchange",
	Short: "exchange operator CLI",
	Long: `exchange is the administrative operator CLI for the exchange suite.

Subcommands here are the operator/runbook paths referenced by the spec
(e.g. "exchange:verify-audit" in spec §5.8) and Phase-07 scheduler jobs.`,
	SilenceUsage:  true, // usage on demand via --help, not on every error
	SilenceErrors: true, // main() owns error printing + exit codes
}

func init() {
	rootCmd.AddCommand(verifyAuditCmd, merkleCmd)
}

// parseDate parses a strict YYYY-MM-DD UTC calendar date.
func parseDate(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --date %q: want YYYY-MM-DD", s)
	}
	return t, nil
}

// openPool loads config (defaults + config.yaml + EXC_* env) and opens the
// primary Postgres pool. Caller closes.
func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
}
