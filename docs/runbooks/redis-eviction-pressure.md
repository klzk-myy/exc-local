# Runbook: Redis memory & eviction pressure (`RedisEvictionsObserved`, `RedisEvictionsSustained`, `RedisMemoryHeadroomLow`, `RedisMemoryHeadroomCritical`)

**Severity:** P2 (evictions observed / headroom low) / P1 (sustained evictions / critical headroom) · **Metrics:** `redis_evicted_keys`, `redis_memory_used_bytes`, `redis_memory_max_bytes` (redis_exporter) · **Owner:** SRE

Two Redis roles carry opposite eviction contracts (deploy/redis/):

| Instance | Config | Eviction meaning |
|---|---|---|
| Coordination (primary) | `redis.conf`: `maxmemory 4gb` + `noeviction` | Evictions must NEVER happen — a nonzero `redis_evicted_keys` means the running process is mis-configured (wrong conf on a replica/replacement). The real symptom at 100% is **write refusal**: rate-limit decisions, circuit-breaker state, degradation mode, session writes start failing. |
| Cache | `redis-cache.conf`: `maxmemory 1gb` + `volatile-lru` | Evictions are legal churn; sustained rate = memory pressure + degraded cache hit ratio. |

## Symptom

`redis_evicted_keys` counter increasing (any evictions — P2 single spike, P1 sustained >10/s), or `redis_memory_used_bytes / redis_memory_max_bytes` crossing 85% (P2 ticket) / 95% (P1 page).

## Diagnosis

1. **Which instance?** `{{ $labels.instance }}` — coordination instances bind :6379 per `redis.conf`; the cache instance per `redis-cache.conf`. Check the `role`/`instance` label before acting.
2. **Confirm the live policy:** `redis-cli CONFIG GET maxmemory-policy` + `CONFIG GET maxmemory` — a `volatile-*`/`allkeys-*` policy on the coordination instance is a config drift defect (redis.conf mandates `noeviction`); `noeviction` on the cache instance is also drift.
3. **What is growing?** `INFO memory` (`mem_clients_*`, `mem_fragmentation_ratio`, `used_memory_dataset`); `INFO keyspace` for DB growth; `--bigkeys` / `MEMORY USAGE` sampling for offender keys. Rate-limit traffic (`rl:tb:*`, `rl:usage:*`, `ip_ban:*`) and circuit-breaker hashes are the coordination instance's largest families.
4. **Correlate:** `RedisMemoryHeadroomLow` on the coordination instance + falling `http_requests_total` at the gateway = write refusal already biting (SET errors surface as limiter fallback / breaker store failures — check `L1ErrorsSustained`).

## Mitigation

1. **Coordination instance approaching 100% (noeviction):** act before write refusal —
   - grow `maxmemory` live: `CONFIG SET maxmemory <new>` buys time (persist in `deploy/redis/redis.conf` after);
   - shed pressure: identify the growing keyspace family; rate-limit keys expire on TTL but `rl:usage:*` hashes carry 48h TTL — a traffic anomaly (see `RateLimitUtilizationHigh`) can balloon them;
   - never `FLUSHALL` — that drops halts, breakers and ban state (zero-loss violation).
2. **Cache instance evicting:** raise `maxmemory` (cache config is 1gb — headroom is cheap) or shrink TTL on the offending family; hit-ratio dashboards confirm relief.
3. **Config drift on either role:** restore the file-managed value (`deploy/redis/redis*.conf`), then investigate how the running process diverged — an operator `CONFIG SET` or a swapped instance image.
4. **AOF/fragmentation bloat:** `mem_fragmentation_ratio` >1.5 with healthy dataset size → `MEMORY PURGE` (jemalloc) or planned `BGREWRITEAOF` during the maintenance window.

## Escalation

- Sustained evictions (P1) or critical headroom (P1) → SRE primary; if the coordination instance starts refusing writes (clients log MISCONF/OOM errors), escalate to P0-adjacent handling: rate-limit and breaker state are in-flight safety machinery — [circuit-breaker-open.md](./circuit-breaker-open.md) for the admission-fail-closed side effects.
- Policy drift discovered → file a compliance-adjacent ticket; a `noeviction` regression is a silent safety control removal.
- Recurring cache churn → capacity review; `CapacityMemoryHeadroom` may already be trending (capacity pack).
