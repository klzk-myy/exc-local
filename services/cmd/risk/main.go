// Command risk is the cross-shard margin coordinator bridge (Task
// 19.3.11, spec §13.1): a single-instance process that subscribes every
// engine shard's margin_ctl_in_{shard} Aeron channel (stream 1101),
// answers REQ/RELEASE frames through risk.MarginCoordinator, and
// publishes ACK/NACK/RELEASE frames on margin_ctl_out_{shard} (stream
// 1102). Coordinator state is a single-writer book — running two
// instances would fork the reservation view, so deployments run
// exactly one.
//
// Boot: config → PG pool (durable reservation ledger, migration 058) →
// Redis (shard map) → NATS (ops alerts) → Aeron (media driver). Then
// Recover() replays live reservations before subscriptions open —
// orphans are tombstoned RECOVERY_ORPHAN and their RELEASE frames
// published before any new REQ is accepted.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"exchange/internal/api"
	"exchange/internal/config"
	"exchange/internal/db"
	"exchange/internal/ipc"
	ipcaeron "exchange/internal/ipc/aeron"
	excnats "exchange/internal/nats"
	"exchange/internal/redis"
	"exchange/internal/risk"
	"exchange/internal/settlement"
	"exchange/internal/utils"
	"exchange/pkg/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "risk: %v\n", err)
		os.Exit(1)
	}
}

// aeronEmitter publishes coordinator→engine frames on the shard's
// margin_ctl_out channel (risk.MarginCtlEmitter seam).
type aeronEmitter struct {
	pubs map[uint32]*ipcaeron.Publication
	log  func(format string, args ...any)
}

