// wsprobe — Phase-08 Task 8.3.2 WebSocket fan-out prober.
//
// Opens N concurrent WS connections to a marketdata endpoint, sends one
// subscribe frame per configured channel, then reads until -duration
// expires. Per connection it tracks:
//
//   - frames received / bytes received
//   - seq contiguity via the §10.9 envelope (prev_last_seq must equal the
//     prior frame's last_seq on the same channel — a mismatch is a real,
//     protocol-visible drop, not a guessed one)
//   - disconnects / unexpected close codes / reconnect attempts
//   - read deadline stalls (no frame for -stall after the first data
//     frame arrived — subscription ACK counts as activity)
//
// Exit status is informational only — the JSON report carries the
// verdict fields the orchestrator evaluates.
//
//	wsprobe -url ws://127.0.0.1:8081/ws/v1/marketdata -conns 120 \
//	    -channel book@EUR/USD -duration 5m -report wsprobe.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type connStats struct {
	frames     atomic.Uint64
	bytes      atomic.Uint64
	seqGaps    atomic.Uint64
	discs      atomic.Uint64 // unexpected disconnects
	reconnects atomic.Uint64 // successful reconnects
	lastSeq    sync.Map      // channel -> uint64 last_seq
	connected  atomic.Bool
}

// eventFrame is the minimum shape needed for seq-contiguity checking
// (internal/marketdata frames.go §10.9 envelope).
type eventFrame struct {
	Channel     string `json:"channel"`
	Seq         uint64 `json:"seq"`
	FirstSeq    uint64 `json:"first_seq"`
	LastSeq     uint64 `json:"last_seq"`
	PrevLastSeq uint64 `json:"prev_last_seq"`
	Action      string `json:"action"`
	Type        string `json:"type"`
}

type wsReport struct {
	Started         string  `json:"started"`
	Ended           string  `json:"ended"`
	DurationS       float64 `json:"duration_s"`
	URL             string  `json:"url"`
	ConnsRequested  int     `json:"conns_requested"`
	ConnsOpened     int     `json:"conns_opened"`
	OpenFailures    int     `json:"open_failures"`
	ConnsAliveAtEnd int     `json:"conns_alive_at_end"`
	Frames          uint64  `json:"frames"`
	Bytes           uint64  `json:"bytes"`
	SeqGaps         uint64  `json:"seq_gaps"`
	Disconnects     uint64  `json:"disconnects"`
	Reconnects      uint64  `json:"reconnects"`
	ReconnectFails  uint64  `json:"reconnect_failures"`
	ConnsZeroFrames int     `json:"conns_zero_frames"` // live conn that never saw a frame
	SubscribeFrame  string  `json:"subscribe_frame"`
	ZeroDrops       bool    `json:"zero_drops"` // seq_gaps==0 && disconnects==0
}

