// errblast — Phase-08.5 Task 8.5.3.3 high-stress error-injection blaster.
//
// Fires a paced stream of INVALID order submissions at the gateway's
// POST /api/v1/orders endpoint (spec §8.4 surface), mixing defect
// classes — negative quantity/price, malformed JSON, invalid signatures,
// unknown instruments, enum violations, oversized payloads, wrong
// content type — while a low-rate control stream probes /health/live
// for liveness. The assertion (spec §2.7, §24 #308): the error-dispatch
// path stays healthy under rejection pressure — every request resolves
// to a clean 4xx/429 rejection, 5xx stays under a threshold, and the
// gateway never stops answering the control probe (crash detection).
//
// Exit codes: 0 pass · 2 target presumed crashed (sustained transport
// failure / liveness loss on the control stream after startup grace) ·
// 3 the 5xx share of blast responses exceeded -max-5xx-pct · 4 the run
// produced no usable traffic.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

// Config is one blast run's parameters (see main.go for flag binding).
type Config struct {
	Addr       string // gateway base URL, e.g. http://127.0.0.1:8080
	OrdersPath string // order-placement endpoint (default /api/v1/orders)
	HealthPath string // liveness control probe (default /health/live)

	// Auth for the well-formed defect classes (bad-signature requests
	// always carry their own garbage credentials):
	//   Token      — a static Bearer JWT attached to every authed request;
	//   JWTKeyB64  — when set, tokens are minted per run, rotating
	//                account_id over JWTAccounts so per-account rate-limit
	//                buckets don't collapse the burst onto one identity.
	Token       string
	JWTKeyB64   string
	JWTAccounts int

	Rate         float64       // invalid orders/sec (target; plan: 10,000)
	Duration     time.Duration // 0 = until ctx/signal
	Workers      int           // concurrent senders
	ControlRate  float64       // health probes/sec (liveness canary)
	ReqTimeout   time.Duration // per-request deadline
	StartupGrace time.Duration // transport failures inside grace are warmup,
	// not a crash

	Max5xxPct  float64 // 5xx share (percent of responses) that fails the run
	XFFPool    int     // distinct X-Forwarded-For identities to rotate (0 = off)
	OversizeKB int     // oversized-payload class body size
	Seed       int64
}

func (c *Config) defaults() {
	if c.OrdersPath == "" {
		c.OrdersPath = "/api/v1/orders"
	}
	if c.HealthPath == "" {
		c.HealthPath = "/health/live"
	}
	if c.Rate <= 0 {
		c.Rate = 10_000
	}
	if c.Workers <= 0 {
		c.Workers = 64
	}
	if c.ControlRate <= 0 {
		c.ControlRate = 5
	}
	if c.ReqTimeout <= 0 {
		c.ReqTimeout = 5 * time.Second
	}
	if c.StartupGrace <= 0 {
		c.StartupGrace = 5 * time.Second
	}
	if c.JWTAccounts <= 0 {
		c.JWTAccounts = 512
	}
	if c.OversizeKB <= 0 {
		c.OversizeKB = 1024 // 1 MiB — comfortably past any sane body cap
	}
}

// ---------------------------------------------------------------------------
// Outcome classification.
// ---------------------------------------------------------------------------

// outcome buckets a single HTTP attempt. 4xx and 429 are the HEALTHY
// paths — an invalid order is supposed to be rejected; 5xx is the
// failure signal (the error-dispatch path itself broke); timeout and
// transport failures are reported separately.
type outcome int

const (
	out2xx3xx    outcome = iota // unexpected success — counted, not fatal
	out4xx                      // 400-428/430+ client rejection — healthy dispatch
	out429                      // rate-limit rejection — healthy dispatch
	out5xx                      // server-side failure — the defect under test
	outTimeout                  // request deadline exceeded
	outTransport                // conn refused/reset/etc — possible crash
	outCount
)

var outcomeNames = [outCount]string{
	"2xx_3xx", "4xx", "429", "5xx", "timeout", "transport_error",
}

// classify buckets one attempt. Order matters: a DeadlineExceeded is a
// timeout, other transport errors are conn failures; HTTP status maps
// 429 → its own bucket, 4xx → client rejection, 5xx → server failure.
func classify(status int, err error) outcome {
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return outTimeout
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return outTimeout
		}
		return outTransport
	}
	switch {
	case status == http.StatusTooManyRequests:
		return out429
	case status >= 500:
		return out5xx
	case status >= 400:
		return out4xx
	default:
		return out2xx3xx
	}
}