// EmitMarginCtl implements risk.MarginCtlEmitter. Offer errors surface
// as errors — a dropped ACK is exactly the engine's pessimistic-floor
// scenario and must be loud.
func (e *aeronEmitter) EmitMarginCtl(_ context.Context, shard uint32, frame []byte) error {
	p, ok := e.pubs[shard]
	if !ok || p == nil {
		return fmt.Errorf("margin ctl emit: no publication for shard %d", shard)
	}
	if rc := p.Offer(frame); rc <= 0 {
		return fmt.Errorf("margin ctl emit: offer shard %d rc=%d", shard, rc)
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level, _ := logging.ParseLevel(cfg.Logging.Level)
	log, err := logging.New(level, cfg.Logging.Format)
	if err != nil {
		return err
	}

	ctx, stop := utils.SignalContext()
	defer stop()

	pool, err := db.NewPool(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns)
	if err != nil {
		return fmt.Errorf("risk: postgres: %w", err)
	}
	defer pool.Close()

	rdb := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	defer rdb.Close()

	shardMap, shardSrc, err := config.LoadShardMapForService(ctx, rdb)
	if err != nil {
		return fmt.Errorf("risk: shard map: %w", err)
	}
	// Engine shard universe: static entries + the elastic band (same
	// enumeration as cmd/gateway's shardIDs helper).
	set := map[int]struct{}{}
	for _, id := range shardMap.Entries() {
		set[id] = struct{}{}
	}
	for i := shardMap.ElasticBase(); i < shardMap.ElasticBase()+shardMap.ElasticCount(); i++ {
		set[i] = struct{}{}
	}
	shards := make([]uint32, 0, len(set))
	for id := range set {
		shards = append(shards, uint32(id))
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i] < shards[j] })
	if len(shards) == 0 {
		return fmt.Errorf("risk: shard map empty — coordinator has nothing to serve")
	}

	// Ops alerts publish through the shared JetStream seam
	// (ops.alerts.risk). NATS down is a boot failure — a coordinator
	// that cannot page on ledger faults violates the alert contract.
	nats, err := excnats.Connect(ctx, excnats.DefaultConfig(cfg.NATS.URLList()), log)
	if err != nil {
		return fmt.Errorf("risk: nats: %w", err)
	}
	defer nats.Close()
	alerter := settlement.PublisherAlerter{Pub: settlement.NatsPublisher{JS: nats.JetStream()}}

	ledger, err := risk.NewPgShardMarginLedger(pool)
	if err != nil {
		return fmt.Errorf("risk: shard margin ledger: %w", err)
	}
	coord, err := risk.NewMarginCoordinator(risk.MarginCoordinatorConfig{
		ShardCount: len(shards),
		Margins:    ledger, // ledger doubles as the lazy AccountMarginSource
		Alerter:    alerter,
		Logf:       func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) },
	}, ledger)
	if err != nil {
		return fmt.Errorf("risk: coordinator: %w", err)
	}

	// Aeron attach — EXC_AERON_DIR overrides the media-driver CnC dir
	// (empty uses the driver's default location, same convention as the
	// bridge binary).
	aero, err := ipcaeron.Connect(os.Getenv("EXC_AERON_DIR"), 5000)
	if err != nil {
		return fmt.Errorf("risk: aeron connect: %w", err)
	}
	defer aero.Close()

	pubs := map[uint32]*ipcaeron.Publication{}
	for _, shard := range shards {
		pub, perr := aero.AddPublication(ipc.MarginCtlOutURI(shard),
			ipc.MarginCtlOutStreamID, 5*time.Second)
		if perr != nil {
			return fmt.Errorf("risk: aeron publish shard %d: %w", shard, perr)
		}
		pubs[shard] = pub
	}
	emitter := &aeronEmitter{pubs: pubs,
		log: func(f string, a ...any) { log.Warn(fmt.Sprintf(f, a...)) }}

	// Recovery BEFORE subscriptions open — live COMMITTED rows reload,
	// PENDING orphans tombstone and their RELEASE frames go out first.
	if _, releases, rerr := coord.Recover(ctx); rerr != nil {
		return fmt.Errorf("risk: coordinator recover: %w", rerr)
	} else {
		for _, f := range releases {
			if eerr := emitter.EmitMarginCtl(ctx, f.Shard, f.Data); eerr != nil {
				log.Error("risk: orphan release emit failed", "shard", f.Shard, "err", eerr)
			}
		}
	}

	// Per-shard subscriptions: each polls in its own goroutine (Aeron
	// requires single-goroutine Poll per Subscription). Fragments are
	// only valid for the callback duration — copy before processing.
	for _, shard := range shards {
		shard := shard
		sub, serr := aero.AddSubscription(ipc.MarginCtlInURI(shard),
			ipc.MarginCtlInStreamID, func(buf []byte) {
				cp := make([]byte, len(buf))
				copy(cp, buf)
				frames, ferr := coord.IngressFrame(ctx, shard, cp)
				if ferr != nil {
					log.Warn("risk: frame handling failed", "shard", shard, "err", ferr)
				}
				for _, f := range frames {
					if eerr := emitter.EmitMarginCtl(ctx, f.Shard, f.Data); eerr != nil {
						log.Error("risk: response emit failed", "shard", f.Shard, "err", eerr)
					}
				}
			}, 5*time.Second)
		if serr != nil {
			return fmt.Errorf("risk: aeron subscribe shard %d: %w", shard, serr)
		}
		defer sub.Close()
		go func(s *ipcaeron.Subscription, id uint32) {
			for ctx.Err() == nil {
				n := s.Poll(10)
				if n < 0 {
					log.Error("risk: aeron poll error", "shard", id)
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
					continue
				}
				if n == 0 {
					// Margin control is latency-adjacent but not the hot
					// path — a short idle wait keeps the loop cheap.
					time.Sleep(100 * time.Microsecond)
				}
			}
		}(sub, shard)
	}

	// Expiry sweeper — RELEASE frames for reservations past TTL.
	go coord.RunSweeper(ctx, emitter)

	// Engine account-state projector (Phase-3 Task 3.3.1): publishes the
	// PG accounts/balances/positions/instruments corpus into the
	// `account:state` hash at 1s cadence. The C++ AccountStateRefresher
	// polls it for the engine's IAccountState/IPositionState binding —
	// heartbeat TTL (default 30s) fails admission closed if this loop
	// stalls. Single instance: this process is already the deployment's
	// singleton coordinator.
	projector, err := risk.NewAccountStateProjector(pool, rdb.Client, log)
	if err != nil {
		return fmt.Errorf("risk: account state projector: %w", err)
	}
	go projector.Run(ctx)

	// R9 health surface (Task 7.3.6) — the K8s pod probes /health/live +
	// /health/ready on EXC_RISK_HEALTH_ADDR (deploy/k8s/risk-coordinator).
	// Empty addr leaves the listener off (bare-metal/supervisord default).
	if addr := os.Getenv("EXC_RISK_HEALTH_ADDR"); addr != "" {
		mux := api.HealthMux([]api.Dependency{
			{Name: "postgres", Required: true, Probe: func(ctx context.Context) error {
				return api.DependencyErr("postgres", pool.Ping(ctx))
			}},
			{Name: "redis", Required: true, Probe: func(ctx context.Context) error {
				return api.DependencyErr("redis", rdb.Ping(ctx))
			}},
			{Name: "nats", Required: true, Probe: func(context.Context) error {
				if !nats.Connected() {
					return api.DependencyErr("nats", errors.New("disconnected"))
				}
				return nil
			}},
		})
		srv := &http.Server{Addr: addr, Handler: mux,
			ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		go func() {
			if err := srv.ListenAndServe(); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				log.Error("risk: health listener died", "err", err)
			}
		}()
		log.Info("risk: health endpoint", "addr", addr)
	}

	log.Info("risk coordinator started",
		"shards", shards, "shard_map_source", shardSrc.String(),
		"shard_count", len(shards))
	<-ctx.Done()
	log.Info("risk coordinator shutting down")
	return nil
}
