// Surveillance-signal → SAR bridge — Phase-21 Task 21.3.3.
//
// Deterministic market-abuse signals reach this service two ways:
//
//  1. JetStream — durable pull consumers on the canonical streams.
//     §14.1c publishes signal events on "surveillance.signals.{type}"
//     (stream "surveillance"); the task text additionally names the
//     "compliance" stream, so the gateway wires one consumer per binding
//     (defaults below + WithBinding). Ack discipline mirrors the
//     webhook ingest: poison payloads Term(), store failures Nak() for
//     AckWait redelivery, success Ack()s; MaxDeliver=5 bounds retries.
//     Duplicate deliveries dedup through sar_reports.source_ref.
//  2. The Phase-17 detector writes surveillance_signals rows directly —
//     DraftFromOpenSignals polls that table so a silent NATS leg never
//     starves the filing pipeline.
//
// Every signal produces a DRAFT SAR keyed 'signal:{id}' — officers
// review/reject in the dashboard; nothing here enforces on accounts
// (Task 21.3.8 owns enforcement).
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	excnats "exchange/internal/nats"
	excerrors "exchange/pkg/errors"
)

// Canonical bindings (§14.1c event subjects).
const (
	SARSignalStream   = "surveillance"
	SARSignalSubject  = "surveillance.signals.>"
	SARSignalDurable  = "sar-signal-drafts"
	SARSignalStreamC  = "compliance" // twin binding per the task text
	SARSignalSubjectC = "compliance.signals.>"
	SARSignalDurableC = "sar-signal-drafts-compliance"
)

// signalMsg is the §14.1c surveillance event schema. The canonical
// fields are signal_id (the surveillance_signals row) and the
// detector's dedup_key; inline fields cover emitters that publish the
// signal body without a stored row.
type signalMsg struct {
	SignalID     int64           `json:"signal_id"`
	SignalType   string          `json:"signal_type"`
	AccountID    *int64          `json:"account_id"`
	AccountHash  uint64          `json:"account_hash"`
	Confidence   string          `json:"confidence"`
	Evidence     json.RawMessage `json:"evidence"`
	InstrumentID string          `json:"instrument_id"`
	Symbol       string          `json:"symbol"`
	Timestamp    time.Time       `json:"timestamp"`
	DetectedAt   time.Time       `json:"detected_at"`
	DedupKey     string          `json:"dedup_key"`
}

// SARSignalIngest consumes one (stream, subject) binding into SAR
// drafts. Run blocks until ctx is done.
type SARSignalIngest struct {
	nc      *excnats.Client
	sar     *SARService
	stream  string
	subject string
	durable string
	log     *slog.Logger
}

// NewSARSignalIngest wires the default surveillance-stream binding.
func NewSARSignalIngest(nc *excnats.Client, sar *SARService,
	log *slog.Logger) *SARSignalIngest {
	if log == nil {
		log = slog.Default()
	}
	return &SARSignalIngest{nc: nc, sar: sar, log: log,
		stream: SARSignalStream, subject: SARSignalSubject,
		durable: SARSignalDurable}
}

// WithBinding returns a copy bound to a different stream/subject —
// used for the compliance-stream twin.
func (j *SARSignalIngest) WithBinding(stream, subject, durable string) *SARSignalIngest {
	c := *j
	c.stream, c.subject, c.durable = stream, subject, durable
	return &c
}

// Run ensures the stream + durable consumer then subscribes until ctx
// is done.
func (j *SARSignalIngest) Run(ctx context.Context) error {
	if j.nc == nil || j.sar == nil {
		return fmt.Errorf("compliance: sar signal ingest not wired")
	}
	if _, err := j.nc.EnsureStream(ctx, j.stream); err != nil {
		return err
	}
	cons, err := j.nc.EnsureConsumerRetry(ctx, j.stream, j.durable,
		excnats.WithFilterSubject(j.subject))
	if err != nil {
		return err
	}
	stop, err := j.nc.Subscribe(cons, func(m jetstream.Msg) {
		j.handle(ctx, m)
	})
	if err != nil {
		return err
	}
	defer stop()
	j.log.Info("sar signal ingest started",
		"stream", j.stream, "subject", j.subject, "durable", j.durable)
	<-ctx.Done()
	return nil
}

// handle decodes one signal event and drafts the SAR. Term on poison,
// Nak on store failure, Ack on success-or-duplicate.
func (j *SARSignalIngest) handle(ctx context.Context, m jetstream.Msg) {
	var msg signalMsg
	if err := json.Unmarshal(m.Data(), &msg); err != nil {
		j.log.Warn("sar ingest: malformed message terminated",
			"subject", m.Subject())
		_ = m.Term()
		return
	}
	if msg.SignalID <= 0 && msg.DedupKey == "" {
		j.log.Warn("sar ingest: no signal identity terminated",
			"subject", m.Subject())
		_ = m.Term()
		return
	}
	var err error
	if msg.SignalID > 0 {
		_, _, err = j.sar.DraftFromSignal(ctx, msg.SignalID)
	} else {
		// Inline-only signal — draft from the payload with the
		// detector's dedup key as the deterministic anchor.
		subject := ""
		if msg.AccountHash != 0 {
			subject = fmt.Sprintf("account_hash:%d", msg.AccountHash)
		}
		detected := msg.DetectedAt
		if detected.IsZero() {
			detected = msg.Timestamp
		}
		var evidence any
		if len(msg.Evidence) > 0 {
			evidence = json.RawMessage(msg.Evidence)
		}
		_, _, err = j.sar.Draft(ctx, SARDraftInput{
			TriggerType: SARTriggerSurveillance,
			AccountID:   msg.AccountID,
			SubjectRef:  subject,
			Description: fmt.Sprintf("surveillance signal %s (%s) — officer review required",
				msg.SignalType, msg.Symbol),
			Evidence:   evidence,
			SourceRef:  "signal-msg:" + msg.DedupKey,
			DetectedAt: detected,
		})
	}
	if err != nil {
		// Coding/validation rejects are poison (never resolve on
		// redelivery); store/infra failures redeliver.
		if excerrors.CodeOf(err) == "INVALID_REQUEST" ||
			excerrors.CodeOf(err) == "NOT_FOUND" {
			j.log.Warn("sar ingest: signal rejected — terminating",
				"err", err)
			_ = m.Term()
			return
		}
		j.log.Warn("sar ingest: draft failed, redelivering", "err", err)
		_ = m.Nak()
		return
	}
	_ = m.Ack()
}

// ---------------------------------------------------------------------------
// PG-side signal poller (Phase-17 writes surveillance_signals directly)
// ---------------------------------------------------------------------------

// DraftFromOpenSignals drafts SARs for surveillance_signals rows not yet
// covered by a report — the deterministic backstop when no NATS
// publisher is wired. Idempotent via the 'signal:{id}' source_ref dedup.
func (s *SARService) DraftFromOpenSignals(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.id FROM surveillance_signals s
		WHERE s.status = 'OPEN'
		  AND NOT EXISTS (
		      SELECT 1 FROM sar_reports r
		      WHERE r.source_ref = 'signal:' || s.id::text)
		ORDER BY s.id LIMIT $1`, limit)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "open signal scan", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, excerrors.Wrap("INTERNAL_ERROR", "signal id scan", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "signal scan", err)
	}
	drafted := 0
	for _, id := range ids {
		_, created, err := s.DraftFromSignal(ctx, id)
		if err != nil {
			return drafted, err
		}
		if created {
			drafted++
		}
	}
	return drafted, nil
}
