// Command natsctl is the ops/provisioning CLI for the JetStream backbone
// (Task 1.3.11, spec §2.3.1). It is intentionally separate from the service
// daemons: provisioning is an operator action, not a boot side effect.
//
//	natsctl init    — create/reconcile all 7 canonical streams (idempotent)
//	natsctl health  — print per-stream/consumer lag report (also :8222/jsz)
//	natsctl smoke   — end-to-end proof: publish → durable fetch → ack
//	natsctl dlq …   — dead-letter queue ops (Task 7.3.10): init | list |
//	                  get <seq> | replay <seq> | discard <seq>
//
// Config comes from the shared loader (config.yaml / EXC_* env vars), e.g.
// EXC_NATS_URLS=nats://127.0.0.1:4222 natsctl init
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"exchange/internal/config"
	excnats "exchange/internal/nats"
	"exchange/internal/observability"
	"exchange/pkg/logging"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "natsctl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: natsctl <init|health|smoke|dlq>")
	}
	cmd, args := args[0], args[1:]

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := logging.ParseLevel(cfg.Logging.Level)
	log, err := logging.New(level, cfg.Logging.Format)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ncfg := excnats.DefaultConfig(cfg.NATS.URLList())
	ncfg.Name = "natsctl-" + cmd
	cli, err := excnats.Connect(ctx, ncfg, log)
	if err != nil {
		return err
	}
	defer cli.Close()

	switch cmd {
	case "init":
		return cmdInit(ctx, cli)
	case "health":
		return cmdHealth(ctx, cli)
	case "smoke":
		return cmdSmoke(ctx, cli, args)
	case "dlq":
		return cmdDLQ(ctx, cli, args)
	default:
		return fmt.Errorf("unknown command %q: usage: natsctl <init|health|smoke|dlq>", cmd)
	}
}

// cmdDLQ is the admin CLI surface for the ops-dlq stream (Task 7.3.10):
// inspect entries and controlled redrive/discard. `init` provisions the
// stream (operator action, same convention as `natsctl init`).
func cmdDLQ(ctx context.Context, cli *excnats.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: natsctl dlq <init|list|get|replay|discard> [seq]")
	}
	sub, args := args[0], args[1:]
	if sub == "init" {
		if _, err := observability.NewJetStreamDLQ(ctx, cli.JetStream()); err != nil {
			return err
		}
		fmt.Println("natsctl: ops-dlq stream ensured")
		return nil
	}
	store, err := observability.OpenJetStreamDLQ(ctx, cli.JetStream())
	if err != nil {
		return fmt.Errorf("%w (run: natsctl dlq init)", err)
	}
	switch sub {
	case "list":
		f := observability.ListFilter{}
		for i := 0; i+1 < len(args); i += 2 {
			switch args[i] {
			case "-stream":
				f.Stream = args[i+1]
			case "-consumer":
				f.Consumer = args[i+1]
			case "-limit":
				f.Limit, _ = strconv.Atoi(args[i+1])
			}
		}
		list, err := store.List(ctx, f)
		if err != nil {
			return err
		}
		fmt.Printf("%-6s %-14s %-20s %-30s %-24s %s\n",
			"SEQ", "STREAM", "CONSUMER", "SUBJECT", "FAILED_AT", "REASON")
		for _, dl := range list {
			fmt.Printf("%-6d %-14s %-20s %-30s %-24s %s\n",
				dl.Seq, dl.Stream, dl.Consumer, dl.Subject,
				dl.FailedAt.Format(time.RFC3339), dl.Reason)
		}
		fmt.Printf("%d entries\n", len(list))
		return nil
	case "get", "replay", "discard":
		if len(args) < 1 {
			return fmt.Errorf("usage: natsctl dlq %s <seq>", sub)
		}
		seq, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("seq: %w", err)
		}
		switch sub {
		case "get":
			dl, err := store.Get(ctx, seq)
			if err != nil {
				return err
			}
			out, _ := json.MarshalIndent(dl, "", "  ")
			fmt.Println(string(out))
			return nil
		case "replay":
			if err := store.Replay(ctx, seq); err != nil {
				return err
			}
			fmt.Printf("natsctl: dlq entry %d replayed to origin subject\n", seq)
			return nil
		default:
			if err := store.Discard(ctx, seq); err != nil {
				return err
			}
			fmt.Printf("natsctl: dlq entry %d discarded\n", seq)
			return nil
		}
	default:
		return fmt.Errorf("unknown dlq command %q: usage: natsctl dlq <init|list|get|replay|discard>", sub)
	}
}

