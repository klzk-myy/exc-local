// Command swapdrill — Phase-09 Task 9.3.16 evidence harness for the
// bare-metal shard binary-swap drill (deploy/scripts/shard_swap_drill.sh).
//
// It plays the gateway role on the shm SPSC transport (the mandated
// fallback; spec §2.3) against a real matching_engine process:
//
//	swapdrill drive -base <ipc_base> -shard N -instrument N \
//	    -rate <orders/s> -hold-file <path> -burst-file <path> -burst-n N \
//	    -sent-ids <path> -events <path.jsonl> -status <path.json> \
//	    -report <path.json>
//
//	  Produces FlatBuffers Event{OrderNew} onto {base}_{shard}_in at -rate,
//	  drains {base}_{shard}_out continuously (JSONL evidence per event),
//	  and never blocks the drain on send backpressure.
//
//	  Control surface (files, touched/removed by the drill script):
//	    -hold-file   while present, new-order sends pause — the local
//	                 stand-in for the gateway's Maintenance-mode 503 hold
//	                 (scripts/deploy/shard-swap.sh step 2).
//	    -burst-file  on each new mtime, fires a one-shot -burst-n burst at
//	                 max write speed regardless of hold — proving that
//	                 ingress written while the engine process is DOWN lands
//	                 in the persistent /dev/shm ring and is journaled after
//	                 the new binary re-attaches (spec §19.6 swap window).
//	  SIGTERM/SIGINT (or -duration) ends the run: sends stop, the outbound
//	  ring drains through a quiet period, report.json is written.
//
//	swapdrill audit -wal-dir <dir> [-expect-ids <path>]
//
//	  Read-only WAL walk (services/internal/recovery.ScanSegment — the same
//	  scanner the wal-recovery CLI and the C++ prescan ladder share).
//	  Emits JSON: seq contiguity, per-type counts, trade_id uniqueness,
//	  and per-order-id coverage against the -expect-ids file.
//
// Exit codes: 0 ok · 1 assertion/IO failure · 2 usage.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	"exchange/internal/recovery"
	"exchange/internal/tracing"
)

// ---------------------------------------------------------------------------
// Order synthesis — identical price/qty model to tests/soak/loadgen.go so
// every emitted order passes the bound instrument's tick grid and the
// pre-trade checker's price bands.
// ---------------------------------------------------------------------------

const (
	tickSizeTicks     = 1_000
	pipSizeTicks      = 10_000
	midStartTicks     = 108_500_000
	midMaxWanderTicks = 1_000_000
	passiveBandPips   = 20
	crossReachPips    = 25
	lotQtyUnits       = 100_000 * 100_000_000
)

// ---------------------------------------------------------------------------
// drive
// ---------------------------------------------------------------------------

type driveStats struct {
	sent          atomic.Uint64 // accepted into the _in ring
	shed          atomic.Uint64 // TryWrite never landed after retry budget
	held          atomic.Uint64 // paced slots skipped while hold-file present
	burstAccepted atomic.Uint64
	burstShed     atomic.Uint64
	burstsFired   atomic.Uint64

	fills        atomic.Uint64
	cancels      atomic.Uint64
	l3Adds       atomic.Uint64
	l3Fills      atomic.Uint64
	l3Cancels    atomic.Uint64
	l3Modifies   atomic.Uint64
	snapshots    atomic.Uint64
	timeTicks    atomic.Uint64
	otherEvents  atomic.Uint64
	decodeErrors atomic.Uint64

	dupTradeIDs   atomic.Uint64
	dupL3Fills    atomic.Uint64 // repeated (order_id, trade_id) Fill legs
	dupFillSeqs   atomic.Uint64 // TradeFill.seq (engine/wal seq) repeats
	l3SeqGaps     atomic.Uint64 // per-instrument L3 seq discontinuities
	l3SeqResets   atomic.Uint64 // seq re-base (engine restart generation)
	lastL3Seq     atomic.Uint64
	ordersSeenOut atomic.Uint64 // distinct generated ids seen on _out

	inOccupancy    atomic.Uint64
	outOccupancy   atomic.Uint64
	inDrops        atomic.Uint64
	outDrops       atomic.Uint64
	producerAlive  atomic.Bool
	burstWhileDead atomic.Uint64 // accepted burst writes with engine down
}

