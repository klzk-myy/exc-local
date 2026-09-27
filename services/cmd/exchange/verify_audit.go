package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"exchange/internal/audit"
)

var verifyAuditCmd = &cobra.Command{
	Use:   "verify-audit --date=YYYY-MM-DD",
	Short: "Recompute and verify the audit hash chain through a date",
	Long: `verify-audit replays audit_hash_chain from genesis through the end of
--date (UTC), recomputing payload_hash and prev_hash for every retained row
and checking sequence continuity and the day's stored Merkle root when one
exists. Any mismatch prints the offending day + sequence_num and exits 2.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dateStr, _ := cmd.Flags().GetString("date")
		if dateStr == "" {
			return fmt.Errorf("--date=YYYY-MM-DD is required")
		}
		date, err := parseDate(dateStr)
		if err != nil {
			return err
		}

		ctx := cmd.Context()
		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		// Full chain from genesis through end of the target day.
		rep, err := audit.VerifyThrough(ctx, pool, date.AddDate(0, 0, 1), nil)
		if err != nil {
			return err
		}

		fmt.Printf("verify-audit: date=%s rows_checked=%d\n",
			date.Format("2006-01-02"), rep.RowsChecked)
		for _, v := range rep.Violations {
			fmt.Printf("violation: %s\n", v)
		}

		// Day-level anchor: if a stored root exists for --date it must
		// equal the recomputed tree (catches tamper that preserves the
		// internal chain, e.g. rewritten payload_hash on the tail row).
		mv, recomputed, stored, err := audit.VerifyDayMerkle(ctx, pool, date)
		if err != nil {
			return err
		}
		switch {
		case mv != nil:
			fmt.Printf("violation: %s\n", mv)
		case stored:
			fmt.Printf("merkle: date=%s root=%s (stored, verified)\n",
				date.Format("2006-01-02"), recomputed)
		default:
			fmt.Printf("merkle: date=%s root=%s (no stored root for date)\n",
				date.Format("2006-01-02"), recomputed)
		}

		if !rep.OK() || mv != nil {
			fmt.Println("verify-audit: FAILED")
			return errIntegrity
		}
		fmt.Println("verify-audit: OK")
		return nil
	},
}

func init() {
	verifyAuditCmd.Flags().String("date", "", "UTC date through which to verify (YYYY-MM-DD)")
	_ = verifyAuditCmd.MarkFlagRequired("date")
}
