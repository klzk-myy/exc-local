# Matching-Engine Shard Drain & Binary Swap Runbook

**Task:** Phase-09 9.3.16 · **Spec:** §19.6 (8-step procedure), §24 #177 · **Scripts:** `scripts/deploy/shard-swap.sh`, `scripts/deploy/verify-shard.sh` · **Invariants:** zero order loss, zero sequence skips, zero duplicate executions; ingress sheds or buffers <3s.

---

## 1. When to use

Engine binary upgrade, config reload requiring restart, or manual shard
failover onto a warm-standby host. **Do NOT use for the K8s Go services** —
those roll via `deploy/scripts/bluegreen.sh`.

## 2. Preconditions

- Host provisioned per `docs/ops/baremetal-provisioning.md`
  (`provision-matching-host.sh --check` clean).
- New binary staged under `/opt/exchange/bin/releases/matching-engine-<sha256>`
  with symbols intact (`nm` check is enforced by the script).
- ≥60s since the previous shard's swap (spec §19.6 inter-shard gap).
- Redis reachable for mode flag + leader lease inspection
  (`REDIS_HOST`, `EXC_REDIS_CLI`).

## 3. Procedure

```sh
sudo scripts/deploy/shard-swap.sh --shard 0 \
    --binary /opt/exchange/bin/releases/matching-engine-$(sha256sum new-bin | cut -d' ' -f1)
# rehearsal:
scripts/deploy/shard-swap.sh --shard 0 --binary <path> --dry-run
```

| Step | Mechanism | Verify |
|---|---|---|
| 1. Pre-flight | sha256, symbols, `nr_hugepages≥2048` (2MB), `isolcpus` live, config hash | script output OK lines |
| 2. Drain | `system:degradation:mode=Maintenance` in Redis → gateways 503 new orders | `redis-cli GET system:degradation:mode` = `Maintenance`; gateway `/health/ready` → 503 |
| 3. Flush | ≤5s in-flight settle (spec bound) | journal quiet on ingress ring |
| 4. Snapshot | `systemctl stop` → SIGTERM → engine `force_snapshot` + WAL `fsync` (main.cpp drain block) | newest file under `/var/lib/exchange/snapshots/<shard>/` has fresh mtime |
| 5. Swap | `/opt/exchange/bin/matching-engine` symlink → new release | `readlink -f` shows new sha256 |
| 6. Reload | `systemctl start` → snapshot load + WAL tail replay | journal `recovery:` line |
| 7. Verify | `verify-shard.sh`: unit active, binary match, `engine:leader:{shard}` re-acquired, `leader:heartbeat:{shard}` progressing, shm rings attached, no `WAL_RECOVERY_HALT`, no poison-pill growth | all OK |
| 8. Unpause | `system:degradation:mode=Normal` | gateway readiness returns 200; latency/error metrics nominal |

## 4. Rollback

Automatic inside the script (steps 5–7 failures → symlink revert + restart +
verify). Manual equivalent:

```sh
sudo ln -sfn /opt/exchange/bin/releases/matching-engine-<PREV-sha> /opt/exchange/bin/matching-engine
sudo systemctl restart matching-engine@0
scripts/deploy/verify-shard.sh --shard 0
redis-cli SET system:degradation:mode Normal
```

## 5. Edge cases (SDD checklist coverage)

- **Crash during snapshot:** WAL is authoritative; next boot replays from the
  last *confirmed* snapshot + full tail — expect a longer `recovery:` window.
- **WAL replay mismatch on new binary:** `WAL_RECOVERY_HALT` in the journal ⇒
  script rolls back automatically; escalate P1, follow
  `docs/runbooks/wal-recovery-halt.md`.
- **Stuck client socket / drain overrun:** systemd `TimeoutStopSec` bounds the
  stop; a lingering process beyond `KillSignal` escalation leaves the shard
  lock (`flock` on `<wal>/<shard>/engine.lock`) held by a zombie — the new
  process fails closed rather than dual-writing. Check `lslocks` before
  retrying.
- **Warm standby:** run the same script with the unit's `-follower` variant on
  the standby host; promotion path is leader-lease acquisition after the
  primary's `SIGABRT`/`SIGTERM` releases it.