func filePresent(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func fileMtime(path string) (time.Time, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

func cmdDrive(args []string) int {
	fs := flag.NewFlagSet("drive", flag.ContinueOnError)
	base := fs.String("base", ipc.DefaultShmBase, "shm ring base name")
	shard := fs.Uint("shard", 0, "shard id")
	instrument := fs.Uint("instrument", 7, "instrument_id stamped on orders")
	rate := fs.Float64("rate", 1500, "target orders/sec")
	crossPct := fs.Float64("cross-pct", 15, "percent of orders priced through the book")
	accounts := fs.Uint64("accounts", 512, "round-robin account_id space")
	seed := fs.Int64("seed", 42, "PRNG seed")
	orderIDBase := fs.Uint64("order-id-base", 1, "first order_id; ids increment")
	holdFile := fs.String("hold-file", "", "while present, order sends pause (gateway hold)")
	burstFile := fs.String("burst-file", "", "mtime bump fires a one-shot burst")
	burstN := fs.Uint64("burst-n", 1500, "orders per burst trigger")
	sentIDsPath := fs.String("sent-ids", "", "append accepted order_ids here (one per line)")
	eventsPath := fs.String("events", "", "JSONL evidence for every outbound event")
	statusPath := fs.String("status", "", "status JSON refreshed every 200ms")
	reportPath := fs.String("report", "swapdrill-report.json", "final JSON report")
	duration := fs.Duration("duration", 0, "optional run length; 0 = until signal")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	st := &driveStats{}

	// sent-ids journal (the audit compares journaled WAL ids against it).
	var sentF *os.File
	if *sentIDsPath != "" {
		var err error
		sentF, err = os.OpenFile(*sentIDsPath,
			os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sent-ids %s: %v\n", *sentIDsPath, err)
			return 1
		}
		defer func() { _ = sentF.Close() }()
	}
	var evF *os.File
	if *eventsPath != "" {
		var err error
		evF, err = os.OpenFile(*eventsPath,
			os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "events %s: %v\n", *eventsPath, err)
			return 1
		}
		defer func() { _ = evF.Close() }()
	}

	// ---- attach (retry until the engine creates the rings) ---------------
	var ch *ipc.Channel
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})
	var stopOnce atomic.Bool
	requestStop := func() {
		if stopOnce.CompareAndSwap(false, true) {
			close(stop)
		}
	}
	go func() { <-sigCh; requestStop() }()
	if *duration > 0 {
		time.AfterFunc(*duration, requestStop)
	}

	for {
		var err error
		ch, err = ipc.OpenChannel(*base, uint16(*shard), ipc.EndpointGateway,
			false, ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload)
		if err == nil {
			break
		}
		select {
		case <-stop:
			fmt.Fprintf(os.Stderr, "aborted during attach: %v\n", err)
			return 1
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { _ = ch.Close() }()
	st.producerAlive.Store(ch.ProducerAlive())

	// ---- dedup / coverage state (send + drain share the order-id space) --
	seenTradeIDs := map[uint64]struct{}{}
	seenL3Fill := map[[2]uint64]struct{}{} // (order_id, trade_id) Fill legs
	seenFillSeq := map[uint64]struct{}{}
	seenOrderOut := map[uint64]struct{}{}
	outHdrPath := "/dev/shm/" + ipc.OutName(*base, uint16(*shard))

	recordOrder := func(id uint64) {
		if id >= *orderIDBase {
			if _, ok := seenOrderOut[id]; !ok {
				seenOrderOut[id] = struct{}{}
				st.ordersSeenOut.Add(1)
			}
		}
	}

	// ---- drain goroutine: consume _out for the WHOLE run -----------------
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		buf := make([]byte, 64<<10)
		lastEvent := time.Now()
		for {
			n := ch.Poll(buf)
			if n <= 0 {
				if n < 0 {
					st.decodeErrors.Add(1)
				}
				if stopOnce.Load() && time.Since(lastEvent) > 700*time.Millisecond {
					return
				}
				time.Sleep(50 * time.Microsecond)
				continue
			}
			lastEvent = time.Now()
			body, _, _ := tracing.StripAeronTrace(buf[:n])
			ev := ipc.DecodeEvent(body)
			if ev == nil {
				st.decodeErrors.Add(1)
				continue
			}
			rec := map[string]any{"t_ns": time.Now().UnixNano(),
				"type": ev.TypeType().String(), "seq": ev.Seq()}
			switch ev.TypeType() {
			case wire.EventTypeTradeFill:
				tf := ipc.EventTradeFill(ev)
				if tf == nil {
					st.decodeErrors.Add(1)
					continue
				}
				st.fills.Add(1)
				if _, dup := seenTradeIDs[tf.TradeId()]; dup {
					st.dupTradeIDs.Add(1)
				} else {
					seenTradeIDs[tf.TradeId()] = struct{}{}
				}
				if _, dup := seenFillSeq[tf.Seq()]; dup {
					st.dupFillSeqs.Add(1)
				} else {
					seenFillSeq[tf.Seq()] = struct{}{}
				}
				recordOrder(tf.BuyOrderId())
				recordOrder(tf.SellOrderId())
				rec["trade_id"] = tf.TradeId()
				rec["wal_seq"] = tf.Seq()
				rec["buy_order_id"] = tf.BuyOrderId()
				rec["sell_order_id"] = tf.SellOrderId()
			case wire.EventTypeOrderCancel:
				st.cancels.Add(1)
				var t flatbuffers.Table
				if ev.Type(&t) {
					oc := &wire.OrderCancel{}
					oc.Init(t.Bytes, t.Pos)
					recordOrder(oc.OrderId())
					rec["order_id"] = oc.OrderId()
					rec["reason"] = oc.Reason()
				}
			case wire.EventTypeL3OrderEvent:
				var t flatbuffers.Table
				if !ev.Type(&t) {
					st.decodeErrors.Add(1)
					continue
				}
				l3 := &wire.L3OrderEvent{}
				l3.Init(t.Bytes, t.Pos)
				switch l3.Kind() {
				case wire.L3EventKindAdd:
					st.l3Adds.Add(1)
				case wire.L3EventKindFill:
					st.l3Fills.Add(1)
				case wire.L3EventKindCancel:
					st.l3Cancels.Add(1)
				case wire.L3EventKindModify:
					st.l3Modifies.Add(1)
				}
				// Duplicate-execution detector: one TRADE row legitimately
				// yields maker+taker Fill legs (same wal_seq, same
				// trade_id, different order_id). A repeat of the exact
				// (order_id, trade_id) leg is a duplicate execution.
				if l3.Kind() == wire.L3EventKindFill {
					k := [2]uint64{l3.OrderId(), l3.TradeId()}
					if _, dup := seenL3Fill[k]; dup {
						st.dupL3Fills.Add(1)
					} else {
						seenL3Fill[k] = struct{}{}
					}
				}
				// Per-instrument L3 seqs are 1-based and contiguous within
				// an engine generation when the transport dropped nothing.
				// A restart legitimately re-bases the counter (read models
				// resync via snapshot) — count it as a generation reset,
				// not a gap.
				ls := l3.Seq()
				if prev := st.lastL3Seq.Load(); prev != 0 {
					switch {
					case ls == prev+1:
					case ls <= prev:
						st.l3SeqResets.Add(1)
					default:
						st.l3SeqGaps.Add(1)
					}
				}
				st.lastL3Seq.Store(ls)
				recordOrder(l3.OrderId())
				rec["l3_kind"] = l3.Kind().String()
				rec["l3_seq"] = l3.Seq()
				rec["wal_seq"] = l3.WalSeq()
				rec["order_id"] = l3.OrderId()
				rec["trade_id"] = l3.TradeId()
				rec["fill_role"] = l3.FillRole()
			case wire.EventTypeBookSnapshot:
				st.snapshots.Add(1)
			case wire.EventTypeTimeTick:
				st.timeTicks.Add(1)
			default:
				st.otherEvents.Add(1)
			}
			if evF != nil {
				if b, err := json.Marshal(rec); err == nil {
					_, _ = evF.Write(append(b, '\n'))
				}
			}
		}
	}()

	// ---- status/monitor goroutine ----------------------------------------
	monDone := make(chan struct{})
	go func() {
		defer close(monDone)
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				// final status write after stop requested
			case <-t.C:
			}
			st.inOccupancy.Store(ch.Occupancy())
			st.inDrops.Store(ch.Drops())
			st.producerAlive.Store(ch.ProducerAlive())
			if hdr, err := ipc.ReadRingHeader(outHdrPath); err == nil {
				st.outDrops.Store(hdr.Drops)
				st.outOccupancy.Store(hdr.Occupancy())
			}
			if *statusPath != "" {
				snap := map[string]any{
					"t_ns":           time.Now().UnixNano(),
					"sent":           st.sent.Load(),
					"shed":           st.shed.Load(),
					"held":           st.held.Load(),
					"in_occupancy":   st.inOccupancy.Load(),
					"in_drops":       st.inDrops.Load(),
					"out_occupancy":  st.outOccupancy.Load(),
					"out_drops":      st.outDrops.Load(),
					"producer_alive": st.producerAlive.Load(),
					"burst_accepted": st.burstAccepted.Load(),
					"events_seen": st.fills.Load() + st.cancels.Load() +
						st.l3Adds.Load() + st.l3Fills.Load() +
						st.l3Cancels.Load() + st.l3Modifies.Load() +
						st.snapshots.Load() + st.timeTicks.Load() +
						st.otherEvents.Load(),
				}
				if b, err := json.Marshal(snap); err == nil {
					_ = os.WriteFile(*statusPath, b, 0o644)
				}
			}
			if stopOnce.Load() {
				return
			}
		}
	}()

	// ---- send loop ---------------------------------------------------------
	rnd := rand.New(rand.NewSource(*seed))
	b := flatbuffers.NewBuilder(512)
	interval := time.Duration(float64(time.Second) / *rate)
	next := time.Now()
	mid := int64(midStartTicks)
	var seq uint64
	var lastBurstMt time.Time

	writeOne := func(orderID uint64, bursting bool) {
		mid += int64(rnd.Intn(5)-2) * tickSizeTicks
		if mid > midStartTicks+midMaxWanderTicks {
			mid = midStartTicks + midMaxWanderTicks
		} else if mid < midStartTicks-midMaxWanderTicks {
			mid = midStartTicks - midMaxWanderTicks
		}
		var side wire.Side
		var price int64
		var tif wire.TimeInForce
		var qty int64
		if rnd.Intn(2) == 0 {
			side = wire.SideBuy
		} else {
			side = wire.SideSell
		}
		cross := rnd.Float64()*100 < *crossPct
		if cross {
			reach := int64(1+rnd.Intn(crossReachPips)) * pipSizeTicks
			if side == wire.SideBuy {
				price = mid + reach
			} else {
				price = mid - reach
			}
			tif = wire.TimeInForceIOC
			qty = lotQtyUnits
		} else {
			off := int64(1+rnd.Intn(passiveBandPips)) * pipSizeTicks
			if side == wire.SideBuy {
				price = mid - off
			} else {
				price = mid + off
			}
			tif = wire.TimeInForceGTC
			qty = int64(1+rnd.Intn(100)) * lotQtyUnits
		}
		b.Reset()
		msg := ipc.EncodeOrderNewEvent(b, orderID-*orderIDBase+1,
			uint64(time.Now().UnixNano()), ipc.OrderNewMsg{
				OrderID:       orderID,
				AccountID:     1 + (orderID-*orderIDBase)%*accounts,
				InstrumentID:  uint32(*instrument),
				Side:          side,
				Type:          wire.OrderTypeLimit,
				Qty:           qty,
				Price:         price,
				TIF:           tif,
				ClientOrderID: fmt.Sprintf("swapdrill-%d", orderID),
			})
		cp := make([]byte, len(msg))
		copy(cp, msg)
		ok := false
		for tries := 0; tries < 64; tries++ {
			if ch.Send(cp) {
				ok = true
				break
			}
		}
		if !ok {
			if bursting {
				st.burstShed.Add(1)
			} else {
				st.shed.Add(1)
			}
			return
		}
		if bursting {
			st.burstAccepted.Add(1)
			// Synchronous probe — the monitor flag is 200ms-stale and the
			// burst window is deliberately tight around the kill.
			if !ch.ProducerAlive() {
				st.burstWhileDead.Add(1)
			}
		} else {
			st.sent.Add(1)
		}
		seq++
		if sentF != nil {
			_, _ = fmt.Fprintf(sentF, "%d\n", orderID)
		}
	}

	for {
		select {
		case <-stop:
			goto done
		default:
		}
		// Burst trigger takes precedence — it models "ingress keeps writing
		// while the engine process is down; shm ring buffers".
		if *burstFile != "" {
			if mt, ok := fileMtime(*burstFile); ok && mt.After(lastBurstMt) {
				lastBurstMt = mt
				st.burstsFired.Add(1)
				for i := uint64(0); i < *burstN; i++ {
					writeOne(*orderIDBase+seq, true)
				}
				continue
			}
		}
		if filePresent(*holdFile) {
			st.held.Add(1)
			time.Sleep(time.Millisecond)
			continue
		}
		if d := time.Until(next); d > 0 {
			if d > 2*time.Millisecond {
				time.Sleep(d - time.Millisecond)
			}
			continue
		}
		next = next.Add(interval)
		if time.Since(next) > time.Second {
			next = time.Now() // rebase after a stall — no catch-up burst
		}
		writeOne(*orderIDBase+seq, false)
	}