// cmdInit creates or reconciles the canonical streams and prints their
// effective config — the provisioning entrypoint (replaces a shell script).
func cmdInit(ctx context.Context, cli *excnats.Client) error {
	infos, err := cli.EnsureStreams(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%-16s %-24s %-9s %-5s %-6s %s\n",
		"STREAM", "SUBJECTS", "RETENTION", "REPL", "MAXAGE", "MSGS")
	for _, i := range infos {
		fmt.Printf("%-16s %-24s %-9s %-5d %-6s %d\n",
			i.Config.Name, i.Config.Subjects, i.Config.Retention,
			i.Config.Replicas, i.Config.MaxAge, i.State.Msgs)
	}
	fmt.Printf("natsctl: %d streams ensured\n", len(infos))
	return nil
}

// cmdHealth prints the application-side lag report. Infrastructure metrics
// (raft state, replica peers) come from GET :8222/jsz — the Prometheus
// scrape target; dashboards are Phase-07 scope.
func cmdHealth(ctx context.Context, cli *excnats.Client) error {
	rep, err := cli.JetStreamHealth(ctx)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	if len(rep.MissingStream) > 0 {
		return fmt.Errorf("streams not provisioned: %v (run: natsctl init)", rep.MissingStream)
	}
	return nil
}

// cmdSmoke is the demo/live proof: ensure streams → publish → durable
// consumer fetch → explicit ack → verify no redelivery.
func cmdSmoke(ctx context.Context, cli *excnats.Client, _ []string) error {
	if _, err := cli.EnsureStreams(ctx); err != nil {
		return err
	}

	cons, err := cli.EnsureConsumer(ctx, "trades", "natsctl-smoke",
		excnats.WithFilterSubject("trades.0.EUR-USD"))
	if err != nil {
		return err
	}

	// Drain anything still pending for this durable (e.g. leftovers from a
	// previous smoke/test run) so the check below observes only the message
	// it publishes itself.
	for {
		stale, err := excnats.Fetch(ctx, cons, 10, 500*time.Millisecond)
		if err != nil {
			return err
		}
		if len(stale) == 0 {
			break
		}
		for _, m := range stale {
			_ = m.Ack()
		}
		fmt.Printf("drained %d stale pending message(s)\n", len(stale))
	}

	payload := []byte(fmt.Sprintf(`{"smoke":true,"ts":%q}`, time.Now().UTC().Format(time.RFC3339Nano)))
	pa, err := cli.Publish(ctx, "trades", 0, "EUR-USD", payload)
	if err != nil {
		return err
	}
	fmt.Printf("published %s → stream=%s seq=%d duplicate=%v\n",
		"trades.0.EUR-USD", pa.Stream, pa.Sequence, pa.Duplicate)

	msgs, err := excnats.Fetch(ctx, cons, 1, 15*time.Second)
	if err != nil {
		return err
	}
	if len(msgs) != 1 {
		return fmt.Errorf("smoke: fetched %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	fmt.Printf("fetched subject=%s data=%s\n", m.Subject(), m.Data())
	if err := m.Ack(); err != nil {
		return fmt.Errorf("smoke: ack: %w", err)
	}
	fmt.Println("acked")

	// Verify no redelivery after explicit ack.
	time.Sleep(300 * time.Millisecond)
	again, err := excnats.Fetch(ctx, cons, 1, 3*time.Second)
	if err != nil {
		return err
	}
	if len(again) != 0 {
		for _, m := range again {
			_ = m.Ack()
		}
		return fmt.Errorf("smoke: acked message redelivered (%d msgs)", len(again))
	}
	fmt.Println("no redelivery after ack — at-least-once delivery verified")
	return nil
}