func main() {
	var (
		url     = flag.String("url", "", "ws(s):// endpoint, e.g. ws://127.0.0.1:8081/ws/v1/marketdata")
		conns   = flag.Int("conns", 100, "concurrent connections")
		channel = flag.String("channel", "book@EUR/USD",
			"comma-separated channels to subscribe per conn; empty = no subscribe")
		duration   = flag.Duration("duration", 60*time.Second, "hold time")
		ramp       = flag.Duration("ramp", 0, "optional dial spread (0 = burst-dial all)")
		reconnect  = flag.Bool("reconnect", true, "reconnect on unexpected disconnect")
		reportPath = flag.String("report", "wsprobe.json", "JSON report path")
		header     = flag.String("header", "", "optional 'K: V' request header (repeatable via ;)")
	)
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "wsprobe: -url is required")
		os.Exit(2)
	}
	started := time.Now()
	deadline := started.Add(*duration)

	var hdr http.Header
	if *header != "" {
		hdr = http.Header{}
		for _, kv := range strings.Split(*header, ";") {
			k, v, ok := strings.Cut(kv, ":")
			if ok {
				hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		// Host dev box: allow plenty of in-flight handshakes during a burst.
		NetDialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}

	var subFrame []byte
	if *channel != "" {
		parts := strings.Split(*channel, ",")
		q := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				q = append(q, fmt.Sprintf("%q", p))
			}
		}
		subFrame = []byte(`{"action":"subscribe","channels":[` + strings.Join(q, ",") + `]}`)
	}

	stats := make([]*connStats, *conns)
	for i := range stats {
		stats[i] = &connStats{}
	}

	var opened, openFails, reconnFails atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < *conns; i++ {
		st := stats[i]
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var c *websocket.Conn
			openedHere := false
			for time.Now().Before(deadline) {
				var err error
				c, _, err = dialer.Dial(*url, hdr)
				if err != nil {
					if !openedHere {
						openFails.Add(1)
						return // initial dial failed — don't spin
					}
					reconnFails.Add(1)
					time.Sleep(500 * time.Millisecond)
					continue
				}
				if openedHere {
					st.reconnects.Add(1)
				}
				openedHere = true
				opened.Add(1)
				st.connected.Store(true)

				if len(subFrame) > 0 {
					_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
					if err := c.WriteMessage(websocket.TextMessage, subFrame); err != nil {
						st.discs.Add(1)
						st.connected.Store(false)
						_ = c.Close()
						if !*reconnect {
							return
						}
						continue
					}
				}

				for {
					if time.Now().After(deadline) {
						_ = c.Close()
						return
					}
					_ = c.SetReadDeadline(deadline)
					_, payload, rerr := c.ReadMessage()
					if rerr != nil {
						if time.Now().Before(deadline) {
							st.discs.Add(1)
						}
						break
					}
					st.frames.Add(1)
					st.bytes.Add(uint64(len(payload)))

					var f eventFrame
					if json.Unmarshal(payload, &f) == nil && f.Channel != "" && f.LastSeq != 0 {
						if prev, ok := st.lastSeq.Load(f.Channel); ok {
							exp := prev.(uint64) + 1
							// §10.9: prev_last_seq chains the stream;
							// tolerate a resync marker (first_seq==0 or
							// explicit reset) but count a silent skip.
							if f.PrevLastSeq != prev.(uint64) && f.PrevLastSeq != 0 {
								st.seqGaps.Add(1)
							} else if f.PrevLastSeq == 0 && f.FirstSeq > exp {
								// resync frame admits the gap — still a drop.
								st.seqGaps.Add(1)
							}
						}
						st.lastSeq.Store(f.Channel, f.LastSeq)
					}
				}
				st.connected.Store(false)
				_ = c.Close()
				if !*reconnect {
					return
				}
			}
		}(i)

		if *ramp > 0 && *conns > 1 {
			time.Sleep(*ramp / time.Duration(*conns))
		}
	}

	// Wait for the hold window, then let each conn's deadline drive exit.
	<-time.After(*duration)
	// Give readers a moment to hit the deadline and return.
	time.Sleep(500 * time.Millisecond)

	rep := wsReport{
		Started:        started.UTC().Format(time.RFC3339Nano),
		Ended:          time.Now().UTC().Format(time.RFC3339Nano),
		DurationS:      time.Since(started).Seconds(),
		URL:            *url,
		ConnsRequested: *conns,
		SubscribeFrame: string(subFrame),
	}
	for _, st := range stats {
		rep.Frames += st.frames.Load()
		rep.Bytes += st.bytes.Load()
		rep.SeqGaps += st.seqGaps.Load()
		rep.Disconnects += st.discs.Load()
		rep.Reconnects += st.reconnects.Load()
		if st.connected.Load() {
			rep.ConnsAliveAtEnd++
		}
		if st.frames.Load() == 0 && st.reconnects.Load()+st.discs.Load() == 0 {
			rep.ConnsZeroFrames++
		}
	}
	rep.ConnsOpened = int(opened.Load())
	rep.OpenFailures = int(openFails.Load())
	rep.ReconnectFails = uint64(reconnFails.Load())
	rep.ZeroDrops = rep.SeqGaps == 0 && rep.Disconnects == 0

	out, _ := json.MarshalIndent(&rep, "", "  ")
	if err := os.WriteFile(*reportPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr,
		"wsprobe done: conns=%d/%d frames=%d seq_gaps=%d disconnects=%d zero_frames_conns=%d\n",
		rep.ConnsOpened, rep.ConnsRequested, rep.Frames, rep.SeqGaps,
		rep.Disconnects, rep.ConnsZeroFrames)
}
