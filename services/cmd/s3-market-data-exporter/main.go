// Command s3-market-data-exporter runs the daily 01:00 UTC ClickHouse→S3
// market-data export (Task 4.3.8, spec §16.1 / §24 #292): per-symbol ZIP
// archives of trades, aggTrades, 1m klines and top-of-book snapshots plus a
// SHA256 manifest, then a completion event for the Phase-23 catalog.
//
// Modes:
//
//	s3-market-data-exporter                 # daemon: run daily at 01:00 UTC
//	s3-market-data-exporter -once           # export yesterday (UTC) and exit
//	s3-market-data-exporter -once -date 2026-09-27
//
// Environment (EXC_ prefix keeps parity with config.Load conventions):
//
//	EXC_CH_URL            ClickHouse HTTP endpoint (default http://127.0.0.1:8123)
//	EXC_CH_USER / EXC_CH_PASSWORD / EXC_CH_DATABASE
//	EXC_EXPORT_DIR        local-dir upload root (dev backend; mutually
//	                      exclusive with the S3 env below)
//	EXC_S3_BUCKET         data bucket (default exchange-market-data; the public
//	                      hostname is data.{domain} per spec §16.1)
//	EXC_S3_ENDPOINT       S3 endpoint; AWS_ENDPOINT_URL honoured as fallback
//	EXC_S3_REGION         signing region (default eu-west-1)
//	EXC_S3_PREFIX         key prefix (default market-data)
//	AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
//	EXC_EXPORT_NATS_URL   e.g. nats://127.0.0.1:4222 — enables NATS notify
//	EXC_EXPORT_NATS_SUBJECT  default analytics.marketdata.export.completed
//	EXC_EXPORT_CALLBACK_URL  HTTP webhook alternative to NATS
//	EXC_EXPORT_SYMBOLS    CSV whitelist (default: discover from ClickHouse)
//	EXC_EXPORT_NO_BOOK=1  skip top-of-book snapshots
//	EXC_EXPORT_RUN_AT     HH:MM UTC run time (default 01:00)
//	EXC_LOGGING_LEVEL / EXC_LOGGING_FORMAT
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"exchange/internal/persistence"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "s3-market-data-exporter: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		once    = flag.Bool("once", false, "export one day and exit")
		dateStr = flag.String("date", "", "UTC day to export YYYY-MM-DD (default: yesterday)")
	)
	flag.Parse()

	level, _ := logging.ParseLevel(envOr("EXC_LOGGING_LEVEL", "info"))
	format := envOr("EXC_LOGGING_FORMAT", "json")
	log, err := logging.New(level, format)
	if err != nil {
		return err
	}

	// --- ClickHouse seam -----------------------------------------------------
	ch := &httpCHQuerier{
		Base:     envOr("EXC_CH_URL", "http://127.0.0.1:8123"),
		User:     os.Getenv("EXC_CH_USER"),
		Password: os.Getenv("EXC_CH_PASSWORD"),
		Database: os.Getenv("EXC_CH_DATABASE"),
	}

	// --- Upload seam ---------------------------------------------------------
	up, err := buildUploader()
	if err != nil {
		return err
	}

	// --- Completion-event seam (NATS subject or HTTP callback) ---------------
	nt, err := buildNotifier()
	if err != nil {
		return err
	}

	cfg := persistence.Config{Schema: persistence.DefaultSchema()}
	if v := os.Getenv("EXC_EXPORT_SYMBOLS"); v != "" {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.Symbols = append(cfg.Symbols, s)
			}
		}
	}
	cfg.Prefix = envOr("EXC_S3_PREFIX", "market-data")
	if os.Getenv("EXC_EXPORT_NO_BOOK") == "1" {
		off := false
		cfg.IncludeBook = &off
	}
	ex := persistence.NewExporter(ch, up, nt, cfg, log)

	ctx, stop := utils.SignalContext()
	defer stop()

	if *once {
		day, err := targetDay(*dateStr)
		if err != nil {
			return err
		}
		res, err := ex.ExportDay(ctx, day)
		if err != nil {
			return err
		}
		log.Info("export complete", "date", res.Date,
			"artifacts", len(res.Artifacts), "manifest", res.ManifestKey)
		return nil
	}

	// Daemon mode: sleep until the next 01:00 UTC boundary, export the day
	// that just closed, repeat. nextRun recomputes after every run so a long
	// export never skews the schedule.
	runAt := envOr("EXC_EXPORT_RUN_AT", "01:00")
	for {
		next, err := nextRun(time.Now().UTC(), runAt)
		if err != nil {
			return err
		}
		log.Info("next export scheduled", "at", next.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			log.Info("s3-market-data-exporter shutting down")
			return nil
		case <-time.After(time.Until(next)):
		}
		// Export the day that just closed: next-1d at 00:00.
		day := next.AddDate(0, 0, -1)
		res, err := ex.ExportDay(ctx, day)
		if err != nil {
			// Fail loud but stay alive — tomorrow's run retries the schedule;
			// a manual `-once -date` re-run is the remediation.
			log.Error("daily export failed", "date", day.Format("2006-01-02"),
				"error", err)
			continue
		}
		log.Info("export complete", "date", res.Date,
			"artifacts", len(res.Artifacts), "manifest", res.ManifestKey)
	}
}

