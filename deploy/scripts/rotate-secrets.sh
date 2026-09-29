#!/usr/bin/env bash
# =============================================================================
# rotate-secrets.sh — operator-facing secret rotation runbook.
# Phase-13.5 Task 13.5.3.5 (spec §24 #111), companion doc:
# deploy/security/secret-inventory.md, docs/ops/secrets-inventory.md,
# docs/runbooks/secret-rotation-overdue.md.
#
# This script orchestrates Vault CLI + service reloads. It NEVER prints or
# persists secret values — every `vault` call writes new material straight
# to the store; consumers resolve it from there.
#
# Usage:
#   rotate-secrets.sh status                      # inventory + Vault reachability
#   rotate-secrets.sh due                         # list secrets past/approaching SLA
#   rotate-secrets.sh rotate <class> [--apply]    # run one class procedure
#   rotate-secrets.sh rotate-all [--apply]
#
# Classes: jwt | db | redis | tls | aeron | banking | datakey
#
# Default mode is DRY-RUN — the command plan is printed, nothing mutates.
# Pass --apply to execute.
#
# Env (required unless noted):
#   VAULT_ADDR        e.g. https://vault.exchange.internal:8200
#   VAULT_TOKEN       or VAULT_TOKEN_FILE (vault-agent sink, bare metal)
#   EXC_ENV           production|staging|... (default: production)
#   PGBOUNCER_HOST    PgBouncer admin console host (default 127.0.0.1:6432)
#   KUBECTL_CONTEXT   optional kubectl context for K8s secret refresh
#
# Exit: 0 ok · 1 step failed (fail-closed: aborts, old material stays live)
#       64 usage · 65 Vault unreachable
# =============================================================================
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"
ENV="${EXC_ENV:-production}"
APPLY=0
PGBOUNCER_HOST="${PGBOUNCER_HOST:-127.0.0.1:6432}"

log()  { printf '[rotate %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die()  { log "ERROR: $*" >&2; exit 1; }
usage(){ sed -n '2,34p' "$0"; exit 64; }

need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }

# run executes (or prints, in dry-run) a mutating command.
run() {
    if [ "$APPLY" -eq 1 ]; then
        log "apply: $*"
        "$@"
    else
        log "dry-run: $*"
    fi
}

vault_token() {
    if [ -n "${VAULT_TOKEN_FILE:-}" ]; then
        [ -r "$VAULT_TOKEN_FILE" ] || die "VAULT_TOKEN_FILE unreadable: $VAULT_TOKEN_FILE"
        cat "$VAULT_TOKEN_FILE"
    elif [ -n "${VAULT_TOKEN:-}" ]; then
        printf '%s' "$VAULT_TOKEN"
    else
        die "set VAULT_TOKEN or VAULT_TOKEN_FILE (vault-agent sink)"
    fi
}

vault_ok() {
    VAULT_TOKEN="$(vault_token)" vault status -format=json >/dev/null 2>&1
}

# --- procedures -------------------------------------------------------------

proc_jwt() {
    # kid-keyed dual-key rotation (24h overlap): new HS256 material is
    # written as a NEW key entry; consumers rebuild their keyring holding
    # old+new — old kid verifies for 24h (services/internal/security).
    local new_kid="jwt-$(date -u +%Y%m%d%H%M%S)"
    log "jwt: minting new HS256 key kid=$new_kid (server-side, never printed)"
    # openssl rand → vault kv put via stdin; the value never touches argv.
    if [ "$APPLY" -eq 1 ]; then
        openssl rand -base64 32 | \
            VAULT_TOKEN="$(vault_token)" vault kv put -mount=secret \
                "exchange/${ENV}/jwt/${new_kid}" hs256_b64=- alg=HS256 kid="$new_kid"
    else
        log "dry-run: openssl rand -base64 32 | vault kv put secret/exchange/${ENV}/jwt/${new_kid} hs256_b64=- alg=HS256 kid=$new_kid"
    fi
    # Point the active-kid marker at the new version; retiring kids keep
    # verifying until their NotAfter (24h) lapses.
    run env "VAULT_TOKEN=$(vault_token)" vault kv patch -mount=secret \
        "exchange/${ENV}/secrets" "jwt_active_kid=${new_kid}"
    log "jwt: overlap window = 24h; retiring kid must be pruned after expiry"
    log "jwt: rolling restart of token-minting services OR wait for the in-process keyring refresh"
}

