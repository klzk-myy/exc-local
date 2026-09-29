#!/usr/bin/env bash
# gen_sbom.sh — Phase-13.5 Task 13.5.3.9: SBOM generation for the
# exchange build (spec §19.2, VDP severity contract — every disclosure
# links to the SBOM generation it was verified against via sbom_ref).
#
# Produces a CycloneDX-shaped JSON document at reports/sbom/:
#   sbom-<UTC date>-<short sha>.json   (the artifact; sbom_ref value)
#   latest.json                        (convenience copy)
#
# Contents are honest — built from first-party metadata, not fabricated:
#   - Go modules:    `go list -m -json all` in services/ (every module,
#                    version, upstream hash when present)
#   - C++ deps:      parsed from core/ vendored/FetchContent pins
#                    (core/CMake* FetchContent_Declares declarations)
#   - Toolchain:     go version + git describe of the source tree
#
# No external SBOM tool is required. If cyclonedx-gomod or syft is
# present on PATH it is preferred and this script delegates — the
# fallback generator below is the zero-dependency baseline.
#
# Env: OUT_DIR (default reports/sbom), SERVICES_DIR (default services),
#      CORE_DIR (default core).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

OUT_DIR="${OUT_DIR:-reports/sbom}"
SERVICES_DIR="${SERVICES_DIR:-services}"
CORE_DIR="${CORE_DIR:-core}"
mkdir -p "$OUT_DIR"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
SHA="$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
OUT="$OUT_DIR/sbom-$STAMP-$SHA.json"

# Prefer real tooling when present; the output path convention is the
# contract either way (sbom_ref points at reports/sbom/<file>).
if command -v cyclonedx-gomod >/dev/null 2>&1; then
    echo "gen_sbom: using cyclonedx-gomod"
    (cd "$SERVICES_DIR" && cyclonedx-gomod mod -json -output "/dev/stdout") \
        > "$OUT"
elif command -v syft >/dev/null 2>&1; then
    echo "gen_sbom: using syft"
    syft dir:"$ROOT" -o cyclonedx-json > "$OUT"
else
    echo "gen_sbom: no SBOM tool found — generating baseline manifest"
    MODS="$(cd "$SERVICES_DIR" && go list -m -json all)"
    GOVER="$(cd "$SERVICES_DIR" && go version | awk '{print $3}')"
    # C++ FetchContent/vendored pins — scrape declarations honestly;
    # absent or unparsed deps are omitted, never invented.
    CXX_DEPS="$(grep -rhoE 'FetchContent_Declares\([[:space:]]*[A-Za-z0-9_]+' "$CORE_DIR" 2>/dev/null \
        | sed -E 's/FetchContent_Declares\([[:space:]]*//' | sort -u || true)"
    export MODS CXX_DEPS STAMP
    python3 - "$OUT" "$GOVER" "$SHA" <<'PYEOF'
import json, os, re, sys

out_path, gover, sha = sys.argv[1], sys.argv[2], sys.argv[3]
mods_raw = os.environ.get("MODS", "")
cxx_raw = os.environ.get("CXX_DEPS", "")

components = []
# go list -m -json all emits a stream of JSON objects (not an array).
dec = json.JSONDecoder()
buf = mods_raw.strip()
i = 0
while i < len(buf):
    while i < len(buf) and buf[i] not in '{[':
        i += 1
    if i >= len(buf):
        break
    obj, j = dec.raw_decode(buf, i)
    i = j
    if not isinstance(obj, dict) or "Path" not in obj:
        continue
    ver = obj.get("Version", "")
    purl = "pkg:golang/%s@%s" % (obj["Path"], ver) if ver \
        else "pkg:golang/%s" % obj["Path"]
    components.append({
        "type": "library",
        "bom-ref": purl,
        "name": obj["Path"],
        "version": ver or "workspace",
        "purl": purl,
        "scope": "required" if not obj.get("Indirect") else "optional",
        "properties": [{"name": "go:module_hash",
                        "value": (obj.get("GoModSum") or "")}],
    })

for dep in sorted(set(d for d in cxx_raw.splitlines() if d.strip())):
    name = dep.strip()
    components.append({
        "type": "library",
        "bom-ref": "cxx:%s" % name,
        "name": name,
        "version": "pinned-in-source",
        "scope": "required",
        "properties": [{"name": "source", "value": "core FetchContent"}],
    })

doc = {
    "bomFormat": "CycloneDX",
    "specVersion": "1.5",
    "serialNumber": "urn:uuid:sbom-%s-%s" % (os.environ.get("STAMP", "0"), sha),
    "version": 1,
    "metadata": {
        "timestamp": os.environ.get("STAMP", ""),
        "tools": [{"vendor": "exchange", "name": "gen_sbom.sh", "version": "baseline"}],
        "component": {"type": "application", "bom-ref": "exchange",
                      "name": "exchange", "version": sha},
        "properties": [{"name": "toolchain:go", "value": gover}],
    },
    "components": components,
}
with open(out_path, "w") as f:
    json.dump(doc, f, indent=2)
print("gen_sbom: %d components -> %s" % (len(components), out_path))
PYEOF
fi

cp -f "$OUT" "$OUT_DIR/latest.json"
echo "gen_sbom: latest -> $OUT_DIR/latest.json"