func targetDay(dateStr string) (time.Time, error) {
	if dateStr == "" {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC), nil
	}
	d, err := time.ParseInLocation("2006-01-02", dateStr, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("-date %q: %w", dateStr, err)
	}
	return d, nil
}

// nextRun returns the next occurrence of HH:MM UTC strictly after now.
func nextRun(now time.Time, hhmm string) (time.Time, error) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("EXC_EXPORT_RUN_AT %q: want HH:MM", hhmm)
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return time.Time{}, fmt.Errorf("EXC_EXPORT_RUN_AT: %w", err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return time.Time{}, fmt.Errorf("EXC_EXPORT_RUN_AT: %w", err)
	}
	if h > 23 || m > 59 {
		return time.Time{}, fmt.Errorf("EXC_EXPORT_RUN_AT %q out of range", hhmm)
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

func buildUploader() (persistence.Uploader, error) {
	if dir := os.Getenv("EXC_EXPORT_DIR"); dir != "" {
		return persistence.DirUploader{Root: dir}, nil
	}
	bucket := envOr("EXC_S3_BUCKET", "exchange-market-data")
	endpoint := os.Getenv("EXC_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("AWS_ENDPOINT_URL")
	}
	if endpoint == "" {
		endpoint = "https://s3." + envOr("EXC_S3_REGION", "eu-west-1") + ".amazonaws.com"
	}
	keyID, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if keyID == "" || secret == "" {
		return nil, fmt.Errorf("no upload backend: set EXC_EXPORT_DIR (dev) or " +
			"AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY (+EXC_S3_*) for S3")
	}
	return &s3Uploader{
		Endpoint:      strings.TrimRight(endpoint, "/"),
		Bucket:        bucket,
		Region:        envOr("EXC_S3_REGION", "eu-west-1"),
		AccessKey:     keyID,
		SecretKey:     secret,
		SessionToken:  os.Getenv("AWS_SESSION_TOKEN"),
		PublicReadACL: os.Getenv("EXC_S3_PUBLIC") == "1",
	}, nil
}

func buildNotifier() (persistence.Notifier, error) {
	var sinks []persistence.Notifier
	if u := os.Getenv("EXC_EXPORT_NATS_URL"); u != "" {
		sinks = append(sinks, &natsNotifier{
			URL: u,
			Subject: envOr("EXC_EXPORT_NATS_SUBJECT",
				"analytics.marketdata.export.completed"),
		})
	}
	if cb := os.Getenv("EXC_EXPORT_CALLBACK_URL"); cb != "" {
		sinks = append(sinks, &httpNotifier{URL: cb})
	}
	switch len(sinks) {
	case 0:
		return nil, nil
	case 1:
		return sinks[0], nil
	default:
		return multiNotifier(sinks), nil
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
