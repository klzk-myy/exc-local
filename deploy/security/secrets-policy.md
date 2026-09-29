# Secrets Policy — Bare-Metal Lifecycle (Phase-13.5 Task 13.5.3.6, spec §24 #213)

Companion to [`secret-inventory.md`](./secret-inventory.md) (what exists)
and [`../../docs/ops/secrets-inventory.md`](../../docs/ops/secrets-inventory.md)
(the operating procedure). This file defines **how** secrets reach the
bare-metal C++ engine and how every credential class rotates without a
Kubernetes control plane.

Threat model: the engine host runs the matching engine (`User=exchange`),
`aeronmd`, `vault-agent`, and the Go bridge — same kernel, different
privileges. Everything else (PgBouncer/Postgres, Redis, Vault, banking
rails, FIX counterparties) is cross-host.

## 1. Store of record

HashiCorp Vault (KV v2 + `database` secrets engine + PKI), or the cloud
KMS equivalent. The Go-side client is `internal/security.VaultSource` —
a real Vault HTTP-API client shape (KV v2 read, `database/creds` issue,
`sys/leases/renew|revoke`); the **dev adapter (`DevSource`) is explicitly
labeled** and refused whenever `EXC_SECRETS_REQUIRED=production` or the
deployment env is production (`config.IsProduction`). Fail-closed code:
`CONFIG_LOAD_FAILED`.

## 2. Engine credential contract (vault-agent → tmpfs → file watch)

The C++ engine consumes **no secrets at boot today** — `core/src/main.cpp`
is argv-only (`-shard`, `-wal-dir`, `-redis`, …) and reads no config file,
no env block, no credential. That is the secure baseline; the contract
below binds the seams that *will* carry credentials (Redis AUTH for the
`-redis` halt latch, the Aeron channel admission token, any future
control-channel TLS key).

**Delivery chain:** Vault KV → `vault-agent` (role:
[`deploy/ansible/roles/vault-agent/`](../ansible/roles/vault-agent/))
→ renders to the tmpfs mount `/run/exchange-secrets/` (nosuid, nodev,
noexec, `exchange:exchange` 0750, files 0640) → engine reads at open.

**Reload contract (the file-watch seam):** the agent re-renders a file
on KV version change; the engine watches its consumed paths for
`IN_CLOSE_WRITE`/`IN_MOVED_TO` and re-reads. Semantics:

1. Open-time read only — a file the engine never opens is never a
   credential source (argv points at *paths*, never values).
2. A failed re-read (missing file, empty, malformed) is **fail-closed**:
   the engine keeps the previous credential until it is rejected by the
   peer, never fabricates or guesses one. A render failure surfaces via
   the agent's render-error alert path, not via silent staleness.
3. Empty/absent secret at **boot** is a startup abort (`CONFIG_LOAD_FAILED`
   class), identical to the Go contract.
4. Files under `/run/exchange-secrets` are RAM-only — reboot wipes them;
   the agent re-auths via AppRole (its `secret_id` bootstrap is
   `remove_secret_id_file_after_reading`, so the host retains no
   long-lived login secret).

**Known gap (honest seam):** `core/src/redis/RespClient.hpp` has no
AUTH support — the `-redis` control path currently assumes a trusted
loopback/isolated segment. When Redis AUTH lands, the engine reads
`/run/exchange-secrets/redis-password` per the contract above; until then
the control path must sit on the isolated VLAN only. Recorded here, not
hidden.

## 3. Aeron IPC trust boundary

### 3.1 Same host (shm)

Two segment families:

| Path | Producer | Files |
|---|---|---|
| `/dev/shm/aeron-exchange/` | `aeronmd.service` (`User=exchange`) | `cnc.dat`, log buffers |
| `/dev/shm/<ipc-base>_<shard>_{in,out}` | `matching-engine@<shard>.service` | SPSC rings (`SharedMemChannel`, `shm_open` mode 0600 at create) |

Contract: **all segments `exchange:exchange` mode 0660, dir 0750** —
engine + bridge attach via group membership; nothing else on the host
can map the rings. `shm_open` creates at 0600 (correct for single-user);
the group upgrade to 0660 is applied by:

```
deploy/baremetal/harden-ipc-perms.sh --apply   # enforce
deploy/baremetal/harden-ipc-perms.sh --check   # audit/CI mode
```

`aeronmd.service` already `chmod 0660`s `cnc.dat`; the script extends
that to every segment file + the dir. Run it as an `ExecStartPost` or a
timer unit after `aeronmd`/`matching-engine` start.

### 3.2 Cross host (UDP media)

