// Sentinel-aware failover client for the coordination Redis topology
// (spec §4.5, Task 1.3.9): 1 primary + 2 replicas watched by 3 sentinels
// (quorum 2). go-redis' FailoverClient provides the mechanics — continuous
// sentinel discovery, +switch-master pub/sub tracking and automatic
// re-resolution of the current master on every new pooled connection —
// so this file adds only what the exchange needs on top:
//
//  1. FailoverConfig/NewFailoverClient — validated, fail-closed
//     construction that returns the same *Client key-schema wrapper used
//     by the rest of the codebase. The C++ Phase-02 LeaderElection and
//     Go services therefore converge on the identical key space
//     (engine:leader:{shard}, system:degradation:*, halt:global, ...)
//     through the same HA path.
//
//  2. Announced-address resolution (SentinelResolveMode). Sentinel
//     announces the address IT uses — in the dev topology the pinned
//     redis-ha IPs (10.99.0.11-13:6379, docker-compose.dev.yml). Clients
//     on that network dial it verbatim; dev-host clients cannot route it
//     and need a rewrite.
//
//     - as_announced : dial verbatim (production / in-network).
//     - announce_map : static host:port -> host:port rewrite table,
//     fail-closed on a miss. Deterministic because the announced IPs
//     are pinned.
//     - host_probe   : dynamic rewrite for dev hosts. Sentinel's
//     master/replica records expose runid; each host_addrs endpoint is
//     probed for its own run_id; the join announced->host addr survives
//     any announced-addr churn.
//
//  3. FailoverClient — thin adapter exposing AnnouncedMasterAddr (ops/
//     tests can see where traffic is pointed), OnSwitch (reconnect-latency
//     seam) and StartHealthProbe (active PING health probe per the task).
//
// SDD edge cases (spec §4.5 / §2.7 fail-closed zero-loss):
//   - Split brain: min-replicas-to-write=1 + min-replicas-max-lag=5 makes
//     a partitioned or restarted ex-primary refuse writes (no replica in
//     lag window => write rejection). The leader lease epoch-lease value
//     "{token}:{epoch}" remains the fencing token: epoch allocation is
//     Phase-02's concern, but any re-acquirer must compare epochs so a
//     demoted holder that briefly kept its lease cannot act stale.
//   - Sentinel quorum loss: go-redis caches the last connected sentinel
//     and the last resolved master; commands keep flowing to the master
//     while it lives. Discovery (not data flow) degrades first — the
//     client keeps the last-known master until sentinel addrs are
//     exhausted, matching "keep last-known" semantics.
//   - Reconnect storms: pool re-dials are bounded by PoolSize; host_probe
//     map rebuilds are rate-limited (250ms); command retries use go-redis'
//     bounded backoff (MaxRetries, Min/MaxRetryBackoff).
package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// SentinelResolveMode selects how sentinel-announced master addresses are
// translated before dialing. See the package header for the three modes.
type SentinelResolveMode string

const (
	ResolveAsAnnounced SentinelResolveMode = "as_announced"
	ResolveAnnounceMap SentinelResolveMode = "announce_map"
	ResolveHostProbe   SentinelResolveMode = "host_probe"
)

// probeRebuildMinInterval bounds host_probe map rebuilds so a flapping
// topology cannot turn dials into a probe storm.
const probeRebuildMinInterval = 250 * time.Millisecond

// probeTimeout bounds each probe round-trip (sentinel SENTINEL commands
// and host INFO). Probes are dev-only plumbing; tight timeouts keep a dead
// endpoint from stalling the dial path.
const probeTimeout = 500 * time.Millisecond

// FailoverConfig drives NewFailoverClient. Durations <= 0 fall back to the
// same conservative defaults as New (5s dial, 3s read/write, 4s pool).
type FailoverConfig struct {
	MasterName    string   // sentinel monitor name ("mymaster" in dev)
	SentinelAddrs []string // host:port sentinel endpoints
	Password      string
	DB            int

	PoolSize     int
	MinIdleConns int
	MaxRetries   int // <0 invalid; 0 resolves to the go-redis default of 3

	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolTimeout  time.Duration

	// Announced-address translation. AnnounceMap holds the literal
	// host:port -> host:port rewrite table consulted only in
	// ResolveAnnounceMap mode (ignored elsewhere — a static entry must
	// never shadow the correct in-network or probed answer). HostAddrs
	// are the candidate endpoints probed in ResolveHostProbe mode.
	ResolveMode SentinelResolveMode
	AnnounceMap map[string]string
	HostAddrs   []string

	// OnSwitch fires when the dialer observes Sentinel announce a different
	// data address — the seam for reconnect-latency metrics and the
	// failover drill's switch detector. Must not block; invoked inline.
	OnSwitch func(announcedAddr string)
}