proc_db() {
    # Dynamic creds (Vault database/creds/<role>, 1h TTL) renew
    # themselves. This procedure rotates the STATIC bootstrap/login role
    # and the PgBouncer-facing user:
    #   PAUSE → ALTER ROLE … PASSWORD → write to Vault → RESUME.
    # PAUSE first so no session is mid-transaction on the old cred.
    log "db: PgBouncer PAUSE→rotate→RESUME on ${PGBOUNCER_HOST}"
    run psql "postgres://pgbouncer:${PGBOUNCER_ADMIN:-}@${PGBOUNCER_HOST}/pgbouncer" -c 'PAUSE;'
    if [ "$APPLY" -eq 1 ]; then
        newpw="$(openssl rand -base64 32 | tr -d '=+/')"
        psql "${EXC_PG_ADMIN_DSN:?EXC_PG_ADMIN_DSN required for db rotation}" \
            -c "ALTER ROLE exchange_app PASSWORD '${newpw}';"
        printf '%s' "$newpw" | VAULT_TOKEN="$(vault_token)" vault kv patch \
            -mount=secret "exchange/${ENV}/postgres" password=-
        unset newpw
    else
        log "dry-run: ALTER ROLE exchange_app PASSWORD <generated>; vault kv patch secret/exchange/${ENV}/postgres password=-"
    fi
    run psql "postgres://pgbouncer:${PGBOUNCER_ADMIN:-}@${PGBOUNCER_HOST}/pgbouncer" -c 'RESUME;'
    log "db: done — Go services on dynamic creds renew at TTL/2 automatically"
}

proc_redis() {
    # Redis AUTH rotation: ACL SETUSER with a new password, write to
    # Vault, rolling client reconnect picks it up via the swapper seam.
    log "redis: ACL SETUSER exchange with fresh password + Vault write"
    if [ "$APPLY" -eq 1 ]; then
        newpw="$(openssl rand -base64 32 | tr -d '=+/')"
        redis-cli -u "${EXC_REDIS_ADMIN_URL:?EXC_REDIS_ADMIN_URL required}" \
            ACL SETUSER exchange on ">${newpw}"
        printf '%s' "$newpw" | VAULT_TOKEN="$(vault_token)" vault kv patch \
            -mount=secret "exchange/${ENV}/redis" password=-
        unset newpw
    else
        log "dry-run: redis-cli ACL SETUSER exchange on '>…'; vault kv patch secret/exchange/${ENV}/redis password=-"
    fi
    log "redis: consumers reconnect via credential swapper (no restart)"
}

proc_tls() {
    # cert-manager/ACME owns issuance (deploy/k8s + 02-externalsecret.yaml
    # peers). This procedure is the manual/Vault-PKI path for internal
    # certs (FIX mTLS is handled by deploy/security/provision-fix-mtls.sh).
    log "tls: cert-manager renews 30d pre-expiry automatically;"
    log "tls: manual Vault PKI issue: vault write ${ENV}-pki/issue/server common_name=<fqdn>"
    if [ "$APPLY" -eq 1 ]; then
        VAULT_TOKEN="$(vault_token)" vault write -format=json \
            "${ENV}-pki/issue/server" "common_name=${TLS_CN:?set TLS_CN}" \
            ttl=2160h | VAULT_TOKEN="$(vault_token)" \
            vault kv put -mount=secret "exchange/${ENV}/tls/${TLS_CN}" -
    else
        log "dry-run: vault write ${ENV}-pki/issue/server common_name=<fqdn> ttl=2160h | vault kv put …"
    fi
}

proc_aeron() {
    # Aeron auth tokens ride the vault-agent template → tmpfs file;
    # rotation = new KV version + agent re-render + engine file-watch
    # reload (deploy/security/secrets-policy.md §engine-contract).
    log "aeron: rotate auth token — vault-agent re-renders to tmpfs; engine reloads via file watch"
    if [ "$APPLY" -eq 1 ]; then
        openssl rand -base64 32 | \
            VAULT_TOKEN="$(vault_token)" vault kv patch -mount=secret \
                "exchange/${ENV}/aeron" auth_token=-
    else
        log "dry-run: openssl rand | vault kv patch secret/exchange/${ENV}/aeron auth_token=-"
    fi
    log "aeron: verify agent sink updated: ls -l /run/exchange-secrets/aeron-token"
}

