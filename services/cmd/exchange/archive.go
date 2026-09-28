package main

// Task 4.3.2/4.3.3/4.3.7 CLI wiring:
//
//	exchange archive-status --shard=0                     (Task 4.3.2)
//	exchange archive-wal --wal-dir=/wal/0 --shard=0       (sealed-segment archive+trim)
//	exchange archive-lifecycle --shard=0                  (90d→GLACIER transition)
//	exchange replay-from-archive --symbol=EUR/USD \
//	    --from=YYYY-MM-DD --to=YYYY-MM-DD [--shard=0]     (Task 4.3.3)
//	exchange restore-partition-archive --partition=NAME   (Task 4.3.7)
//
// S3 configuration is env-driven (objectstore.ConfigFromEnv):
//   EXC_S3_ENDPOINT          devs3/MinIO/R2 base endpoint (empty = real AWS)
//   EXC_S3_REGION            default us-east-1
//   EXC_S3_ACCESS_KEY_ID / EXC_S3_SECRET_ACCESS_KEY
//   EXC_S3_WAL_BUCKET        default exchange-wal
//   EXC_S3_ARCHIVE_BUCKET    default exchange-partition-archive
//   EXC_POSTGRES_DSN         overrides config postgres.dsn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exchange/internal/archiver"
	"exchange/internal/objectstore"
	"exchange/internal/recovery"
)

func s3Client(ctx context.Context, bucket string) (*objectstore.S3Client, error) {
	cfg := objectstore.ConfigFromEnv(bucket, os.Getenv)
	if cfg.Endpoint != "" {
		return objectstore.NewDev(ctx, cfg)
	}
	return objectstore.NewAWS(ctx, cfg)
}

func walClient(ctx context.Context) (*objectstore.S3Client, error) {
	bucket := os.Getenv("EXC_S3_WAL_BUCKET")
	if bucket == "" {
		bucket = recovery.DefaultWalBucket
	}
	return s3Client(ctx, bucket)
}

func archiveBucketClient(ctx context.Context) (*objectstore.S3Client, error) {
	bucket := os.Getenv("EXC_S3_ARCHIVE_BUCKET")
	if bucket == "" {
		bucket = archiver.DefaultPartitionBucket
	}
	return s3Client(ctx, bucket)
}

// --- archive-status (Task 4.3.2 #5) --------------------------------------

var archiveStatusCmd = &cobra.Command{
	Use:   "archive-status --shard=N",
	Short: "Show archived WAL segments and sizes for a shard",
	Long: `archive-status reads s3://{wal-bucket}/{shard}/index.json and
cross-checks each entry with HEAD, printing segment, date, size, seq span,
storage class (STANDARD/GLACIER) and presence. Missing objects are flagged
— an index/object mismatch is an integrity problem, exit 1.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		client, err := walClient(ctx)
		if err != nil {
			return err
		}
		svc := recovery.NewArchiveService(client)
		shard64, _ := cmd.Flags().GetUint("shard")
		rows, err := svc.Status(ctx, uint16(shard64))
		if err != nil {
			return err
		}
		if js, _ := cmd.Flags().GetBool("json"); js {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SEGMENT\tDATE\tSIZE\tSEQ-FIRST\tSEQ-LAST\tCLASS\tGLACIER-AT\tPRESENT")
		var total int64
		missing := 0
		for _, r := range rows {
			total += r.SizeBytes
			if !r.Present {
				missing++
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%v\n",
				r.Segment, r.Date, r.SizeBytes, r.FirstSeq, r.LastSeq,
				r.StorageClass, orDash(r.GlacierTransitionAt), r.Present)
		}
		w.Flush()
		fmt.Printf("archive-status: shard=%d segments=%d total_bytes=%d bucket=%s\n",
			shard64, len(rows), total, client.Bucket())
		if missing > 0 {
			return fmt.Errorf("archive-status: %d indexed segments missing from S3", missing)
		}
		return nil
	},
}

// --- archive-wal: sealed-segment upload+trim (Task 4.3.2 #1-2) -----------

var archiveWalCmd = &cobra.Command{
	Use:   "archive-wal --wal-dir=DIR --shard=N",
	Short: "Archive sealed WAL segments to S3, then trim local copies",
	Long: `archive-wal uploads every *.wal segment in --wal-dir except the
newest (the one the engine may still be appending to) to
s3://{bucket}/{shard}/{date}/{segment}, verifies the returned ETag against
md5(local bytes), updates index.json, and only then deletes the local
file. Any failure aborts before the local trim — zero-loss guard.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		dir, _ := cmd.Flags().GetString("wal-dir")
		if dir == "" {
			return fmt.Errorf("--wal-dir is required")
		}
		shard64, _ := cmd.Flags().GetUint("shard")
		all, _ := cmd.Flags().GetBool("include-active")

		client, err := walClient(ctx)
		if err != nil {
			return err
		}
		svc := recovery.NewArchiveService(client)
		if all {
			// Explicit opt-in: archive even the newest segment (operator
			// guarantee the engine is stopped for this shard).
			ents, rerr := os.ReadDir(dir)
			if rerr != nil {
				return rerr
			}
			var done int
			for _, e := range ents {
				if e.IsDir() || len(e.Name()) < 4 ||
					e.Name()[len(e.Name())-4:] != ".wal" {
					continue
				}
				m, aerr := svc.TrimSegment(ctx, uint16(shard64), dir+string(os.PathSeparator)+e.Name())
				if aerr != nil {
					return fmt.Errorf("archived %d, then: %w", done, aerr)
				}
				fmt.Printf("archived %s -> %s (%d bytes, etag %s)\n",
					m.Segment, m.Key, m.SizeBytes, m.ETag)
				done++
			}
			fmt.Printf("archive-wal: shard=%d archived=%d (include-active)\n", shard64, done)
			return nil
		}
		metas, err := svc.ArchiveDir(ctx, dir, uint16(shard64))
		if err != nil {
			return err
		}
		for _, m := range metas {
			fmt.Printf("archived %s -> %s (%d bytes, etag %s)\n",
				m.Segment, m.Key, m.SizeBytes, m.ETag)
		}
		fmt.Printf("archive-wal: shard=%d archived=%d (active segment left local)\n",
			shard64, len(metas))
		return nil
	},
}

