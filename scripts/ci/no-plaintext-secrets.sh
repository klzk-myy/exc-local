#!/usr/bin/env bash
# =============================================================================
# no-plaintext-secrets.sh — pen-test-adjacent CI gate (Phase-13.5
# Task 13.5.3.6 item 6): scan deploy configs, compose files, env files,
# Dockerfiles, Ansible/systemd/K8s assets for secret-shaped literal
# values. This is a PATTERN scan — it never sees or handles real
# secrets; it only proves none are committed.
#
# Complements .gitleaks.toml (full-repo entropy scan): this file is the
# deploy-surface check that runs without a gitleaks binary.
#
# Scope (config-shaped files only — docs/*.md are prose by design and
# gitleaks covers them):
#   deploy/**.{yml,yaml,conf,cfg,ini,properties,json,toml,env,service}
#   deploy/**/*.service, deploy/**/Dockerfile*, docker-compose*.yml
#   config/**, services/config.example.yaml, infrastructure/**
#   .env*, *.env anywhere outside .git
#
# Flagged shapes:
#   1. key:value / key=value where key names a credential AND the value
#      is a non-placeholder literal (>=8 chars, no ${...}, no quotes-empty)
#   2. URL-embedded creds: scheme://user:password@host
#   3. PEM private-key blocks: -----BEGIN ... PRIVATE KEY-----
#   4. AWS access key ids / JWT-shaped tokens in config files
#
# Placeholders never flag: ${VAR}, ${VAR:-d}, "", '', ***, <placeholder>,
# CHANGE_ME/REPLACE_ME/example/dummy, and the repo's explicit dev
# placeholder `exchange_dev` (dev-only DSN in config.example.yaml).
#
# Exit: 0 clean · 1 findings
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

FINDINGS=0
report() { # file line text
    FINDINGS=$((FINDINGS + 1))
    printf 'SECRET-LIKE LITERAL  %s:%s  %s\n' "$1" "$2" "$3" >&2
}

