// bookpump — Phase-08 Task 8.3.2 marketdata-leg order injector.
//
// Writes a paced stream of FlatBuffers Event{OrderNew} messages into a
// shard's _in ring ONLY — unlike tests/soak/loadgen it never touches the
// _out ring, leaving the SPSC outbound consumer slot free for the real
// marketdata delta source (EXC_MARKETDATA_SOURCE=ipc). That is the point
// of this tool: the load-test WS leg needs a live book to fan out while
// the marketdata service is the sole _out consumer.
//
// Order flow mirrors the soak loadgen's price model (EURUSD-style random
// walk at the 10^8 pipette scale, 1'000-tick grid) minus all accounting:
// no latency histograms, no dup detection — just paced writes.
//
//	bookpump -base NAME -shard N -instrument ID -rate 2000 \
//	    -duration 60s -cross-pct 10 -accounts 1000
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
)

// Instrument ticks (10^8 scale) — identical constants to tests/soak's
// loadgen so the two tools generate compatible flow on different shards.
const (
	tickSizeTicks     = 1_000
	pipSizeTicks      = 10_000
	midStartTicks     = 108_500_000
	midMaxWanderTicks = 1_000_000
	passiveBandPips   = 20
	crossReachPips    = 25
	lotQtyUnits       = 100_000 * 100_000_000
)

func parseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("not a duration or seconds: %q", s)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func main() {
	var (
		base        = flag.String("base", ipc.DefaultShmBase, "shm ring base name")
		shard       = flag.Uint("shard", 0, "shard id")
		instrument  = flag.Uint("instrument", 7, "instrument_id stamped on orders")
		rate        = flag.Float64("rate", 2000, "target orders/sec")
		durationRaw = flag.String("duration", "0", "Go duration or bare seconds; 0 = until signal")
		orderIDBase = flag.Uint64("order-id-base", 1, "first order_id; ids increment")
		accounts    = flag.Uint64("accounts", 1000, "round-robin account_id space")
		crossPct    = flag.Float64("cross-pct", 10, "percent of orders priced through the book")
		seed        = flag.Int64("seed", 42, "PRNG seed")
	)
	flag.Parse()

	duration, err := parseDuration(*durationRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad -duration %q: %v\n", *durationRaw, err)
		os.Exit(2)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})
	var durTimer *time.Timer
	if duration > 0 {
		durTimer = time.AfterFunc(duration, func() { close(stop) })
		defer durTimer.Stop()
	}
	go func() {
		<-sigCh
		select {
		case <-stop:
		default:
			close(stop)
		}
	}()

	// Attach (retry until the engine's rings exist). Gateway endpoint so
	// Send() writes _in; we deliberately never Poll() — the marketdata
	// service owns _out on this shard.
	var ch *ipc.Channel
	for {
		if c, aerr := ipc.OpenChannel(*base, uint16(*shard), ipc.EndpointGateway,
			false, ipc.DefaultRingCapacity, ipc.DefaultRingSlotPayload); aerr == nil {
			ch = c
			break
		} else {
			fmt.Fprintf(os.Stderr, "attach %s_%d: %v (retrying)\n", *base, *shard, aerr)
		}
		select {
		case <-stop:
			os.Exit(2)
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { _ = ch.Close() }()

	rnd := rand.New(rand.NewSource(*seed))
	b := flatbuffers.NewBuilder(512)
	pid := os.Getpid()
	mid := int64(midStartTicks)

	interval := time.Duration(float64(time.Second) / *rate)
	var seq, sent, drops uint64
	next := time.Now()
	for {
		select {
		case <-stop:
			fmt.Fprintf(os.Stderr, "bookpump stopped: sent=%d drops=%d\n", sent, drops)
			return
		default:
		}
		next = next.Add(interval)
		for {
			rem := time.Until(next)
			if rem <= 0 {
				break
			}
			if rem > time.Millisecond {
				time.Sleep(rem - time.Millisecond)
				continue
			}
			runtime.Gosched()
		}

		mid += int64(rnd.Intn(5)-2) * tickSizeTicks
		if mid > midStartTicks+midMaxWanderTicks {
			mid = midStartTicks + midMaxWanderTicks
		} else if mid < midStartTicks-midMaxWanderTicks {
			mid = midStartTicks - midMaxWanderTicks
		}

		seq++
		var side wire.Side
		if rnd.Intn(2) == 0 {
			side = wire.SideBuy
		} else {
			side = wire.SideSell
		}
		var price int64
		var tif wire.TimeInForce
		var qty int64
		if rnd.Float64()*100 < *crossPct {
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
		msg := ipc.EncodeOrderNewEvent(b, seq, uint64(time.Now().UnixNano()),
			ipc.OrderNewMsg{
				OrderID:       *orderIDBase + seq - 1,
				AccountID:     1 + (seq-1)%*accounts,
				InstrumentID:  uint32(*instrument),
				Side:          side,
				Type:          wire.OrderTypeLimit,
				Qty:           qty,
				Price:         price,
				TIF:           tif,
				ClientOrderID: fmt.Sprintf("pump-%d-%d", pid, seq),
			})
		ok := false
		for tries := 0; tries < 64; tries++ {
			if ch.Send(msg) {
				ok = true
				break
			}
			runtime.Gosched()
		}
		if ok {
			sent++
		} else {
			drops++
		}
		if time.Since(next) > time.Second { // stall: rebase, don't burst
			next = time.Now()
		}
	}
}
