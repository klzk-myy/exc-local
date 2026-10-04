// JetStream ingest — Phase-14 Task 14.3.12.
//
// The task calls for a NATS JetStream webhook worker; the durable
// delivery pipeline itself (sign/POST/retry/dead-letter) is owned by
// the canonical Phase-05 dispatcher — this consumer is the missing
// ingest leg: events published to the "webhooks" stream are unpacked
// and queued into webhook_deliveries via Store.Enqueue, so the
// existing signed-delivery engine does the wire work.
//
// Message contract (producers: bridge/domain emitters):
//
//	{"account_id": <int64>, "event": "<registered-event>", "data": <any>}
//
// Processing semantics: unknown events / malformed payloads Term() the
// message (poison — redelivery cannot fix it); enqueue failures Nak()
// so the at-least-once consumer redelivers after AckWait; success
// Ack()s. MaxDeliver=5 bounds redelivery on repeated Nak — poison
// termination is the fail-closed path either way.
package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	excnats "exchange/internal/nats"
)

// IngestStream is the canonical event subject space for webhook
// fan-out; the durable consumer name is stable across restarts so ack
// state survives.
const (
	IngestStream  = "webhooks"
	IngestDurable = "webhook-ingest"
)

// ingestMsg is the stream payload producers publish.
type ingestMsg struct {
	AccountID int64           `json:"account_id"`
	Event     string          `json:"event"`
	Data      json.RawMessage `json:"data"`
}

// JetStreamIngest consumes the "webhooks" stream into the delivery
// queue. Construct it once at gateway startup; Run blocks until ctx.
type JetStreamIngest struct {
	nc    *excnats.Client
	store *Store
	log   *slog.Logger
}

func NewJetStreamIngest(nc *excnats.Client, store *Store,
	log *slog.Logger) *JetStreamIngest {
	if log == nil {
		log = slog.Default()
	}
	return &JetStreamIngest{nc: nc, store: store, log: log}
}

// Run ensures the stream + durable consumer then subscribes until ctx
// is done. Stream creation is idempotent (CreateOrUpdateStream); the
// consumer keeps the canonical template (AckExplicit, AckWait 30s,
// MaxDeliver 5).
func (j *JetStreamIngest) Run(ctx context.Context) error {
	if j.nc == nil || j.store == nil {
		return fmt.Errorf("webhooks: jetstream ingest not wired")
	}
	if _, err := j.nc.EnsureStream(ctx, IngestStream); err != nil {
		return err
	}
	cons, err := j.nc.EnsureConsumerRetry(ctx, IngestStream, IngestDurable,
		excnats.WithFilterSubject(IngestStream+".>"))
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
	j.log.Info("webhook jetstream ingest started",
		"stream", IngestStream, "durable", IngestDurable)
	<-ctx.Done()
	return nil
}

// handle decodes one message and queues its deliveries. See the file
// header for the Ack/Nak/Term decision table.
func (j *JetStreamIngest) handle(ctx context.Context, m jetstream.Msg) {
	var msg ingestMsg
	if err := json.Unmarshal(m.Data(), &msg); err != nil ||
		msg.AccountID == 0 || msg.Event == "" {
		j.log.Warn("webhook ingest: malformed message terminated",
			"subject", m.Subject())
		_ = m.Term()
		return
	}
	if !ValidEvents[msg.Event] {
		j.log.Warn("webhook ingest: unknown event terminated",
			"event", msg.Event)
		_ = m.Term()
		return
	}
	env, err := json.Marshal(Envelope{
		Event:     msg.Event,
		AccountID: msg.AccountID,
		Data:      msg.Data,
		SentAt:    time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		_ = m.Term()
		return
	}
	if _, err := j.store.Enqueue(ctx, msg.AccountID, msg.Event, env); err != nil {
		j.log.Warn("webhook ingest: enqueue failed, redelivering",
			"event", msg.Event, "account_id", msg.AccountID, "err", err)
		_ = m.Nak()
		return
	}
	_ = m.Ack()
}
