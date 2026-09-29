# vault-agent role — Phase-13.5 Task 13.5.3.6 (spec §24 #213)

Provisions the HashiCorp Vault **agent** as a systemd sidecar on
bare-metal matching-engine hosts. The agent authenticates via AppRole,
sinks its token to tmpfs, and renders managed secrets to tmpfs files —
the C++ engine reads credentials from file paths and reloads on change
(the file-watch contract is specified in
`deploy/security/secrets-policy.md` §2).

## Guarantees

| Property | Mechanism |
|---|---|
| No secrets on disk | tmpfs sink `/run/exchange-secrets` (nosuid,nodev,noexec) |
| No long-lived host credential | AppRole `secret_id` deleted after first auth (`remove_secret_id_file_after_reading`) |
| Least privilege | rendered files `exchange:exchange` 0640; agent `ProtectSystem=strict`, `NoNewPrivileges` |
| Fail closed | `error_on_missing_key` render failure → alert, never an empty credential file; engine file-open failure is a boot abort |
| Boot ordering | `vault-agent-exchange.service` runs `Before=matching-engine.target`; drop-in adds `After=` on the engine unit |

## Prerequisites

- `vault` binary at `/usr/bin/vault` (agent mode; the daemon is NOT
  installed — the agent package provides it).
- AppRole `role_id`/`secret_id` delivered out-of-band to
  `/etc/exchange/vault/{role_id,secret_id}` — this role **asserts they
  exist** and never creates or stores them.
- Vault policy bound to the AppRole grants `read` on
  `secret/data/exchange/<env>/*` and `update` on `sys/leases/renew`
  for the agent's own token only.

## Engine-facing files (see defaults for the full list)

| Rendered path | Secret | Consumer |
|---|---|---|
| `/run/exchange-secrets/.token` | agent token sink | engine-adjacent Go helpers (VaultSource `TokenFile`) |
| `/run/exchange-secrets/aeron-token` | `exchange/<env>/aeron.auth_token` | matching-engine IPC admission |
| `/run/exchange-secrets/redis-password` | `exchange/<env>/redis.password` | `-redis` control-path RespClient AUTH |

Consumers read the file at open and re-read on `CLOSE_WRITE` — the
agent's re-render is the reload trigger, no SIGHUP, no restart.
