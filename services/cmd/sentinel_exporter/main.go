// sentinel_exporter — Phase-09 Task 9.3.20 metrics producer for the
// redis-sentinel alert rules (deploy/monitoring/redis-sentinel-alerts.yml).
//
// Polls the sentinel quorum and the redis data nodes and serves the exact
// series the authored rules evaluate:
//
//	sentinel_up                      — reachable sentinel processes
//	sentinel_quorum_ok               — 1 while the monitor sees quorum
//	sentinel_failover_in_progress    — 1 while the master flags a failover
//	redis_master_up                  — 1 while the monitored master answers PING
//	redis_replicas_up                — replicas flagged healthy (no s/o-down)
//	redis_replication_lag_seconds    — max master_last_io_seconds_ago over
//	                                   replicas (the RPO ≤ 5s alert line)
//
// Env:
//
//	SENTINEL_ADDRS   comma-separated host:port sentinels
//	                 (default 127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381)
//	MASTER_NAME      sentinel monitor name (default mymaster)
//	REDIS_ADDRS      comma-separated host:port data nodes polled for
//	                 replication lag — sentinel-announced addresses are
//	                 container-internal, so the host-mapped ports are
//	                 supplied explicitly (default 127.0.0.1:16379..16381)
//	LISTEN           metrics listener (default :9109)
//	POLL             poll interval (default 5s)
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	sentinelUp   atomic.Int64
	quorumOK     atomic.Int64
	failoverProg atomic.Int64
	masterUp     atomic.Int64
	replicasUp   atomic.Int64
	replLagMaxMs atomic.Int64 // milliseconds; exported /1000
	scrapeErr    atomic.Int64
)

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func pollSentinels(ctx context.Context, addrs []string, master string) {
	up := 0
	quorumSeen := false
	failover := false
	var maxHealthy int64
	for _, a := range addrs {
		c := redis.NewSentinelClient(&redis.Options{
			Addr: a, DialTimeout: 500 * time.Millisecond,
			ReadTimeout: 500 * time.Millisecond,
		})
		mv, err := c.Master(ctx, master).Result()
		if err != nil {
			c.Close()
			continue
		}
		up++
		if q, _ := strconv.Atoi(mv["quorum"]); q >= 2 {
			quorumSeen = true
		}
		if strings.Contains(mv["flags"], "failover") ||
			strings.Contains(mv["flags"], "o_down") {
			failover = true
		}
		// Replica count from this sentinel's view.
		healthy := int64(0)
		if reps, err := c.Replicas(ctx, master).Result(); err == nil {
			for _, rv := range reps {
				fl := rv["flags"]
				if !strings.Contains(fl, "s_down") && !strings.Contains(fl, "o_down") &&
					!strings.Contains(fl, "disconnected") {
					healthy++
				}
			}
		}
		if healthy > maxHealthy {
			maxHealthy = healthy
		}
		c.Close()
	}
	sentinelUp.Store(int64(up))
	replicasUp.Store(maxHealthy)
	if quorumSeen && up >= 2 {
		quorumOK.Store(1)
	} else {
		quorumOK.Store(0)
	}
	if failover {
		failoverProg.Store(1)
	} else {
		failoverProg.Store(0)
	}
}

func pollRedis(ctx context.Context, addrs []string) {
	mUp := int64(0)
	var maxLagMs int64
	for _, a := range addrs {
		c := redis.NewClient(&redis.Options{
			Addr: a, DialTimeout: 500 * time.Millisecond,
			ReadTimeout: 500 * time.Millisecond,
		})
		info, err := c.Info(ctx, "replication").Result()
		if err != nil {
			c.Close()
			continue
		}
		role := ""
		lag := ""
		for _, ln := range strings.Split(info, "\n") {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(ln, "role:") {
				role = strings.TrimPrefix(ln, "role:")
			}
			if strings.HasPrefix(ln, "master_last_io_seconds_ago:") {
				lag = strings.TrimPrefix(ln, "master_last_io_seconds_ago:")
			}
		}
		if role == "master" {
			mUp = 1
		}
		if role == "slave" || role == "replica" {
			if s, err := strconv.ParseFloat(lag, 64); err == nil {
				if ms := int64(s * 1000); ms > maxLagMs {
					maxLagMs = ms
				}
			}
		}
		c.Close()
	}
	masterUp.Store(mUp)
	replLagMaxMs.Store(maxLagMs)
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, `# HELP sentinel_up Reachable sentinel processes.
# TYPE sentinel_up gauge
sentinel_up %d
# HELP sentinel_quorum_ok 1 while the monitor sees its quorum.
# TYPE sentinel_quorum_ok gauge
sentinel_quorum_ok %d
# HELP sentinel_failover_in_progress 1 while the master reports a failover.
# TYPE sentinel_failover_in_progress gauge
sentinel_failover_in_progress %d
# HELP redis_master_up 1 while the monitored primary answers.
# TYPE redis_master_up gauge
redis_master_up %d
# HELP redis_replicas_up Replicas flagged healthy by sentinel.
# TYPE redis_replicas_up gauge
redis_replicas_up %d
# HELP redis_replication_lag_seconds Max replica lag vs primary (RPO budget 5s).
# TYPE redis_replication_lag_seconds gauge
redis_replication_lag_seconds %.3f
# HELP sentinel_exporter_scrape_errors Total poll errors.
# TYPE sentinel_exporter_scrape_errors counter
sentinel_exporter_scrape_errors %d
`,
		sentinelUp.Load(), quorumOK.Load(), failoverProg.Load(),
		masterUp.Load(), replicasUp.Load(),
		float64(replLagMaxMs.Load())/1000.0, scrapeErr.Load())
}

func main() {
	sentinels := strings.Split(envOr("SENTINEL_ADDRS",
		"127.0.0.1:36379,127.0.0.1:36380,127.0.0.1:36381"), ",")
	master := envOr("MASTER_NAME", "mymaster")
	nodes := strings.Split(envOr("REDIS_ADDRS",
		"127.0.0.1:16379,127.0.0.1:16380,127.0.0.1:16381"), ",")
	listen := envOr("LISTEN", ":9109")
	poll, _ := time.ParseDuration(envOr("POLL", "5s"))

	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			pollSentinels(ctx, sentinels, master)
			pollRedis(ctx, nodes)
			cancel()
			time.Sleep(poll)
		}
	}()

	http.HandleFunc("/metrics", metrics)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	log.Printf("sentinel_exporter: %s sentinels=%v master=%s nodes=%v poll=%s",
		listen, sentinels, master, nodes, poll)
	log.Fatal(http.ListenAndServe(listen, nil))
}
