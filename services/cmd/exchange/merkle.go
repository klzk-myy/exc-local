package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"exchange/internal/audit"
)

var merkleCmd = &cobra.Command{
	Use:   "merkle (--date=YYYY-MM-DD | --run-daily)",
	Short: "Compute and store a daily audit Merkle root",
	Long: `merkle computes the SHA-256 Merkle root over one UTC day's
audit_hash_chain rows (ordered by sequence_num) and upserts it into
audit_merkle_roots.

  --date       compute for this UTC date
  --run-daily  compute for the previous UTC day — the body of the 00:10 UTC
               scheduled job (Phase-07 wires the scheduler)`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dateStr, _ := cmd.Flags().GetString("date")
		runDaily, _ := cmd.Flags().GetBool("run-daily")
		if (dateStr == "") == !runDaily {
			return fmt.Errorf("exactly one of --date=YYYY-MM-DD or --run-daily is required")
		}

		ctx := cmd.Context()
		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		var day time.Time
		var root string
		var n int
		if runDaily {
			day, root, n, err = audit.RunDailyMerkleJob(ctx, pool)
			if err != nil {
				return err
			}
		} else {
			day, err = parseDate(dateStr)
			if err != nil {
				return err
			}
			root, n, err = audit.ComputeMerkleRoot(ctx, pool, day)
			if err != nil {
				return err
			}
		}

		fmt.Printf("merkle: date=%s rows=%d root=%s stored\n",
			day.Format("2006-01-02"), n, root)
		return nil
	},
}

func init() {
	merkleCmd.Flags().String("date", "", "UTC date to compute (YYYY-MM-DD)")
	merkleCmd.Flags().Bool("run-daily", false, "compute the previous UTC day's root (00:10 UTC job body)")
}
