// Command chaostool — helpers for the Phase-04.5 Task 4.5.3.1 chaos suite.
//
// Subcommands:
//
//	wal-marker  write a {seq}.wal rebase-marker segment into a WAL dir
//	            (identical bytes to RecoveryManager::write_rebase_marker /
//	            recovery.RecWriteRebaseMarker — used to fabricate trimmed/gapped
//	            WAL fixtures without hand-rolling the format)
//	probe       attach to the shm rings as a gateway client, submit N orders,
//	            and print a JSON per-order ack accounting (filled / cancelled /
//	            unanswered) — the "timeout-safe requeue" probe: an order is
//	            unambiguously REJECTED only when it produced no terminal event,
//	            and unambiguously ACCEPTED when it did. Silent loss = acked but
//	            absent from the journal, which the harness verifies via wal_audit.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/recovery"

	flatbuffers "github.com/google/flatbuffers/go"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: chaostool <wal-marker|probe> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "wal-marker":
		err = cmdWalMarker(os.Args[2:])
	case "probe":
		err = cmdProbe(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "chaostool: %v\n", err)
		os.Exit(1)
	}
}

func cmdWalMarker(args []string) error {
	fs := flag.NewFlagSet("wal-marker", flag.ContinueOnError)
	dir := fs.String("dir", "", "WAL segment directory (required)")
	shard := fs.Uint("shard", 0, "shard id")
	seq := fs.Uint64("seq", 0, "marker seq (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return fmt.Errorf("--dir is required")
	}
	path, wrote, err := recovery.RecWriteRebaseMarker(*dir, uint16(*shard), *seq)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"marker_path": path, "written": wrote, "seq": *seq, "shard": *shard,
	})
}

// ---------------------------------------------------------------------------
// probe — bounded order submission with per-order terminal-event accounting.
// ---------------------------------------------------------------------------

func cmdProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	base := fs.String("base", "", "shm ring base name (required)")
	shard := fs.Uint("shard", 0, "shard id")
	instrument := fs.Uint("instrument", 7, "instrument id")
	count := fs.Int("count", 50, "orders to submit")
	price := fs.Int64("price", 1050000000, "limit price (ticks)")
	qty := fs.Int64("qty", 100000000, "qty units (1 lot = 1e8)")
	ioc := fs.Bool("ioc", true, "send IOC takers (fill or terminal-cancel)")
	orderIDBase := fs.Uint64("order-id-base", 1, "first order_id")
	ackWindow := fs.Duration("ack-window", 3*time.Second,
		"how long to drain the out ring after the last send")
	attachTimeout := fs.Duration("attach-timeout", 10*time.Second,
		"max wait for the engine's rings to exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" {
		return fmt.Errorf("--base is required")
	}

	// Attach (gateway endpoint consumes _out, produces _in).
	deadline := time.Now().Add(*attachTimeout)
	var ch *ipc.Channel
	for {
		var aerr error
		ch, aerr = ipc.OpenChannel(*base, uint16(*shard), ipc.EndpointGateway,
			false, ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
		if aerr == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("channel attach: %w", aerr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = ch.Close() }()

	// Drain in the background; per-order outcome map.
	type outcome struct {
		Fills   int `json:"fills"`
		Cancels int `json:"cancels"`
	}
	results := map[uint64]*outcome{}
	tradeIDs := map[uint64]bool{}
	dupFills := 0
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 64<<10)
		last := time.Now()
		for {
			n := ch.Poll(buf)
			if n <= 0 {
				if time.Since(last) > 400*time.Millisecond {
					return // quiet period
				}
				time.Sleep(time.Millisecond)
				continue
			}
			last = time.Now()
			ev := ipc.DecodeEvent(buf[:n])
			if ev == nil {
				continue
			}
			switch ev.TypeType() {
			case wire.EventTypeTradeFill:
				tf := ipc.EventTradeFill(ev)
				if tf == nil {
					continue
				}
				if tradeIDs[tf.TradeId()] {
					dupFills++
				}
				tradeIDs[tf.TradeId()] = true
				for _, oid := range [2]uint64{tf.BuyOrderId(), tf.SellOrderId()} {
					if o := results[oid]; o != nil {
						o.Fills++
					}
				}
			case wire.EventTypeOrderCancel:
				var t flatbuffers.Table
				if ev.Type(&t) {
					oc := &wire.OrderCancel{}
					oc.Init(t.Bytes, t.Pos)
					if o := results[oc.OrderId()]; o != nil {
						o.Cancels++
					}
				}
			}
		}
	}()

	// Submit. Order ids are dense from order-id-base so the harness can map
	// each wire id to a journal row unambiguously.
	b := flatbuffers.NewBuilder(512)
	sent, sendDrops := 0, 0
	tif := wire.TimeInForceIOC
	if !*ioc {
		tif = wire.TimeInForceGTC
	}
	for i := 0; i < *count; i++ {
		oid := *orderIDBase + uint64(i)
		side := wire.SideBuy
		if i%2 == 1 {
			side = wire.SideSell
		}
		b.Reset()
		msg := ipc.EncodeOrderNewEvent(b, uint64(i+1),
			uint64(time.Now().UnixNano()), ipc.OrderNewMsg{
				OrderID: oid, AccountID: 1 + uint64(i%64),
				InstrumentID: uint32(*instrument), Side: side,
				Type: wire.OrderTypeLimit, Qty: *qty, Price: *price,
				TIF:           tif,
				ClientOrderID: fmt.Sprintf("chaos-probe-%d", oid),
			})
		cp := make([]byte, len(msg))
		copy(cp, msg)
		sendDeadline := time.Now().Add(2 * time.Second)
		sentOK := false
		for !sentOK { // bounded retry — the "timeout" side of the contract
			sentOK = ch.Send(cp)
			if !sentOK && time.Now().After(sendDeadline) {
				break
			}
			if !sentOK {
				time.Sleep(time.Millisecond)
			}
		}
		if !sentOK {
			sendDrops++ // ring stayed full past the timeout — cleanly rejected
		} else {
			results[oid] = &outcome{}
		}
		sent++
	}

	// Drain for the ack window, then report.
	time.Sleep(*ackWindow)
	<-drainDone

	acked, unacked := 0, 0
	unackedIDs := []uint64{}
	for oid, o := range results {
		if o.Fills > 0 || o.Cancels > 0 {
			acked++
		} else {
			unacked++
			unackedIDs = append(unackedIDs, oid)
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"sent":            sent,
		"send_drops":      sendDrops,
		"acked":           acked,
		"unacked":         unacked,
		"unacked_ids":     unackedIDs,
		"dup_fill_events": dupFills,
		"trade_ids_seen":  len(tradeIDs),
		"producer_alive":  ch.ProducerAlive(),
	})
}