func checkHostPort(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("not a host:port address: %w", err)
	}
	return nil
}

// validate enforces the fail-closed contract: a misconfigured failover
// client must fail at construction, not at 3 a.m. mid-failover. Applies
// defaults for zero-valued tunables.
func (c *FailoverConfig) validate() error {
	if strings.TrimSpace(c.MasterName) == "" {
		return errors.New("redis failover: master_name must not be empty")
	}
	if len(c.SentinelAddrs) == 0 {
		return errors.New("redis failover: at least one sentinel addr is required")
	}
	for _, a := range c.SentinelAddrs {
		if err := checkHostPort(a); err != nil {
			return fmt.Errorf("redis failover: sentinel addr %q: %w", a, err)
		}
	}
	switch c.ResolveMode {
	case ResolveAsAnnounced:
		// Nothing extra required.
	case ResolveAnnounceMap:
		if len(c.AnnounceMap) == 0 {
			return errors.New("redis failover: resolve_mode announce_map requires a non-empty announce_map")
		}
		for k, v := range c.AnnounceMap {
			if err := checkHostPort(k); err != nil {
				return fmt.Errorf("redis failover: announce_map key %q: %w", k, err)
			}
			if err := checkHostPort(v); err != nil {
				return fmt.Errorf("redis failover: announce_map value %q: %w", v, err)
			}
		}
	case ResolveHostProbe:
		if len(c.HostAddrs) == 0 {
			return errors.New("redis failover: resolve_mode host_probe requires host_addrs")
		}
		for _, a := range c.HostAddrs {
			if err := checkHostPort(a); err != nil {
				return fmt.Errorf("redis failover: host_addrs entry %q: %w", a, err)
			}
		}
	default:
		return fmt.Errorf("redis failover: unknown resolve_mode %q (want as_announced|announce_map|host_probe)", c.ResolveMode)
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("redis failover: max_retries %d must be >= 0", c.MaxRetries)
	}

	if c.MaxRetries == 0 {
		c.MaxRetries = 3 // go-redis default; a failover client must retry
	}
	if c.PoolSize <= 0 {
		c.PoolSize = 20
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 3 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 3 * time.Second
	}
	if c.PoolTimeout <= 0 {
		c.PoolTimeout = 4 * time.Second
	}
	return nil
}

// FailoverClient is the sentinel-aware coordination client: the full
// Client helper surface plus failover observability. It deliberately keeps
// the identical key space — engine:leader:{shard}, system:degradation:*,
// circuit_breaker:*, halt:global — so leader lease and degradation
// observation work unchanged across master switches (Phase-02 C++
// LeaderElection consumes the same keys via its own sentinel path).
type FailoverClient struct {
	*Client
	resolver *sentinelAddrResolver
}

// NewFailoverClient builds a failover Client from a validated
// FailoverConfig. The client is lazy: sentinel discovery happens on the
// first command; call Ping at startup for a readiness check.
func NewFailoverClient(cfg FailoverConfig) (*FailoverClient, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	resolver := newSentinelAddrResolver(cfg)
	rdb := goredis.NewFailoverClient(&goredis.FailoverOptions{
		MasterName:    cfg.MasterName,
		SentinelAddrs: cfg.SentinelAddrs,
		Password:      cfg.Password,
		DB:            cfg.DB,
		PoolSize:      cfg.PoolSize,
		MinIdleConns:  cfg.MinIdleConns,
		MaxRetries:    cfg.MaxRetries,
		DialTimeout:   cfg.DialTimeout,
		ReadTimeout:   cfg.ReadTimeout,
		WriteTimeout:  cfg.WriteTimeout,
		PoolTimeout:   cfg.PoolTimeout,
		// go-redis routes BOTH sentinel connections and resolved
		// master/replica connections through this dialer — resolve()
		// passes sentinel addrs through untouched and rewrites only
		// announced data addresses.
		Dialer: resolver.dial,
	})
	return &FailoverClient{Client: &Client{Client: rdb}, resolver: resolver}, nil
}

// AnnouncedMasterAddr returns the data address Sentinel most recently
// handed the dialer ("" before the first command). Purely observational.
func (f *FailoverClient) AnnouncedMasterAddr() string {
	if f.resolver == nil {
		return ""
	}
	return f.resolver.lastAnnounced()
}