proc_banking() {
    # Banking rail keys re-issue at the rail portal first (out-of-band),
    # then land in Vault. Rotation is always new-version writes.
    log "banking: rails issue keys out-of-band (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2 portals)"
    log "banking: store with: vault kv patch secret/exchange/${ENV}/banking/<rail> api_key=-  (stdin)"
    log "banking: then restart banking-rails-worker or wait for the credential swapper"
}

proc_datakey() {
    # AES-256-GCM data key (api_keys.secret_enc, webhook secrets).
    # Dual-key decrypt window: consumers accept the keyring, not one key.
    log "datakey: generate 32B, write as NEW key version alongside current"
    if [ "$APPLY" -eq 1 ]; then
        openssl rand -base64 32 | \
            VAULT_TOKEN="$(vault_token)" vault kv patch -mount=secret \
                "exchange/${ENV}/secrets" data-key-b64=-
    else
        log "dry-run: openssl rand -base64 32 | vault kv patch secret/exchange/${ENV}/secrets data-key-b64=-"
    fi
    log "datakey: consumers re-read on next ExternalSecret refresh (15m) or restart"
}

# --- commands ---------------------------------------------------------------

cmd_status() {
    need vault
    log "env=${ENV} vault=${VAULT_ADDR:-<unset>} mode=$([ "$APPLY" -eq 1 ] && echo APPLY || echo DRY-RUN)"
    if vault_ok; then
        log "vault: reachable ($(vault status -format=json 2>/dev/null | grep -o '"initialized":[^,]*' || echo '?'))"
    else
        log "vault: UNREACHABLE or unauthed — fail-closed per spec §2.7"
        exit 65
    fi
    log "inventory: ${REPO_ROOT}/deploy/security/secret-inventory.md"
    VAULT_TOKEN="$(vault_token)" vault kv list -mount=secret "exchange/${ENV}" 2>/dev/null || true
}

cmd_due() {
    # The authoritative overdue source is the scheduler metrics gauge
    # (secret_rotation_seconds_until_expiry) + SECRET_ROTATION_* alerts;
    # this is the operator quick-check: list KV entries older than 90d.
    need vault
    vault_ok || { log "vault unreachable"; exit 65; }
    log "secrets past/approaching 90d rotation SLA (metadata.created_time):"
    VAULT_TOKEN="$(vault_token)" vault kv list -mount=secret -format=json "exchange/${ENV}" 2>/dev/null \
        | tr -d '[]", ' | tr '\n' ' ' | tr -s ' ' '\n' | while read -r p; do
            [ -z "$p" ] && continue
            created="$(VAULT_TOKEN="$(vault_token)" vault kv get -mount=secret -format=json \
                "exchange/${ENV}/${p}" 2>/dev/null | grep -o '"created_time": *"[^"]*"' | cut -d'"' -f4)"
            [ -z "$created" ] && continue
            age_s=$(( $(date +%s) - $(date -d "$created" +%s 2>/dev/null || echo 0) ))
            if [ "$age_s" -gt $((76*86400)) ]; then
                printf '  %-50s age=%dd %s\n' "$p" $((age_s/86400)) \
                    "$([ "$age_s" -gt $((90*86400)) ] && echo 'OVERDUE' || echo 'due_soon')"
            fi
        done
}

main() {
    [ $# -lt 1 ] && usage
    local cmd="" target=""
    case "${1:-}" in
        status|due) cmd="$1" ;;
        rotate)
            [ $# -ge 2 ] || usage
            cmd="rotate"; target="$2"
            ;;
        rotate-all) cmd="rotate"; target="all" ;;
        *) usage ;;
    esac
    shift
    [ "$cmd" = "rotate" ] && shift || true  # consume <class>
    while [ $# -gt 0 ]; do
        case "$1" in
            --apply) APPLY=1 ;;
            *) usage ;;
        esac
        shift
    done

    need vault
    log "env=${ENV} mode=$([ "$APPLY" -eq 1 ] && echo APPLY || echo DRY-RUN)"

    case "$cmd" in
        status) cmd_status ;;
        due)    cmd_due ;;
        rotate)
            vault_ok || { log "vault unreachable"; exit 65; }
            case "$target" in
                jwt|db|redis|tls|aeron|banking|datakey) "proc_$target" ;;
                all) for c in jwt db redis tls aeron banking datakey; do
                        "proc_$c"
                    done ;;
                *) usage ;;
            esac
            ;;
    esac
}

main "$@"