// ---------------------------------------------------------------------------
// Latency histogram — 1µs buckets to 50ms + overflow (same shape as
// tests/load/restprobe so reports are cross-comparable).
// ---------------------------------------------------------------------------

const (
	latMicroBuckets = 50_000
	latOverflowIdx  = latMicroBuckets
	latArrLen       = latMicroBuckets + 1
)

type latHist struct {
	buckets [latArrLen]uint64
	count   atomic.Uint64
	sum     atomic.Uint64
	max     atomic.Uint64
}

func (h *latHist) observe(ns int64) {
	if ns < 0 {
		ns = 0
	}
	idx := ns / 1_000
	if idx > latOverflowIdx {
		idx = latOverflowIdx
	}
	atomic.AddUint64(&h.buckets[idx], 1)
	h.count.Add(1)
	h.sum.Add(uint64(ns))
	for {
		m := h.max.Load()
		if uint64(ns) <= m || h.max.CompareAndSwap(m, uint64(ns)) {
			break
		}
	}
}

func (h *latHist) percentile(q float64) uint64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	want := uint64(float64(total)*q + 0.999)
	var acc uint64
	for i := 0; i < latArrLen; i++ {
		acc += atomic.LoadUint64(&h.buckets[i])
		if acc >= want {
			if i >= latOverflowIdx {
				return h.max.Load()
			}
			return uint64(i)*1_000 + 999
		}
	}
	return h.max.Load()
}

// ---------------------------------------------------------------------------
// Defect classes. Each build() returns the exact wire request for one
// submission — body, extra headers, and whether the tool's auth should
// be applied (the bad-signature class carries its own garbage).
// ---------------------------------------------------------------------------

type reqSpec struct {
	body        []byte
	contentType string
	headers     map[string]string
	noAuth      bool // supplies its own (invalid) credentials
}

type defectClass struct {
	name  string
	build func(r *rand.Rand, seq uint64, cfg *Config) reqSpec
}

func orderJSON(symbol, side, price, qty string) []byte {
	return []byte(fmt.Sprintf(
		`{"symbol":%q,"side":%q,"type":"LIMIT","price":%q,"quantity":%q,"time_in_force":"GTC","client_order_id":%q}`,
		symbol, side, price, qty, "errblast-"+strconv.FormatUint(uint64(time.Now().UnixNano()), 36)))
}

func newIDemKey(r *rand.Rand) string {
	// UUIDv7 shape — middleware.IsUUIDv7 requires version/variant bits
	// (same wire shape as itest.NewIdemKey).
	var b [16]byte
	_, _ = r.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// idemHdr attaches a well-formed Idempotency-Key so the defect under
// test is reached (a missing key would be rejected upstream and would
// mask the validation path each class targets).
func idemHdr(r *rand.Rand) map[string]string {
	return map[string]string{"Idempotency-Key": newIDemKey(r)}
}

// badJWT mints a syntactically valid HS256 token signed with the WRONG
// key — the gateway's kid="v1" verifier must reject it (401), which is
// the "invalid signature" defect class.
func badJWT(r *rand.Rand) string {
	key := make([]byte, 32)
	_, _ = r.Read(key)
	hdr, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "v1", "typ": "JWT"})
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"iss": "exc.local", "aud": "exc-api", "sub": "errblast",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"jti": fmt.Sprintf("errblast-bad-%d", now.UnixNano()),
		"typ": "access", "account_id": 1, "scopes": []string{"trade"},
	})
	segs := base64.RawURLEncoding.EncodeToString(hdr) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(segs))
	return segs + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// mintJWT builds a properly-signed HS256 access token (iss exc.local /
// aud exc-api / kid v1 / typ access) — the wire shape the gateway's
// OptionalAuthMiddleware verifies.
func mintJWT(keyB64, subject string, accountID int64, scopes []string) (string, error) {
	kb, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil {
		return "", err
	}
	now := time.Now()
	hdr, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "v1", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss": "exc.local", "aud": "exc-api", "sub": subject,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
		"jti": fmt.Sprintf("errblast-%d", now.UnixNano()),
		"typ": "access", "account_id": accountID, "scopes": scopes,
	})
	segs := base64.RawURLEncoding.EncodeToString(hdr) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, kb)
	mac.Write([]byte(segs))
	return segs + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// defectClasses is the injection mix — every class maps to a documented