// --- archive-lifecycle: modelled 90d→GLACIER (Task 4.3.2 #4) -------------

var archiveLifecycleCmd = &cobra.Command{
	Use:   "archive-lifecycle --shard=N",
	Short: "Apply the 90-day S3→Glacier transition to the archive index",
	Long: `archive-lifecycle flips index entries older than 90 days to
storage_class=GLACIER and republishes lifecycle.json — the modelled half
of the bucket lifecycle policy (devs3 cannot perform real transitions;
the index carries the state for status/audit views).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		shard64, _ := cmd.Flags().GetUint("shard")
		client, err := walClient(ctx)
		if err != nil {
			return err
		}
		svc := recovery.NewArchiveService(client)
		n, err := svc.ApplyLifecycle(ctx, uint16(shard64))
		if err != nil {
			return err
		}
		fmt.Printf("archive-lifecycle: shard=%d transitioned=%d rule=%dd->GLACIER\n",
			shard64, n, recovery.DefaultRetentionDays)
		return nil
	},
}

// --- replay-from-archive (Task 4.3.3) ------------------------------------

var replayCmd = &cobra.Command{
	Use:   "replay-from-archive --symbol=EUR/USD --from=YYYY-MM-DD --to=YYYY-MM-DD",
	Short: "Replay archived WAL segments into a read-only reconstruction",
	Long: `replay-from-archive downloads the shard's archived WAL segments
covering --from..--to and replays them read-only: trade history, book
snapshots at --snapshot-interval (default 5m), and per-account P&L
deltas. --symbol resolves to an instrument_id via Postgres; use
--instrument-id to skip the DB lookup. Output is JSON (--json) or a
text summary. Read-only — never touches live trading state.

Fail-closed: missing objects, corrupt index/segments, or shard-seq gaps
abort the replay (seq gaps downgradeable via --allow-seq-gaps).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		from, err := parseDate(must(cmd, "from"))
		if err != nil {
			return err
		}
		to, err := parseDate(must(cmd, "to"))
		if err != nil {
			return err
		}
		var instID uint32
		var symbols map[uint32]string
		if s, _ := cmd.Flags().GetString("symbol"); s != "" {
			pool, err := openPool(ctx)
			if err != nil {
				return fmt.Errorf("--symbol requires postgres (%v); use --instrument-id", err)
			}
			id, m, err := recovery.ResolveInstrumentID(ctx, pool, s)
			pool.Close()
			if err != nil {
				return err
			}
			instID, symbols = id, m
		}
		if id, _ := cmd.Flags().GetUint("instrument-id"); id != 0 {
			instID = uint32(id)
		}

		client, err := walClient(ctx)
		if err != nil {
			return err
		}
		iv, _ := cmd.Flags().GetDuration("snapshot-interval")
		shard64, _ := cmd.Flags().GetUint("shard")
		allowGaps, _ := cmd.Flags().GetBool("allow-seq-gaps")
		rep, err := recovery.NewReplayer(client).Replay(ctx, recovery.ReplayOptions{
			Shard:          uint16(shard64),
			InstrumentID:   instID,
			From:           from,
			To:             to,
			SnapshotEvery:  iv,
			AllowSeqGaps:   allowGaps,
			ResolveSymbols: symbols,
		})
		if err != nil {
			return err
		}
		if js, _ := cmd.Flags().GetBool("json"); js {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(rep)
		}
		fmt.Printf("replay-from-archive: shard=%d instrument=%d range=%s..%s\n",
			rep.Shard, rep.InstrumentID, rep.From, rep.To)
		fmt.Printf("segments=%d trades=%d snapshots=%d pnl_rows=%d missing_dates=%d\n",
			len(rep.Segments), len(rep.Trades), len(rep.Snapshots),
			len(rep.PnL), len(rep.MissingDates))
		for _, w := range rep.Warnings {
			fmt.Printf("warning: %s\n", w)
		}
		for _, g := range rep.SeqGaps {
			fmt.Printf("seq-gap: %s\n", g)
		}
		return nil
	},
}

