package main

// Phase-13 Tasks 13.3.7/13.3.8 — the daily scheduled job bodies.
//
//	exchange solvency-tree          — build + sign + publish the
//	                                  proof-of-reserves Merkle snapshot
//	                                  (deploy/crons/solvency-tree.sh,
//	                                  daily 22:00 UTC / NY close)
//	exchange api-key-expiry-sweep   — warn→revoke→restore pass over the
//	                                  90-day un-allowlisted-key policy
//	                                  (deploy/crons/api-key-expiry.sh)
//
// Both commands are idempotent and fail closed: a generation defect
// (negative liability, signing failure, DB error) exits non-zero and
// publishes nothing — cron wrappers page on the exit code.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"exchange/internal/auth"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/reconciliation"
)

var solvencyTreeCmd = &cobra.Command{
	Use:   "solvency-tree",
	Short: "Generate and publish the daily proof-of-reserves Merkle snapshot",
	Long: `solvency-tree extracts every client liability (balances × accounts,
zero rows included, negative rows abort), aggregates ACTIVE nostro
balances per currency, builds the salted-leaf Merkle tree, signs the
canonical payload (EXC_SOLVENCY_* env selects the signer — production
requires the cold-storage GPG key via EXC_SOLVENCY_GPG_FINGERPRINT;
dev/staging may use the labelled DEV-HMAC signer) and persists the
snapshot plus every inclusion proof atomically.

Scheduled at 22:00 UTC daily via deploy/crons/solvency-tree.sh.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		pool, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
		if err != nil {
			return err
		}
		defer pool.Close()

		store, err := reconciliation.NewSolvencyStore(pool)
		if err != nil {
			return err
		}
		signer, err := reconciliation.SignerFromEnv(os.Getenv, cfg.Environment)
		if err != nil {
			return err
		}
		svc, err := reconciliation.NewSolvencyService(store, store, signer)
		if err != nil {
			return err
		}
		snap, err := svc.Generate(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("solvency-tree: snapshot=%d leaves=%d root=%s solvent=%t signer=%s(%s)\n",
			snap.ID, snap.LeafCount, snap.MerkleRoot, snap.Solvent,
			snap.SignerKind, snap.SignerFingerprint)
		return nil
	},
}

var apiKeyExpirySweepCmd = &cobra.Command{
	Use:   "api-key-expiry-sweep",
	Short: "Run one warn→revoke→restore pass of the API-key privilege-expiry policy",
	Long: `api-key-expiry-sweep runs the Task 13.3.8 policy: keys older than
90 days without an ip_allowlist lose trade/transfer (the row and its
remaining scopes survive); holders are warned 7 days ahead; a key that
later gains an allowlist is restored verbatim. Idempotent — safe to run
daily via deploy/crons/api-key-expiry.sh and from the gateway's hourly
in-process sweeper.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		pool, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
		if err != nil {
			return err
		}
		defer pool.Close()

		keys, err := auth.NewKeyStore(pool, nil)
		if err != nil {
			return err
		}
		// nil sink: the cron path is the scheduled-of-record; the
		// gateway's sweeper carries the wired security_alert notifier.
		policy, err := auth.NewKeyExpiryPolicy(keys, nil)
		if err != nil {
			return err
		}
		res, err := policy.Sweep(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("api-key-expiry-sweep: warned=%d revoked=%d restored=%d\n",
			res.Warned, res.Revoked, res.Restored)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(solvencyTreeCmd, apiKeyExpirySweepCmd)
}