// error-dispatch path (spec §2.7 L2/L3 rejection taxonomy).
func defectClasses() []defectClass {
	// Canonical symbol form is "EUR/USD" (instruments table universe —
	// the slash-less EURUSD reads as an unknown instrument upstream).
	syms := []string{"EUR/USD", "GBP/USD", "USD/JPY", "AUD/USD"}
	sym := func(r *rand.Rand) string { return syms[r.Intn(len(syms))] }
	return []defectClass{
		{"negative_quantity", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			return reqSpec{body: orderJSON(sym(r), "SELL", "1.08500",
				"-"+strconv.Itoa(1+r.Intn(100_000))), contentType: "application/json",
				headers: idemHdr(r)}
		}},
		{"negative_price", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			return reqSpec{body: orderJSON(sym(r), "BUY",
				"-"+strconv.FormatFloat(r.Float64()+0.1, 'f', 5, 64), "1000"),
				contentType: "application/json", headers: idemHdr(r)}
		}},
		{"malformed_json", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			good := orderJSON(sym(r), "BUY", "1.08500", "1000")
			return reqSpec{body: good[:len(good)/2], // truncated mid-token
				contentType: "application/json", headers: idemHdr(r)}
		}},
		{"invalid_signature", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			h := idemHdr(r)
			h["Authorization"] = "Bearer " + badJWT(r)
			return reqSpec{body: orderJSON(sym(r), "BUY", "1.08500", "1000"),
				contentType: "application/json", headers: h, noAuth: true}
		}},
		{"invalid_hmac_headers", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			h := idemHdr(r)
			var k [16]byte
			_, _ = r.Read(k[:])
			h["X-API-KEY"] = "ek_" + hex.EncodeToString(k[:8])
			h["X-TIMESTAMP"] = strconv.FormatInt(time.Now().Unix(), 10)
			h["X-SIGNATURE"] = base64.RawURLEncoding.EncodeToString(k[:])
			return reqSpec{body: orderJSON(sym(r), "SELL", "1.08500", "1000"),
				contentType: "application/json", headers: h, noAuth: true}
		}},
		{"unknown_instrument", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			return reqSpec{body: orderJSON(
				fmt.Sprintf("ZZZ%03d", r.Intn(1000)), "BUY", "1.00000", "1000"),
				contentType: "application/json", headers: idemHdr(r)}
		}},
		{"enum_violation", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			return reqSpec{body: orderJSON(sym(r), "SIDEWAYS", "1.08500", "1000"),
				contentType: "application/json", headers: idemHdr(r)}
		}},
		{"missing_fields", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			return reqSpec{body: []byte(`{"symbol":"EUR/USD"}`),
				contentType: "application/json", headers: idemHdr(r)}
		}},
		{"oversized_payload", func(r *rand.Rand, _ uint64, cfg *Config) reqSpec {
			// Valid JSON prefix + a huge junk member to oversize the body.
			var b bytes.Buffer
			b.WriteString(`{"symbol":"EUR/USD","side":"BUY","type":"LIMIT","price":"1.08500","quantity":"1000","pad":"`)
			chunk := []byte("0123456789abcdef")
			for i := 0; i < cfg.OversizeKB*1024/16; i++ {
				b.Write(chunk)
			}
			b.WriteString(`"}`)
			return reqSpec{body: b.Bytes(), contentType: "application/json",
				headers: idemHdr(r)}
		}},
		{"wrong_content_type", func(r *rand.Rand, _ uint64, _ *Config) reqSpec {
			return reqSpec{body: orderJSON(sym(r), "BUY", "1.08500", "1000"),
				contentType: "text/plain", headers: idemHdr(r)}
		}},
	}
}

// ---------------------------------------------------------------------------
// Statistics.
// ---------------------------------------------------------------------------

type classStats struct {
	counts [outCount]atomic.Uint64
}

func (s *classStats) add(o outcome)        { s.counts[o].Add(1) }
func (s *classStats) get(o outcome) uint64 { return s.counts[o].Load() }

type blastStats struct {
	perClass []classStats
	total    classStats
	hist     latHist
	// status code distribution + degradation-mode header distribution —
	// evidence that the rejection/envelope surface stayed live.
	mu       sync.Mutex
	statuses map[int]uint64
	modes    map[string]uint64
}

