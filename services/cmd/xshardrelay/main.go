// xshardrelay — cross-shard ctl frame router (IMP-PLAN Phase-3 Task 4,
// spec §2.2a/§13.1).
//
// Each matching engine publishes its outbound ctl frames on
// aeron:ipc?alias=xshard_ctl_out_{shard} (stream 1201) and polls
// xshard_ctl_in_{shard} (stream 1202). Aeron IPC is point-to-point per
// channel alias — this relay subscribes every configured shard's _out
// channel, reads CrossShardCtlHeader.dst_shard (u32, LE, offset 12), and
// republishes the frame onto the destination shard's _in channel. Frames
// carrying an unknown magic are counted and dropped — the relay never
// guesses at foreign protocols.
//
// Point-to-point delivery is load-bearing: the engine coordinators filter
// on dst_shard but a frame delivered to two shards would double-execute a
// leg. dst_shard outside the configured shard set drops loudly.
//
// Deployment: one relay per host pair covered by aeron:ipc (same media
// driver). Cross-host topologies need aeron:udp URIs — set
// EXC_XSHARD_OUT_URI_TMPL / EXC_XSHARD_IN_URI_TMPL accordingly (the {shard}
// placeholder is substituted per shard).

package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	ipcaeron "exchange/internal/ipc/aeron"
)

const (
	// Magic values mirror core/include/matching/*ShardCoordinator.hpp.
	basketCtlMagic = 0x32424858 // "XHB2" — basket 2PC (Task 2.3.8)
	optCtlMagic    = 0x4F484F58 // "XOHO" — optimistic TRY_MATCH (2.3.25)

	ctlHeaderLen    = 16 // CrossShardCtlHeader
	dstShardOffset  = 12 // u32 dst_shard
	xshardOutStream = 1201
	xshardInStream  = 1202
)

var (
	outURITmpl = "aeron:ipc?alias=xshard_ctl_out_%d"
	inURITmpl  = "aeron:ipc?alias=xshard_ctl_in_%d"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	shardsFlag := flag.String("shards", os.Getenv("EXC_XSHARD_SHARDS"),
		"comma-separated shard ids, e.g. 0,1,2 (or EXC_XSHARD_SHARDS)")
	flag.Parse()

	var shards []uint32
	for _, tok := range strings.Split(*shardsFlag, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		n, err := strconv.ParseUint(tok, 10, 32)
		if err != nil {
			log.Error("xshardrelay: bad shard id", "token", tok, "err", err)
			os.Exit(2)
		}
		shards = append(shards, uint32(n))
	}
	if len(shards) == 0 {
		log.Error("xshardrelay: no shards configured (-shards / EXC_XSHARD_SHARDS)")
		os.Exit(2)
	}
	if v := os.Getenv("EXC_XSHARD_OUT_URI_TMPL"); v != "" {
		outURITmpl = v
	}
	if v := os.Getenv("EXC_XSHARD_IN_URI_TMPL"); v != "" {
		inURITmpl = v
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	aero, err := ipcaeron.Connect(os.Getenv("EXC_AERON_DIR"), 5000)
	if err != nil {
		log.Error("xshardrelay: aeron connect", "err", err)
		os.Exit(1)
	}
	defer aero.Close()

	// One publication per destination shard — the pubs map doubles as the
	// configured-shard set.
	pubs := map[uint32]*ipcaeron.Publication{}
	for _, s := range shards {
		pub, err := aero.AddPublication(fmt.Sprintf(inURITmpl, s),
			xshardInStream, 5*time.Second)
		if err != nil {
			log.Error("xshardrelay: publish in-channel", "shard", s, "err", err)
			os.Exit(1)
		}
		pubs[s] = pub
	}

	// Counters are bumped from one poll goroutine per shard — atomics.
	var routed, dropped atomic.Uint64
	for _, src := range shards {
		src := src
		sub, err := aero.AddSubscription(fmt.Sprintf(outURITmpl, src),
			xshardOutStream, func(buf []byte) {
				if len(buf) < ctlHeaderLen {
					dropped.Add(1)
					log.Warn("xshardrelay: short frame dropped",
						"src", src, "len", len(buf))
					return
				}
				magic := binary.LittleEndian.Uint32(buf[0:4])
				if magic != basketCtlMagic && magic != optCtlMagic {
					dropped.Add(1)
					log.Warn("xshardrelay: unknown ctl magic dropped",
						"src", src, "magic", fmt.Sprintf("0x%08x", magic))
					return
				}
				dst := binary.LittleEndian.Uint32(buf[dstShardOffset : dstShardOffset+4])
				pub, ok := pubs[dst]
				if !ok {
					dropped.Add(1)
					log.Warn("xshardrelay: unroutable dst shard",
						"src", src, "dst", dst)
					return
				}
				// Copy: the fragment aliases the Aeron log buffer only for
				// the callback duration.
				cp := make([]byte, len(buf))
				copy(cp, buf)
				if rc := pub.Offer(cp); rc <= 0 {
					dropped.Add(1)
					log.Error("xshardrelay: offer failed",
						"src", src, "dst", dst, "rc", rc)
					return
				}
				routed.Add(1)
			}, 5*time.Second)
		if err != nil {
			log.Error("xshardrelay: subscribe out-channel", "shard", src, "err", err)
			os.Exit(1)
		}
		defer sub.Close()
		go func(s *ipcaeron.Subscription, shard uint32) {
			for ctx.Err() == nil {
				if n := s.Poll(10); n < 0 {
					log.Error("xshardrelay: poll error", "shard", shard)
					return
				}
			}
		}(sub, src)
	}

	log.Info("xshardrelay: routing ctl frames", "shards", shards,
		"streams", fmt.Sprintf("%d/%d", xshardInStream, xshardOutStream))
	<-ctx.Done()
	log.Info("xshardrelay: stopped", "routed", routed.Load(), "dropped", dropped.Load())
}
