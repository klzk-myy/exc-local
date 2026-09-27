package nats

import (
	"context"
	"fmt"
	"time"
)

// HealthReport is the point-in-time JetStream health snapshot returned by
// JetStreamHealth (Task 1.3.11 §4).
//
// For infrastructure metrics — per-server stream/consumer replica state,
// raft leader info, API error counts — each node also serves
// GET :8222/jsz (ports 8222/8223/8224 in dev). That endpoint is the intended
// Prometheus scrape target; dashboards and alert rules are owned by
// Phase-07 (Task 7.3.8). This report complements it from inside the
// application: consumer lag in domain terms.
type HealthReport struct {
	CheckedAt     time.Time      `json:"checked_at"`
	ConnectedURL  string         `json:"connected_url"`
	Account       AccountHealth  `json:"account"`
	Streams       []StreamHealth `json:"streams"`
	MissingStream []string       `json:"missing_streams,omitempty"`
}

// AccountHealth summarises account-level JetStream usage.
type AccountHealth struct {
	Streams     int    `json:"streams"`
	Consumers   int    `json:"consumers"`
	MemoryBytes uint64 `json:"memory_bytes"`
	StoreBytes  uint64 `json:"store_bytes"`
}

// StreamHealth is one stream's state plus each consumer's lag.
type StreamHealth struct {
	Name        string           `json:"name"`
	Subjects    []string         `json:"subjects"`
	Replicas    int              `json:"replicas"`
	Retention   string           `json:"retention"`
	MaxAge      time.Duration    `json:"max_age"`
	Messages    uint64           `json:"messages"`
	Bytes       uint64           `json:"bytes"`
	FirstSeq    uint64           `json:"first_seq"`
	LastSeq     uint64           `json:"last_seq"`
	ClusterName string           `json:"cluster_name,omitempty"`
	Leader      string           `json:"leader,omitempty"`
	Consumers   []ConsumerHealth `json:"consumers"`
}

// ConsumerHealth is per-consumer lag: NumPending is messages waiting to be
// delivered, NumAckPending is delivered-but-unacked (in-flight or stuck),
// NumRedelivered is lifetime redelivery count.
type ConsumerHealth struct {
	Name           string `json:"name"`
	NumPending     uint64 `json:"num_pending"`
	NumAckPending  int    `json:"num_ack_pending"`
	NumWaiting     int    `json:"num_waiting"`
	NumRedelivered int    `json:"num_redelivered"`
	// Lag is pending + in-flight: total messages not yet fully processed.
	Lag uint64 `json:"lag"`
}

// JetStreamHealth snapshots account, stream and consumer state for every
// canonical stream. A configured-but-missing stream is reported in
// MissingStream rather than failing the whole check, so health endpoints
// can distinguish "broker down" from "not yet provisioned".
func (c *Client) JetStreamHealth(ctx context.Context) (*HealthReport, error) {
	rep := &HealthReport{CheckedAt: time.Now().UTC(), ConnectedURL: c.nc.ConnectedUrl()}

	acct, err := c.js.AccountInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("nats: account info: %w", err)
	}
	rep.Account = AccountHealth{
		Streams:     acct.Streams,
		Consumers:   acct.Consumers,
		MemoryBytes: acct.Memory,
		StoreBytes:  acct.Store,
	}

	for _, name := range Streams {
		s, err := c.js.Stream(ctx, name)
		if err != nil {
			rep.MissingStream = append(rep.MissingStream, name)
			continue
		}
		info, err := s.Info(ctx)
		if err != nil {
			return rep, fmt.Errorf("nats: stream %q info: %w", name, err)
		}
		sh := StreamHealth{
			Name:      info.Config.Name,
			Subjects:  info.Config.Subjects,
			Replicas:  info.Config.Replicas,
			Retention: info.Config.Retention.String(),
			MaxAge:    info.Config.MaxAge,
			Messages:  info.State.Msgs,
			Bytes:     info.State.Bytes,
			FirstSeq:  info.State.FirstSeq,
			LastSeq:   info.State.LastSeq,
		}
		if info.Cluster != nil {
			sh.ClusterName = info.Cluster.Name
			sh.Leader = info.Cluster.Leader
		}
		for ci := range s.ListConsumers(ctx).Info() {
			if ci == nil {
				continue
			}
			sh.Consumers = append(sh.Consumers, ConsumerHealth{
				Name:           ci.Name,
				NumPending:     ci.NumPending,
				NumAckPending:  ci.NumAckPending,
				NumWaiting:     ci.NumWaiting,
				NumRedelivered: ci.NumRedelivered,
				Lag:            ci.NumPending + uint64(max(ci.NumAckPending, 0)),
			})
		}
		rep.Streams = append(rep.Streams, sh)
	}
	return rep, nil
}