// StartHealthProbe runs an active PING probe every interval until ctx is
// done or the returned stop func is called. report receives each round-
// trip result (latency, error) — nil report means probe-and-drop. The
// probe keeps the pool warm so the first real command after a master
// switch does not pay the cold-dial cost.
func (f *FailoverClient) StartHealthProbe(ctx context.Context, interval time.Duration, report func(d time.Duration, err error)) (stop func()) {
	if interval <= 0 {
		interval = time.Second
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
			}
			start := time.Now()
			err := f.Ping(ctx)
			if report != nil {
				report(time.Since(start), err)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// SentinelMasterAddr asks a live sentinel for the current master of
// masterName — the ops/drill source of truth for "who is master now".
func SentinelMasterAddr(ctx context.Context, sentinelAddr, masterName string) (string, error) {
	sc := goredis.NewSentinelClient(&goredis.Options{
		Addr:        sentinelAddr,
		DialTimeout: probeTimeout,
		ReadTimeout: probeTimeout,
	})
	defer func() { _ = sc.Close() }()
	addr, err := sc.GetMasterAddrByName(ctx, masterName).Result()
	if err != nil {
		return "", fmt.Errorf("sentinel %s get-master-addr-by-name %q: %w", sentinelAddr, masterName, err)
	}
	if len(addr) != 2 {
		return "", fmt.Errorf("sentinel %s get-master-addr-by-name %q: malformed reply %v", sentinelAddr, masterName, addr)
	}
	return net.JoinHostPort(addr[0], addr[1]), nil
}

// ---------------------------------------------------------------------------
// Announced-address resolver
// ---------------------------------------------------------------------------

// sentinelAddrResolver implements FailoverOptions.Dialer. go-redis calls
// it with a sentinel addr (sentinel connections) or a sentinel-announced
// data addr (master connections); the mode decides whether the announced
// addr is dialed verbatim or rewritten for host-side reachability.
type sentinelAddrResolver struct {
	cfg         FailoverConfig
	sentinelSet map[string]struct{} // configured sentinel addrs: pass through
	netDialer   net.Dialer

	mu        sync.Mutex
	probeMap  map[string]string // announced addr -> host addr (host_probe)
	lastProbe time.Time
	probeErr  error // last rebuild failure, for diagnostics

	announced atomic.Value // string: last resolved data addr (OnSwitch feed)
}

func newSentinelAddrResolver(cfg FailoverConfig) *sentinelAddrResolver {
	set := make(map[string]struct{}, len(cfg.SentinelAddrs))
	for _, a := range cfg.SentinelAddrs {
		set[a] = struct{}{}
	}
	return &sentinelAddrResolver{
		cfg:         cfg,
		sentinelSet: set,
		netDialer:   net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 5 * time.Minute},
	}
}

func (r *sentinelAddrResolver) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	target, err := r.resolve(ctx, addr)
	if err != nil {
		return nil, err
	}
	return r.netDialer.DialContext(ctx, network, target)
}

// resolve maps the address go-redis wants to dial to the address this
// host should actually dial. Sentinel addrs are always dialed verbatim —
// they were configured by us, not announced.
func (r *sentinelAddrResolver) resolve(ctx context.Context, addr string) (string, error) {
	if _, ok := r.sentinelSet[addr]; ok {
		return addr, nil
	}
	switch r.cfg.ResolveMode {
	case ResolveAsAnnounced:
		// Verbatim: the map must NOT shadow this mode — inside the redis-ha
		// network the announced addr is the correct one and a stale static
		// entry would misroute.
		r.noteAnnounced(addr)
		return addr, nil
	case ResolveAnnounceMap:
		mapped, ok := r.cfg.AnnounceMap[addr]
		if !ok {
			return "", fmt.Errorf("redis failover: announced addr %q missing from announce_map (fail-closed)", addr)
		}
		r.noteAnnounced(addr)
		return mapped, nil
	case ResolveHostProbe:
		mapped, err := r.probeResolve(ctx, addr)
		if err != nil {
			return "", err
		}
		r.noteAnnounced(addr)
		return mapped, nil
	default:
		return "", fmt.Errorf("redis failover: unhandled resolve_mode %q", r.cfg.ResolveMode)
	}
}

// noteAnnounced records the newest announced data addr and fires OnSwitch
// on changes — the reconnect-measurement seam.
func (r *sentinelAddrResolver) noteAnnounced(addr string) {
	prev, _ := r.announced.Load().(string)
	if prev == addr {
		return
	}
	r.announced.Store(addr)
	if r.cfg.OnSwitch != nil && prev != "" {
		r.cfg.OnSwitch(addr)
	}
}

