package main

// Completion-event notifiers for the Phase-23 catalog service
// (Task 23.3.7): NATS core publish or an HTTP callback POST. Both carry the
// ExportResult JSON as the event payload.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	gonats "github.com/nats-io/nats.go"

	"exchange/internal/persistence"
)

// natsNotifier publishes the ExportResult JSON to a NATS subject
// (default analytics.marketdata.export.completed — the `analytics`
// JetStream stream binds analytics.>).
type natsNotifier struct {
	URL     string
	Subject string
}

func (n *natsNotifier) Notify(ctx context.Context, evt persistence.ExportResult) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	nc, err := gonats.Connect(n.URL, gonats.Timeout(5*time.Second),
		gonats.Name("s3-market-data-exporter"))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	if err := nc.Publish(n.Subject, payload); err != nil {
		return fmt.Errorf("nats publish %s: %w", n.Subject, err)
	}
	return nc.FlushWithContext(ctx)
}

// httpNotifier POSTs the ExportResult JSON to a webhook URL (the
// catalog-service callback seam when NATS is not in the deployment).
type httpNotifier struct{ URL string }

func (h *httpNotifier) Notify(ctx context.Context, evt persistence.ExportResult) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL,
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("callback %s: HTTP %d: %s", h.URL, resp.StatusCode,
			string(body))
	}
	return nil
}

// multiNotifier fans the event out to every configured sink; first error
// wins (all sinks still get attempted).
type multiNotifier []persistence.Notifier

func (m multiNotifier) Notify(ctx context.Context, evt persistence.ExportResult) error {
	var first error
	for _, n := range m {
		if err := n.Notify(ctx, evt); err != nil && first == nil {
			first = err
		}
	}
	return first
}