done:

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
	}
	<-monDone
	if sentF != nil {
		_ = sentF.Sync()
	}
	if evF != nil {
		_ = evF.Sync()
	}

	rep := map[string]any{
		"orders_sent":             st.sent.Load(),
		"burst_accepted":          st.burstAccepted.Load(),
		"burst_shed":              st.burstShed.Load(),
		"burst_while_engine_dead": st.burstWhileDead.Load(),
		"bursts_fired":            st.burstsFired.Load(),
		"send_shed":               st.shed.Load(),
		"held_paced_slots":        st.held.Load(),
		"ring_accepted_total":     st.sent.Load() + st.burstAccepted.Load(),
		"fills":                   st.fills.Load(),
		"cancels":                 st.cancels.Load(),
		"l3_adds":                 st.l3Adds.Load(),
		"l3_fills":                st.l3Fills.Load(),
		"l3_cancels":              st.l3Cancels.Load(),
		"l3_modifies":             st.l3Modifies.Load(),
		"book_snapshots":          st.snapshots.Load(),
		"time_ticks":              st.timeTicks.Load(),
		"other_events":            st.otherEvents.Load(),
		"decode_errors":           st.decodeErrors.Load(),
		"dup_trade_ids":           st.dupTradeIDs.Load(),
		"dup_l3_fills":            st.dupL3Fills.Load(),
		"dup_fill_seqs":           st.dupFillSeqs.Load(),
		"l3_seq_gaps":             st.l3SeqGaps.Load(),
		"l3_seq_resets":           st.l3SeqResets.Load(),
		"orders_seen_outbound":    st.ordersSeenOut.Load(),
		"in_ring_drops":           st.inDrops.Load(),
		"out_ring_drops":          st.outDrops.Load(),
		"shard":                   *shard,
		"instrument_id":           *instrument,
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*reportPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr,
		"swapdrill: sent=%d burst=%d shed=%d fills=%d dup_tid=%d dup_l3fill=%d l3_gaps=%d report=%s\n",
		st.sent.Load(), st.burstAccepted.Load(), st.shed.Load(),
		st.fills.Load(), st.dupTradeIDs.Load(), st.dupL3Fills.Load(),
		st.l3SeqGaps.Load(), *reportPath)
	return 0
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