// --- restore-partition-archive (Task 4.3.7 #5) ----------------------------

var restorePartitionCmd = &cobra.Command{
	Use:   "restore-partition-archive --partition=NAME [--into-schema=archive_restore]",
	Short: "Restore drill: download, verify, recreate, row-count parity",
	Long: `restore-partition-archive downloads the archived partition export
(zstd CSV + manifest) from the WORM bucket, verifies SHA-256 + ETag,
recreates the table under --into-schema, COPYs the rows back, and asserts
row-count parity against the manifest. Updates the archive log to
RESTORED on success.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		part := must(cmd, "partition")
		if part == "" {
			return fmt.Errorf("--partition is required")
		}
		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		client, err := archiveBucketClient(ctx)
		if err != nil {
			return err
		}
		a := archiver.New(pool, client)
		res, err := a.RestorePartition(ctx, part, must(cmd, "into-schema"))
		if err != nil {
			return err
		}
		fmt.Printf("restore-partition-archive: %s -> %s rows=%d manifest=%d parity=%v sha256=%s\n",
			res.Partition, res.IntoTable, res.LoadedRows, res.ManifestRows,
			res.Parity, res.ArchiveSHA256)
		return nil
	},
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func must(cmd *cobra.Command, name string) string {
	s, _ := cmd.Flags().GetString(name)
	return s
}

func init() {
	rootCmd.AddCommand(archiveStatusCmd, archiveWalCmd, archiveLifecycleCmd,
		replayCmd, restorePartitionCmd)

	archiveStatusCmd.Flags().Uint("shard", 0, "shard id")
	archiveStatusCmd.Flags().Bool("json", false, "emit JSON")

	archiveWalCmd.Flags().String("wal-dir", "", "shard WAL segment directory")
	archiveWalCmd.Flags().Uint("shard", 0, "shard id")
	archiveWalCmd.Flags().Bool("include-active", false,
		"also archive the newest segment (engine must be stopped)")
	_ = archiveWalCmd.MarkFlagRequired("wal-dir")

	archiveLifecycleCmd.Flags().Uint("shard", 0, "shard id")

	replayCmd.Flags().String("symbol", "", "instrument symbol, e.g. EUR/USD (resolves via postgres)")
	replayCmd.Flags().Uint("instrument-id", 0, "instrument id (skips DB lookup)")
	replayCmd.Flags().Uint("shard", 0, "shard id")
	replayCmd.Flags().String("from", "", "first UTC date YYYY-MM-DD")
	replayCmd.Flags().String("to", "", "last UTC date YYYY-MM-DD (inclusive)")
	replayCmd.Flags().Duration("snapshot-interval", 5*time.Minute, "book snapshot cadence (WAL time)")
	replayCmd.Flags().Bool("allow-seq-gaps", false, "warn instead of failing on shard-seq gaps")
	replayCmd.Flags().Bool("json", false, "emit full JSON report")
	_ = replayCmd.MarkFlagRequired("from")
	_ = replayCmd.MarkFlagRequired("to")

	restorePartitionCmd.Flags().String("partition", "", "partition name, e.g. trades_p2025_06")
	restorePartitionCmd.Flags().String("into-schema", "archive_restore",
		"schema to recreate the partition under")
	_ = restorePartitionCmd.MarkFlagRequired("partition")
}