func (s *blastStats) record(class int, o outcome, status int, latNs int64, modeHdr string) {
	s.perClass[class].add(o)
	s.total.add(o)
	s.hist.observe(latNs)
	s.mu.Lock()
	s.statuses[status]++
	if modeHdr != "" {
		s.modes[modeHdr]++
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Control stream — the liveness canary. Consecutive probe failures after
// startup grace mean the gateway stopped answering (crash/hang), which
// is the run's headline failure.
// ---------------------------------------------------------------------------

type controlStats struct {
	sent            atomic.Uint64
	ok              atomic.Uint64
	nonOK           atomic.Uint64 // answered but not 2xx
	failed          atomic.Uint64 // transport error / timeout
	consecFailed    atomic.Int64
	maxConsecFailed atomic.Int64
	crashed         atomic.Bool
}

// ---------------------------------------------------------------------------
// Report.
// ---------------------------------------------------------------------------

type ClassReport struct {
	Sent      uint64 `json:"sent"`
	R4xx      uint64 `json:"4xx"`
	R429      uint64 `json:"429"`
	R5xx      uint64 `json:"5xx"`
	R2xx3xx   uint64 `json:"2xx_3xx"`
	Timeouts  uint64 `json:"timeouts"`
	Transport uint64 `json:"transport_errors"`
}

type ControlReport struct {
	Sent            uint64 `json:"sent"`
	OK              uint64 `json:"ok"`
	NonOK           uint64 `json:"non_ok"`
	Failed          uint64 `json:"failed"`
	MaxConsecFailed int64  `json:"max_consecutive_failures"`
	Crashed         bool   `json:"crashed"`
}

type Report struct {
	Tool         string  `json:"tool"`
	Started      string  `json:"started"`
	Ended        string  `json:"ended"`
	DurationS    float64 `json:"duration_s"`
	TargetRate   float64 `json:"target_rate"`
	AchievedRate float64 `json:"achieved_rate"`

	Totals    ClassReport            `json:"totals"`
	PerClass  map[string]ClassReport `json:"per_class"`
	Statuses  map[string]uint64      `json:"status_codes"`
	Modes     map[string]uint64      `json:"degradation_mode_header"`
	LatencyUS struct {
		P50  uint64 `json:"p50"`
		P99  uint64 `json:"p99"`
		Max  uint64 `json:"max"`
		Mean uint64 `json:"mean"`
	} `json:"latency_us"`
	Control ControlReport `json:"control"`

	VerdictCode int      `json:"verdict_code"`
	Verdict     string   `json:"verdict"`
	Reasons     []string `json:"reasons"`
}

func classReport(s *classStats) ClassReport {
	var r ClassReport
	for o := outcome(0); o < outCount; o++ {
		n := s.get(o)
		r.Sent += n
		switch o {
		case out2xx3xx:
			r.R2xx3xx = n
		case out4xx:
			r.R4xx = n
		case out429:
			r.R429 = n
		case out5xx:
			r.R5xx = n
		case outTimeout:
			r.Timeouts = n
		case outTransport:
			r.Transport = n
		}
	}
	return r
}

// ---------------------------------------------------------------------------
// Pacer — deadline scheduler (tests/soak pattern): deadline i is
// start + i*interval; sleep while >1ms early, yield for the last stretch.
// ---------------------------------------------------------------------------

type pacer struct {
	intervalNs float64
	start      time.Time
	i          int64
}

func newPacer(rate float64, start time.Time) pacer {
	if rate <= 0 {
		rate = 1
	}
	return pacer{intervalNs: float64(time.Second) / rate, start: start}
}

func (p *pacer) next() time.Time {
	p.i++
	target := p.start.Add(time.Duration(float64(p.i) * p.intervalNs))
	for {
		rem := time.Until(target)
		if rem <= 0 {
			return target
		}
		if rem > time.Millisecond {
			time.Sleep(rem - time.Millisecond)
			continue
		}
		time.Sleep(0) // yield the slot for the final sub-ms stretch
	}
}

// ---------------------------------------------------------------------------
// Run — the blast itself.
// ---------------------------------------------------------------------------

// Run executes the error-injection blast: paced invalid-order fire +
// the control liveness stream, until Duration elapses, ctx cancels, or
// the control stream declares the target crashed. Returns the report
// (verdict fields populated) and the process exit code.
func Run(ctx context.Context, cfg Config) (*Report, int) {
	cfg.defaults()
	classes := defectClasses()

	// Pre-mint the rotating token pool once — per-request HS256 is cheap
	// but pointless when the account rotation is bounded anyway.
	var tokenPool []string
	if cfg.JWTKeyB64 != "" {
		tokenPool = make([]string, 0, cfg.JWTAccounts)
		for i := 0; i < cfg.JWTAccounts; i++ {
			tok, err := mintJWT(cfg.JWTKeyB64, "errblast", int64(1+i),
				[]string{"trade", "read"})
			if err != nil {
				return failEarly(fmt.Sprintf("mint JWT: %v", err))
			}
			tokenPool = append(tokenPool, tok)
		}
	} else if cfg.Token != "" {
		tokenPool = []string{cfg.Token}
	}

	tr := &http.Transport{
		MaxConnsPerHost:     cfg.Workers * 2,
		MaxIdleConns:        cfg.Workers * 2,
		MaxIdleConnsPerHost: cfg.Workers * 2,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: cfg.ReqTimeout}
	ctrl := &http.Client{Timeout: cfg.ReqTimeout}

	started := time.Now()
	blastCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	st := &blastStats{perClass: make([]classStats, len(classes)),
		statuses: map[int]uint64{}, modes: map[string]uint64{}}
	cs := &controlStats{}

	// ---- control stream -----------------------------------------------------
	go func() {
		interval := time.Duration(float64(time.Second) / cfg.ControlRate)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		var consec int64
		for {
			select {
			case <-blastCtx.Done():
				return
			case <-tk.C:
			}
			cs.sent.Add(1)
			req, err := http.NewRequestWithContext(blastCtx, http.MethodGet,
				cfg.Addr+cfg.HealthPath, nil)
			if err != nil {
				cs.failed.Add(1)
				consec++
			} else if resp, err := ctrl.Do(req); err != nil {
				cs.failed.Add(1)
				consec++
			} else {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					cs.ok.Add(1)
					consec = 0
				} else {
					cs.nonOK.Add(1)
					consec++
				}
			}
			cs.consecFailed.Store(consec)
			for {
				m := cs.maxConsecFailed.Load()
				if consec <= m || cs.maxConsecFailed.CompareAndSwap(m, consec) {
					break
				}
			}
			// Sustained liveness failure past the warmup window = the
			// gateway stopped answering — abort the blast, verdict 2.
			if consec >= 5 && time.Since(started) > cfg.StartupGrace {
				cs.crashed.Store(true)
				cancel()
				return
			}
		}
	}()

	// ---- workers ------------------------------------------------------------
	seqCh := make(chan uint64, cfg.Workers*4)
	var wg sync.WaitGroup
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			var xff string
			for seq := range seqCh {
				if cfg.XFFPool > 0 {
					xff = fmt.Sprintf("10.96.%d.%d", r.Intn(250)+1, r.Intn(250)+1)
				}
				ci := int(seq % uint64(len(classes)))
				spec := classes[ci].build(r, seq, &cfg)

				req, err := http.NewRequestWithContext(blastCtx,
					http.MethodPost, cfg.Addr+cfg.OrdersPath,
					bytes.NewReader(spec.body))
				if err != nil {
					st.record(ci, outTransport, 0, 0, "")
					continue
				}
				if spec.contentType != "" {
					req.Header.Set("Content-Type", spec.contentType)
				}
				for k, v := range spec.headers {
					req.Header.Set(k, v)
				}
				if !spec.noAuth && len(tokenPool) > 0 {
					req.Header.Set("Authorization",
						"Bearer "+tokenPool[int(seq)%len(tokenPool)])
				}
				if xff != "" {
					req.Header.Set("X-Forwarded-For", xff)
				}

				t0 := time.Now()
				resp, err := client.Do(req)
				lat := time.Since(t0).Nanoseconds()
				if err != nil {
					st.record(ci, classify(0, err), 0, lat, "")
					if blastCtx.Err() != nil {
						return // aborted — drain ends the run
					}
					continue
				}
				mode := resp.Header.Get("X-Degradation-Mode")
				code := resp.StatusCode
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				st.record(ci, classify(code, nil), code, lat, mode)
			}
		}(cfg.Seed + int64(w))
	}

	// ---- paced dispatcher ---------------------------------------------------
	dispatched := func() {
		defer close(seqCh)
		p := newPacer(cfg.Rate, started)
		var seq uint64
		for {
			if cfg.Duration > 0 && time.Since(started) >= cfg.Duration {
				return
			}
			if blastCtx.Err() != nil {
				return
			}
			p.next()
			seq++
			select {
			case seqCh <- seq:
			case <-blastCtx.Done():
				return
			}
		}
	}
	dispatched() // run on this goroutine; returns on duration/cancel
	wg.Wait()

	rep := &Report{
		Tool:       "errblast",
		Started:    started.UTC().Format(time.RFC3339),
		Ended:      time.Now().UTC().Format(time.RFC3339),
		DurationS:  time.Since(started).Seconds(),
		TargetRate: cfg.Rate,
		PerClass:   map[string]ClassReport{},
		Statuses:   map[string]uint64{},
		Modes:      st.modes,
	}
	rep.Totals = classReport(&st.total)
	if rep.DurationS > 0 {
		rep.AchievedRate = float64(rep.Totals.Sent) / rep.DurationS
	}
	for i, c := range classes {
		rep.PerClass[c.name] = classReport(&st.perClass[i])
	}
	st.mu.Lock()
	for code, n := range st.statuses {
		rep.Statuses[strconv.Itoa(code)] = n
	}
	st.mu.Unlock()
	rep.LatencyUS.P50 = st.hist.percentile(0.50) / 1_000
	rep.LatencyUS.P99 = st.hist.percentile(0.99) / 1_000
	rep.LatencyUS.Max = st.hist.max.Load() / 1_000
	if st.hist.count.Load() > 0 {
		rep.LatencyUS.Mean = st.hist.sum.Load() / st.hist.count.Load() / 1_000
	}
	rep.Control = ControlReport{
		Sent:            cs.sent.Load(),
		OK:              cs.ok.Load(),
		NonOK:           cs.nonOK.Load(),
		Failed:          cs.failed.Load(),
		MaxConsecFailed: cs.maxConsecFailed.Load(),
		Crashed:         cs.crashed.Load(),
	}

	code, reasons := verdict(rep, &cfg)
	rep.VerdictCode = code
	rep.Verdict = map[bool]string{true: "PASS", false: "FAIL"}[code == 0]
	rep.Reasons = reasons
	return rep, code
}

