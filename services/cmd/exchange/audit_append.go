package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"exchange/internal/audit"
)

// auditAppendCmd is a dev/operator fixture for appending a chain row —
// real emitters (Phase-21 audit middleware) call audit.Append inside their
// own transactions; this command exists so operators and tests can drive
// the real AppendAuto path end-to-end.
var auditAppendCmd = &cobra.Command{
	Use:   "audit-append --table=NAME [--record-id=N] --action=INSERT|UPDATE|DELETE [--payload=BYTES]",
	Short: "Append one row to the audit hash chain (dev/operator fixture)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		table, _ := cmd.Flags().GetString("table")
		action, _ := cmd.Flags().GetString("action")
		recordID, _ := cmd.Flags().GetInt64("record-id")
		hasRecordID := cmd.Flags().Changed("record-id")
		payload, _ := cmd.Flags().GetString("payload")

		ctx := cmd.Context()
		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		var rid *int64
		if hasRecordID {
			rid = &recordID
		}
		e, err := audit.AppendAuto(ctx, pool, table, rid, action, []byte(payload))
		if err != nil {
			return err
		}
		ridStr := "-"
		if e.RecordID != nil {
			ridStr = fmt.Sprintf("%d", *e.RecordID)
		}
		fmt.Printf("appended: id=%d seq=%d table=%s record_id=%s action=%s\n  payload_hash=%s\n  prev_hash=%s\n",
			e.ID, e.SequenceNum, e.TableName, ridStr, e.Action,
			e.PayloadHash, e.PrevHash)
		return nil
	},
}

func init() {
	auditAppendCmd.Flags().String("table", "", "audited table name (required)")
	auditAppendCmd.Flags().Int64("record-id", 0, "audited record id (omit flag for NULL)")
	auditAppendCmd.Flags().String("action", "", "INSERT|UPDATE|DELETE (required)")
	auditAppendCmd.Flags().String("payload", "", "canonical audited-record bytes (optional; pass empty for self-verifiable rows)")
	_ = auditAppendCmd.MarkFlagRequired("table")
	_ = auditAppendCmd.MarkFlagRequired("action")
	rootCmd.AddCommand(auditAppendCmd)
}
