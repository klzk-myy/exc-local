#!/usr/bin/env bash
# provision-fix-mtls.sh — issue a FIX client mTLS cert + CompID credential pack
# from Vault PKI (Phase-13.5 Task 13.5.3.6, spec §24 #213).
#
# Real Vault only — this script requires the `vault` CLI and a live
# VAULT_ADDR + token. There is no dev fake: provisioning against a
# stand-in source would mint credentials the counterparty boundary can't
# trust. Fail-closed everywhere.
#
#   ./provision-fix-mtls.sh --compid EXCHFIX001 [--env production]
#                           [--pki-mount fix-pki] [--pki-role fix-client]
#                           [--ttl 8760h] [--out-dir <dir>]
#                           [--bindings-file <path>]
#
# Output: <out>/<compid>/ containing
#   client.crt     leaf certificate            (0600)
#   client.key     private key — see-once      (0600)
#   ca-chain.pem   issuing chain               (0644)
#   compid.txt     CompID the cert binds to    (0644)
#   pack.json      {compid, cert_sha256, serial, not_after, ttl}
#
# The private key leaves Vault exactly once (the issue response body).
# Register the CompID↔SHA-256 fingerprint binding in the FIX session
# entitlement store — Phase-18 consumes it at session logon; a mismatched
# cert is rejected SESSION_NOT_ENTITLED before any order traffic.
set -euo pipefail

COMPID="" ENV="production" PKI_MOUNT="fix-pki" PKI_ROLE="fix-client"
TTL="8760h" OUT_DIR="fix-client-packs" BINDINGS_FILE=""

while [ $# -gt 0 ]; do
    case "$1" in
        --compid)         COMPID="${2:?missing value}"; shift 2 ;;
        --env)            ENV="${2:?missing value}"; shift 2 ;;
        --pki-mount)      PKI_MOUNT="${2:?missing value}"; shift 2 ;;
        --pki-role)       PKI_ROLE="${2:?missing value}"; shift 2 ;;
        --ttl)            TTL="${2:?missing value}"; shift 2 ;;
        --out-dir)        OUT_DIR="${2:?missing value}"; shift 2 ;;
        --bindings-file)  BINDINGS_FILE="${2:?missing value}"; shift 2 ;;
        -h|--help)        sed -n '2,30p' "$0"; exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

[ -n "$COMPID" ] || { echo "ERROR: --compid required" >&2; exit 2; }
# FIX CompID charset — session-layer identifier, no shell/path chars.
case "$COMPID" in
    *[!A-Za-z0-9_.-]*|'') echo "ERROR: invalid CompID '$COMPID'" >&2; exit 2 ;;
esac

die() { echo "ERROR: $1" >&2; exit "${2:-1}"; }

command -v vault >/dev/null 2>&1 || die "vault CLI not installed — production Vault required (no fake source exists)"
command -v jq    >/dev/null 2>&1 || die "jq required"
command -v openssl >/dev/null 2>&1 || die "openssl required"
[ -n "${VAULT_ADDR:-}" ] || die "VAULT_ADDR unset"
[ -n "${VAULT_TOKEN:-}" ] || die "VAULT_TOKEN unset (or configure vault-agent auth)"

vault status >/dev/null 2>&1 || die "Vault unreachable at $VAULT_ADDR — refusing (CONFIG_LOAD_FAILED contract)"

ISSUE_PATH="${PKI_MOUNT}/issue/${PKI_ROLE}"
CN="${COMPID,,}.fix.clients.exchange"   # stable per-CompID identity, lowercase DNS-safe

resp="$(mktemp)"; trap 'rm -f "$resp"' EXIT
if ! vault write -format=json "$ISSUE_PATH" \
        common_name="$CN" ttl="$TTL" \
        > "$resp" 2>/dev/null; then
    die "vault write $ISSUE_PATH failed — check PKI mount/role, token policy (fix-clients-policy)"
fi

serial="$(jq -r '.data.serial_number' "$resp")"
not_after="$(jq -r '.data.expiration' "$resp")"
cert_pem="$(jq -r '.data.certificate' "$resp")"
key_pem="$(jq -r '.data.private_key' "$resp")"
ca_pem="$(jq -r '.data.issuing_ca' "$resp")"
[ "$cert_pem" != "null" ] && [ "$key_pem" != "null" ] || die "Vault response missing certificate/private_key"
# Renewals reuse the same issue path; capture the new lease id for audit.
lease_id="$(jq -r '.lease_id // ""' "$resp")"

dir="$OUT_DIR/$COMPID"
install -d -m 0700 "$dir"
umask 077
printf '%s' "$cert_pem" > "$dir/client.crt"
printf '%s' "$key_pem"  > "$dir/client.key"
printf '%s' "$ca_pem"   > "$dir/ca-chain.pem"
printf '%s\n' "$COMPID" > "$dir/compid.txt"
chmod 0600 "$dir/client.crt" "$dir/client.key"
chmod 0644 "$dir/ca-chain.pem" "$dir/compid.txt"

fp="$(openssl x509 -in "$dir/client.crt" -noout -fingerprint -sha256 | cut -d= -f2)"

cat > "$dir/pack.json" <<EOF
{"compid":"$COMPID","env":"$ENV","cn":"$CN","cert_sha256":"$fp",
 "serial":"$serial","not_after_unix":$not_after,"ttl":"$TTL",
 "lease_id":"$lease_id","pki_mount":"$PKI_MOUNT","issued_by":"provision-fix-mtls.sh"}
EOF
chmod 0644 "$dir/pack.json"

# Optional: append the entitlement binding for Phase-18 consumption.
if [ -n "$BINDINGS_FILE" ]; then
    printf '%s\t%s\t%s\t%s\n' "$COMPID" "$fp" "$ENV" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        >> "$BINDINGS_FILE"
fi

echo "provisioned: $dir"
echo "  compid     = $COMPID"
echo "  cert_sha256= $fp"
echo "  serial     = $serial"
echo "  ttl        = $TTL (renewable via lease $lease_id)"
echo "  next: bind compid<->fingerprint in the FIX session store;"
echo "        hand client.crt+client.key to the client ONCE (see-once, like API keys);"
echo "        schedule renewal at 30d-before-expiry (same window as TLS, §24 #111)."