type auditReport struct {
	Dir           string           `json:"dir"`
	Segments      int              `json:"segments"`
	Entries       uint64           `json:"entries"`
	FirstSeq      uint64           `json:"first_seq"`
	LastSeq       uint64           `json:"last_seq"`
	SeqGaps       [][2]uint64      `json:"seq_gaps"`
	SeqOverlaps   [][2]uint64      `json:"seq_overlaps"`
	Corrupt       int              `json:"corrupt_segments"`
	ByType        map[string]int64 `json:"by_type"`
	OrderIDs      uint64           `json:"journaled_order_ids"`
	TradeIDs      uint64           `json:"journaled_trade_ids"`
	DupTradeIDs   uint64           `json:"dup_trade_ids"`
	ExpectedIDs   uint64           `json:"expected_order_ids"`
	MissingCount  uint64           `json:"missing_order_count"`
	MissingIDs    []uint64         `json:"missing_order_ids"`
	UnexpectedIDs []uint64         `json:"unexpected_order_ids"`
}

func cmdAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	dir := fs.String("wal-dir", "", "shard WAL segment directory (required)")
	expectPath := fs.String("expect-ids", "", "file of expected order_ids")
	maxList := fs.Int("max-missing", 100, "cap on listed missing/unexpected ids")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "--wal-dir required")
		return 2
	}

	ents, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit: %v\n", err)
		return 1
	}
	rep := auditReport{Dir: *dir, ByType: map[string]int64{}}
	orderIDs := map[uint64]struct{}{}
	tradeIDs := map[uint64]struct{}{}
	var allSeqs []uint64
	var files []string
	for _, de := range ents {
		if de.IsDir() || len(de.Name()) < 4 ||
			de.Name()[len(de.Name())-4:] != ".wal" {
			continue
		}
		files = append(files, de.Name())
	}
	sort.Strings(files)
	for _, name := range files {
		data, err := os.ReadFile(*dir + "/" + name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit: %v\n", err)
			return 1
		}
		res, entries, err := recovery.ScanSegment(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit: scan %s: %v\n", name, err)
			return 1
		}
		rep.Segments++
		rep.Entries += res.Entries
		if res.Corrupt {
			rep.Corrupt++
		}
		for _, e := range entries {
			allSeqs = append(allSeqs, e.Seq)
			rep.ByType[e.Type.String()]++
			switch e.Type {
			case recovery.EvOrderNew:
				if p, derr := recovery.DecodeOrderNew(e.Payload); derr == nil {
					orderIDs[p.OrderID] = struct{}{}
				}
			case recovery.EvTrade:
				if p, derr := recovery.DecodeTrade(e.Payload); derr == nil {
					if _, dup := tradeIDs[p.TradeID]; dup {
						rep.DupTradeIDs++
					}
					tradeIDs[p.TradeID] = struct{}{}
				}
			}
		}
	}
	rep.OrderIDs = uint64(len(orderIDs))
	rep.TradeIDs = uint64(len(tradeIDs))

	// Global seq continuity: sort all entry seqs, then walk the deduped
	// stream — a repeated seq is an overlap (divergent journal copy), a
	// forward jump is a gap (entries lost).
	sort.Slice(allSeqs, func(i, j int) bool { return allSeqs[i] < allSeqs[j] })
	if len(allSeqs) > 0 {
		rep.FirstSeq, rep.LastSeq = allSeqs[0], allSeqs[len(allSeqs)-1]
		prev := allSeqs[0]
		for _, s := range allSeqs[1:] {
			switch {
			case s == prev:
				rep.SeqOverlaps = append(rep.SeqOverlaps, [2]uint64{s, s})
			case s < prev:
				rep.SeqOverlaps = append(rep.SeqOverlaps, [2]uint64{prev, s})
			case s > prev+1:
				rep.SeqGaps = append(rep.SeqGaps, [2]uint64{prev + 1, s - 1})
			}
			prev = s
		}
	}

	if *expectPath != "" {
		raw, err := os.ReadFile(*expectPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit: expect-ids %s: %v\n", *expectPath, err)
			return 1
		}
		var want []uint64
		for _, ln := range splitLines(string(raw)) {
			if ln == "" {
				continue
			}
			v, perr := strconv.ParseUint(ln, 10, 64)
			if perr != nil {
				continue
			}
			want = append(want, v)
		}
		wantSet := map[uint64]struct{}{}
		for _, id := range want {
			wantSet[id] = struct{}{}
		}
		rep.ExpectedIDs = uint64(len(wantSet))
		missing := uint64(0)
		for _, id := range want {
			if _, ok := orderIDs[id]; !ok {
				missing++
				if len(rep.MissingIDs) < *maxList {
					rep.MissingIDs = append(rep.MissingIDs, id)
				}
			}
		}
		rep.MissingCount = missing
		for id := range orderIDs {
			if _, ok := wantSet[id]; !ok {
				if len(rep.UnexpectedIDs) < *maxList {
					rep.UnexpectedIDs = append(rep.UnexpectedIDs, id)
				}
			}
		}
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if rep.Corrupt > 0 || len(rep.SeqGaps) > 0 || len(rep.SeqOverlaps) > 0 ||
		rep.DupTradeIDs > 0 || rep.MissingCount > 0 {
		return 1
	}
	return 0
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr,
			"usage: swapdrill <drive|audit> [flags]")
		os.Exit(2)
	}
	var rc int
	switch os.Args[1] {
	case "drive":
		rc = cmdDrive(os.Args[2:])
	case "audit":
		rc = cmdAudit(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		rc = 2
	}
	os.Exit(rc)
}