Cross-host Aeron traffic (engine→bridge fan-out on a second host, DR
shadow) is **not** protected by file perms — it rides a WireGuard or
IPsec transport so Aeron UDP never traverses a routed network in the
clear:

- WireGuard: point-to-point `wg0` between engine host and bridge host;
  `aeron.conf` `aeron.udp.channel.endpoint` bound to the tunnel address;
  `AllowedIPs` scoped to the channel subnet only.
- IPsec alternative: transport-mode ESP between the fixed host pair
  (`/etc/ipsec.d/exchange-aeron.conf`), same endpoint binding.

The Aeron channel **auth token** (§2 contract, `aeron-token` tmpfs file)
is the admission check inside the tunnel — the tunnel is the transport
boundary, the token is the identity check.

## 4. Database credential rotation

### 4.1 Static bootstrap user (PgBouncer path)

`deploy/scripts/rotate-secrets.sh rotate db` performs the zero-downtime
sequence:

1. `psql pgbouncer://admin -c 'PAUSE;'` — no new transactions on the old
   credential;
2. `ALTER ROLE exchange_app PASSWORD …` (generated server-side, never
   printed);
3. `vault kv patch exchange/<env>/postgres password=-` — new KV version;
4. `RESUME;` — pool resumes on the new credential; Go services reload
   pools via the `Swapper` (§4.2), not restart.

### 4.2 Dynamic credentials (Go services)

`internal/security.VaultSource.Issue` → `GET /v1/database/creds/<role>`
returns a leased credential with **TTL contract = 1h** (leases >2h are
rejected as contract violations). `Swapper.LeaseRenewal` renews at TTL/2,
re-issues on renewal failure, and swaps the live pool handle atomically
(`Rotate` builds+verifies the new handle before the swap; a failed build
leaves the current pool untouched). Consumers hold `Swapper.Get()`,
never the raw credential — **rotation without restart** by construction.

Redis follows the same seam: `ACL SETUSER` + Vault patch → clients
reconnect through the swapper.

## 5. FIX session credentials (CompID + mTLS)

Phase-18 owns FIX consumption (Task 18.3.11 client certification pack);
this task owns **provisioning**:

- Vault PKI mount `fix-pki`, role `fix-client`, TTL ≤12mo;
- `deploy/security/provision-fix-mtls.sh --compid <ID>` issues
  `CN=<compid>.fix.clients.exchange`, writes a cert pack
  (`client.crt`/`client.key`/`ca-chain.pem`/`compid.txt`, mode 0600 in a
  0700 dir) and prints the SHA-256 fingerprint;
- the platform registers the **CompID↔fingerprint binding** in the FIX
  session entitlement store (mismatch → `SESSION_NOT_ENTITLED`); the
  private key leaves Vault exactly once (the issue response) — same
  see-once contract as API keys (§6).

## 6. API key distribution (verified against code)

`internal/auth/apikey.go`:

- `CreateHMAC` generates the secret, seals it with `SecretBox`
  (AES-256-GCM under the `data-key`) and persists only `secret_enc` —
  the plaintext returns to the caller once and is never stored;
- asymmetric (Ed25519/RSA) keys store the **public half only** — private
  material never enters the platform (§24 #283);
- `RevealHMACSecret` exists only for verification-time unwrap;
- rotation lineage (`rotates_from_id`/`overlap_until`, ≤72h) is the
  client-facing dual-key window matching §24 #111's overlap requirement.

Institutional clients receive the secret once at issuance (admin portal /
vault transit); the server side can verify forever but can never hand the
secret to a second party — there is no plaintext to exfiltrate.

## 7. Validation — no plaintext secrets anywhere

Two layers, both checked in:

1. `scripts/ci/no-plaintext-secrets.sh` — pattern scan of deploy configs,
   compose, env files, Dockerfiles, K8s/Ansible/systemd assets for
   credential-shaped literals. Green = no committed secrets.
   Allowlisted shapes: `${VAR}`/`${VAR:-}` envsubst placeholders,
   secret-name references (`secretKey:`/`remoteRef`), the explicit dev
   `exchange_dev` DSN.
2. `.gitleaks.toml` (Phase-01.5 Task 1.5.3.4) — the entropy-aware
   full-repo scan wired to the security workflow.

Plus the runtime gate: `internal/security.SourceFromEnv` refuses dev
adapters under `EXC_SECRETS_REQUIRED=production`, and the
`TestDrillVaultPartitionFailClosed` /
`TestDrillNoDevFallbackWhenRequired` drills (Task 13.5.3.7) execute the
fail-closed boot on every test run.