# is_placeholder <value> -> 0 when the value is a non-secret placeholder.
is_placeholder() {
    local v="${1#\"}"; v="${v%\"}"; v="${v#\'}"; v="${v%\'}"
    case "$v" in
        ""|"-"|"***"|"****"|"x"|"xx"|"xxx"|"XXXX"|"null"|"none"|"nil"|"~") return 0 ;;
        '${'*|'$'*|'%'*'}'|'%'*'%') return 0 ;;            # env expansion
        '<'*'>'|'{{'*'}}'|'#{'*'}') return 0 ;;            # template markers
        *[Ee][Nn][Vv]'_FILE'*|'/etc/'*|'/run/'*|'/var/'*|'/dev/'*) return 0 ;;
        *'[0-9a-f]'*|'0x'*) ;;                              # fall through
    esac
    case "$(printf '%s' "$v" | tr 'A-Z' 'a-z')" in
        *change_me*|*changeme*|*replace_me*|*placeholder*|*example*|*dummy*|*redacted*|*redact*|*your_*|*insert_*|*todo*|*todo*|*sample*|*fake*|*dev_only*|*devonly*|*not_a_secret*|*notasecret*) return 0 ;;
        exchange_dev|dev|development|test|testing|true|false|yes|no|on|off|required|optional|disable|disabled|enable|enabled) return 0 ;;
        *_dev|*_dev_*) return 0 ;;  # named dev credentials (exchange_dev precedent — trino_dev etc.)
    esac
    # Canonical zero-bytes encodings are placeholders, not secrets:
    # 32×0x00 base64 → all-A (drill data_key), hex zeros likewise.
    if printf '%s' "$v" | grep -qE '^A+={0,2}$|^0{16,}$'; then return 0; fi
    # File-path values for *_FILE keys are references, not secrets.
    case "$v" in
        /*|*'/'*) return 0 ;;
    esac
    return 1
}

# collect_files — the deploy-surface file set.
collect_files() {
    {
        find deploy config infrastructure scripts -type f \
            \( -name '*.yml' -o -name '*.yaml' -o -name '*.conf' \
               -o -name '*.cfg' -o -name '*.ini' -o -name '*.properties' \
               -o -name '*.json' -o -name '*.toml' -o -name '*.env' \
               -o -name '*.service' -o -name '*.timer' -o -name '*.j2' \
               -o -name 'Dockerfile*' -o -name '*.hcl' \) 2>/dev/null
        find . -maxdepth 2 -type f \( -name 'docker-compose*.yml' \
            -o -name 'docker-compose*.yaml' -o -name '.env*' \
            -o -name '*.env' \) 2>/dev/null
        find services -maxdepth 2 -name 'config*.yaml' -o -name 'config*.yml' 2>/dev/null
    } | grep -v -E '(\.git/|node_modules/|/build|fault-report\.json)' | sort -u
}

KEY_RE='(password|passwd|pwd|secret[_-]?key|secretkey|api[_-]?key|apikey|access[_-]?key|private[_-]?key|encryption[_-]?key|master[_-]?key|hmac[_-]?key|session[_-]?key|jwt[_-]?key|auth[_-]?token|api[_-]?token|access[_-]?token|refresh[_-]?token|bearer|credential|dsn|client[_-]?secret|app[_-]?secret|api[_-]?secret|signing[_-]?key|data[_-]?key|webhook[_-]?secret|service[_-]?key|private[_-]?token)'

while IFS= read -r f; do
    [ -f "$f" ] || continue
    # binary files out — `grep -Iq .` is false for binary content
    # (and empty files, which have nothing to scan anyway). NOTE: a
    # literal NUL cannot be passed as a grep pattern in bash — the
    # naive `$'\x00'` collapses to an empty pattern and matches
    # EVERYTHING, silently skipping the whole file set.
    if ! grep -qI . "$f" 2>/dev/null; then continue; fi

    # Rule 3 — PEM private keys (whole-file scan).
    if grep -nE -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' "$f" >/dev/null 2>&1; then
        while IFS=: read -r ln _; do
            report "$f" "$ln" "PEM private-key block"
        done < <(grep -nE -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' "$f")
    fi

    # Rule 4 — AWS access key id / JWT-shaped tokens.
    while IFS=: read -r ln line; do
        report "$f" "$ln" "AWS-key-shaped token: ${line:0:120}"
    done < <(grep -nE 'AKIA[0-9A-Z]{16}' "$f" || true)
    while IFS=: read -r ln line; do
        report "$f" "$ln" "JWT-shaped token: ${line:0:120}"
    done < <(grep -nE 'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}' "$f" || true)

    # Rule 2 — credentialed URLs (scheme://user:pass@host).
    while IFS=: read -r ln line; do
        # extract the password segment between the first ':' after '//'
        # and the '@'
        cred="$(printf '%s' "$line" | grep -oE '[a-zA-Z][a-zA-Z0-9+.-]*://[^/[:space:]"'"'"']+:[^@/[:space:]"'"'"']+@' | head -1 || true)"
        [ -z "$cred" ] && continue
        pw="${cred##*:}"; pw="${pw%@}"
        if ! is_placeholder "$pw"; then
            report "$f" "$ln" "URL-embedded credential: ${cred%%://*}://…:***@…"
        fi
    done < <(grep -nE '[a-zA-Z][a-zA-Z0-9+.-]*://[^/[:space:]]+:[^@/[:space:]]+@' "$f" || true)

    # Rule 1 — key:value / key=value credential literals.
    while IFS= read -r match; do
        ln="${match%%:*}"
        line="${match#*:}"
        # split on the first : or = — key is the left token, val the
        # remainder minus comments/trailing punctuation.
        key="$(printf '%s' "$line" | sed -E 's/^[[:space:]-]*//; s/[:=].*$//' | xargs 2>/dev/null || true)"
        val="$(printf '%s' "$line" | sed -E "s/^[^:=]*[:=][[:space:]]*//; s/[[:space:]]*#.*$//; s/[,;].*$//" | xargs 2>/dev/null || true)"
        [ ${#val} -lt 8 ] && continue
        # Reference-style keys name a secret, they don't carry one:
        # K8s `secretKey`/`secretName`, `*_file`/`*_path` pointers, and
        # the ExternalSecret `property` field are identifiers — the
        # VALUE `data-key` in `secretKey: data-key` is a name, not a
        # credential. Naming metadata is public by design.
        case "$key" in
            secretKey|secretName|*KeyRef|*keyRef|*key_name|*File|*file|*Path|*path|*Name|*name|property|field|role|mount|*Ref|*Dir|*dir|*Id|*id)
                continue ;;
        esac
        # values containing spaces are prose, not literals
        case "$val" in *[[:space:]]*) continue ;; esac
        if ! is_placeholder "$val"; then
            report "$f" "$ln" "credential literal: ${line:0:100}"
        fi
    done < <(grep -nEi "$KEY_RE[[:space:]]*[:=][[:space:]]*[^[:space:]]+" "$f" || true)
done < <(collect_files)

if [ "$FINDINGS" -gt 0 ]; then
    printf 'no-plaintext-secrets: %d finding(s) — deploy surfaces must not carry secret literals (Vault/KMS only; spec §19.6, Task 13.5.3.6)\n' "$FINDINGS" >&2
    exit 1
fi
echo "no-plaintext-secrets: clean ($(collect_files | wc -l) files scanned)"