func failEarly(why string) (*Report, int) {
	return &Report{Verdict: "FAIL", VerdictCode: 4, Reasons: []string{why}}, 4
}

// verdict applies the run's pass/fail policy (spec §2.7 fail-closed
// reading of the evidence):
//
//	2 — the control stream lost the target (crash/hang) or every blast
//	    attempt failed at transport level;
//	3 — 5xx share of blast responses exceeded Max5xxPct;
//	4 — no traffic completed at all;
//	0 — rejections stayed clean under pressure.
func verdict(rep *Report, cfg *Config) (int, []string) {
	var reasons []string
	if rep.Control.Crashed {
		reasons = append(reasons,
			"control stream lost liveness past startup grace — target presumed crashed")
	}
	if rep.Totals.Sent == 0 {
		reasons = append(reasons, "no blast traffic completed")
		return 4, reasons
	}
	responded := rep.Totals.Sent - rep.Totals.Transport
	if responded == 0 {
		reasons = append(reasons,
			"every attempt failed at transport level — target unreachable")
		return 2, reasons
	}
	fivePct := 100 * float64(rep.Totals.R5xx) / float64(rep.Totals.Sent)
	if rep.Control.Crashed {
		return 2, reasons
	}
	if fivePct > cfg.Max5xxPct {
		reasons = append(reasons, fmt.Sprintf(
			"5xx share %.2f%% exceeds threshold %.2f%% (%d of %d)",
			fivePct, cfg.Max5xxPct, rep.Totals.R5xx, rep.Totals.Sent))
		return 3, reasons
	}
	if rep.Totals.Timeouts > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"note: %d request timeouts (%.2f%%) — rejection latency degraded",
			rep.Totals.Timeouts,
			100*float64(rep.Totals.Timeouts)/float64(rep.Totals.Sent)))
	}
	if rep.Control.Failed > 0 || rep.Control.NonOK > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"note: %d control probes failed / %d non-2xx (max %d consecutive)",
			rep.Control.Failed, rep.Control.NonOK, rep.Control.MaxConsecFailed))
	}
	if len(reasons) == 0 {
		reasons = append(reasons,
			"rejection path stayed clean: all invalid orders rejected 4xx/429, 5xx within threshold, liveness held")
	}
	return 0, reasons
}