func (r *sentinelAddrResolver) lastAnnounced() string {
	a, _ := r.announced.Load().(string)
	return a
}

// probeResolve looks up the announced addr in the runid-joined map,
// rebuilding the map on a miss (rate-limited). Fail-closed: an announced
// addr that cannot be tied to a live host_addrs endpoint is an error, not
// a guaranteed-unroutable dial attempt.
func (r *sentinelAddrResolver) probeResolve(ctx context.Context, addr string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mapped, ok := r.probeMap[addr]; ok {
		return mapped, nil
	}
	if since := time.Since(r.lastProbe); since < probeRebuildMinInterval {
		return "", fmt.Errorf("redis failover probe: announced addr %q unmapped, rebuild cooldown %v remaining",
			addr, probeRebuildMinInterval-since)
	}
	r.lastProbe = time.Now()
	if err := r.buildProbeMap(ctx); err != nil {
		r.probeErr = err
		return "", err
	}
	if mapped, ok := r.probeMap[addr]; ok {
		return mapped, nil
	}
	return "", fmt.Errorf("redis failover probe: announced addr %q matches no live host_addrs endpoint", addr)
}

// buildProbeMap joins Sentinel's topology records against the host_addrs
// endpoints on run_id. Sentinel reports each node's runid; each host
// endpoint reports its own run_id via INFO server. Nodes that share a
// runid are the same server — so announced ip:port maps to the host port
// that serves it. Container IPs may churn; runids survive the join.
func (r *sentinelAddrResolver) buildProbeMap(ctx context.Context) error {
	// Step 1: runid -> announced addr(s), from any reachable sentinel.
	runidToAnnounced := map[string][]string{}
	var firstErr error
	for _, saddr := range r.cfg.SentinelAddrs {
		sc := goredis.NewSentinelClient(&goredis.Options{
			Addr:        saddr,
			DialTimeout: probeTimeout,
			ReadTimeout: probeTimeout,
			MaxRetries:  0,
		})
		master, merr := sc.Master(ctx, r.cfg.MasterName).Result()
		replicas, rerr := sc.Replicas(ctx, r.cfg.MasterName).Result()
		_ = sc.Close()
		if merr == nil {
			collectNode(runidToAnnounced, master)
		}
		if rerr == nil {
			for _, rep := range replicas {
				collectNode(runidToAnnounced, rep)
			}
		}
		if len(runidToAnnounced) > 0 {
			break
		}
		if firstErr == nil {
			firstErr = errors.Join(merr, rerr)
		}
	}
	if len(runidToAnnounced) == 0 {
		if firstErr == nil {
			firstErr = errors.New("sentinel returned no master/replica records")
		}
		return fmt.Errorf("redis failover probe: sentinel topology query failed: %w", firstErr)
	}

	// Step 2: runid -> host addr, by asking each candidate its run_id.
	newMap := map[string]string{}
	for _, haddr := range r.cfg.HostAddrs {
		runID, err := probeRunID(ctx, haddr)
		if err != nil {
			continue // dead/transitioning endpoint — skip, fail-closed later if nothing maps
		}
		for _, announced := range runidToAnnounced[runID] {
			newMap[announced] = haddr
		}
	}
	if len(newMap) == 0 {
		return fmt.Errorf("redis failover probe: no host_addrs endpoint matches sentinel topology (%d nodes seen)", len(runidToAnnounced))
	}
	r.probeMap = newMap
	return nil
}

// collectNode extracts runid -> announced addr candidates from a
// SENTINEL master/replica record. Both the ip:port form (what
// get-master-addr-by-name returns) and the "name" form are indexed so
// either announced spelling resolves.
func collectNode(dst map[string][]string, fields map[string]string) {
	runID := fields["runid"]
	if runID == "" {
		return
	}
	if ip, port := fields["ip"], fields["port"]; ip != "" && port != "" {
		dst[runID] = append(dst[runID], net.JoinHostPort(ip, port))
	}
	if name := fields["name"]; name != "" {
		dst[runID] = append(dst[runID], name)
	}
}

// probeRunID asks one host endpoint for its run_id via INFO server.
func probeRunID(ctx context.Context, addr string) (string, error) {
	c := goredis.NewClient(&goredis.Options{
		Addr:        addr,
		DialTimeout: probeTimeout,
		ReadTimeout: probeTimeout,
		MaxRetries:  0,
	})
	defer func() { _ = c.Close() }()
	info, err := c.Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(info, "\n") {
		if runID, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok {
			return strings.TrimSpace(runID), nil
		}
	}
	return "", fmt.Errorf("no run_id in INFO server reply from %s", addr)
}
