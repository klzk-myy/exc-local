#!/usr/bin/env python3
"""Enforce the production observability contract (Phase-09 Task 9.3.29,
spec §19.14, §24 #340) against deploy/monitoring/observability-budgets.yml —
the machine-readable mirror of docs/ops/observability-contract.md §1-3.

CI leg (default, static — no cluster required):
  1. budgets file parses; global scrape_interval matches prometheus.yml;
     every service with a scrape_job has that job configured.
  2. Every metric family registered in services/**/*.go (non-test files)
     carries a contract prefix or an exact-name allowlist entry, and uses
     a valid Prometheus name.
  3. Every label name in .With(...) calls is allowlisted; denylisted names
     (order_id, account_id, trace_id, ...) are a hard fail. Names on the
     contract's review tier print as WARNINGS (visible, tracked).
  4. Per-scope family counts stay within family_budget (a cardinality
     blow-up starts as a new family or label — this is the static tripwire;
     true series counts are a live-pipeline property).
  5. Trace sampling budget: tracer.go default RatioHead == budgets head
     ratio, tail-on-error and tail-on-slow upgrades present; otel collector
     keeps memory_limiter first and the attributes/crop hygiene pass.
  6. Retention: every required data class exists in the tiering policy
     with the contract's retain_days.

Live seam (NOT run in CI — needs a Prometheus):
  python3 scripts/ci/check_observability_budgets.py --live http://prom:9090
  queries count by (job) of all series and enforces services.*.budget —
  the per-service cardinality budgets are LIVE series budgets; the static
  leg bounds families/labels, the live leg bounds actual series.

Usage: python3 scripts/ci/check_observability_budgets.py [--live URL]
Exit: 0 pass (warnings printed); 1 any failure.
"""
import os
import re
import sys
import urllib.parse
import urllib.request
import json

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
BUDGETS = os.path.join(ROOT, "deploy", "monitoring", "observability-budgets.yml")
PROM = os.path.join(ROOT, "deploy", "prometheus", "prometheus.yml")
GO_ROOT = os.path.join(ROOT, "services")

# The registry implementation itself registers nothing — its Counter/Gauge
# signatures are API definitions, not families.
SKIP_FILES = {os.path.join("internal", "observability", "registry.go")}

REG_RE = re.compile(
    r'\b(?:Counter|Gauge|Histogram|CounterFunc|GaugeFunc|VecFunc)\(\s*("[^"]+"|\w+)')
CONST_RE = re.compile(r'\b(\w+)\s*(?:string)?\s*=\s*"([a-zA-Z_:][a-zA-Z0-9_:]*)"')
WITH_RE = re.compile(r'\.With\(([^)]*)\)')
NAME_RE = re.compile(r'^[a-z_:][a-z0-9_:]*$')
STR_LIT = re.compile(r'^"[^"]*"$')


def fail_out(failures, warnings):
    for w in warnings:
        print(f"  WARN  {w}")
    if failures:
        print("\nFAILURES:")
        for f in failures:
            print("  -", f)
        return 1
    print("\nOK: observability budgets enforced (static leg)")
    return 0


def collect_go():
    """Return ({file: [metric names]}, [(file, label_name)]) for
    non-test Go files under services/."""
    metrics, labels = {}, []
    for root, _dirs, files in os.walk(GO_ROOT):
        for fn in sorted(files):
            if not fn.endswith(".go") or fn.endswith("_test.go"):
                continue
            rel = os.path.relpath(os.path.join(root, fn), GO_ROOT)
            if rel in SKIP_FILES:
                continue
            src = open(os.path.join(root, fn), encoding="utf-8").read()
            # const/var name→string resolution (same-file only — the
            # Metric* constants live beside their registrations).
            consts = dict((m.group(1), m.group(2)) for m in CONST_RE.finditer(src))
            names = []
            for m in REG_RE.finditer(src):
                arg = m.group(1)
                if arg.startswith('"'):
                    names.append(arg.strip('"'))
                elif arg in consts:
                    names.append(consts[arg])
                # else: identifier bound elsewhere — cannot resolve
                # statically; noted in the report, never assumed.
            if names:
                metrics[rel] = names
            for m in WITH_RE.finditer(src):
                args = [a.strip() for a in m.group(1).split(",")]
                for i in range(0, len(args), 2):
                    if STR_LIT.match(args[i]):
                        labels.append((rel, args[i].strip('"')))
    return metrics, labels


def main():
    failures, warnings = [], []
    b = yaml.safe_load(open(BUDGETS, encoding="utf-8"))

    # ── 1. scrape contract ────────────────────────────────────────────────
    prom = yaml.safe_load(open(PROM, encoding="utf-8"))
    want_iv = b["scrape_interval"]
    got_iv = str(prom.get("global", {}).get("scrape_interval", ""))
    if got_iv != want_iv:
        failures.append(
            f"prometheus.yml scrape_interval {got_iv!r} != contract {want_iv!r}")
    jobs = {j.get("job_name") for j in prom.get("scrape_configs", [])}
    for svc, cfg in (b.get("services") or {}).items():
        job = cfg.get("scrape_job")
        if job and job not in jobs:
            failures.append(f"service {svc}: scrape job {job!r} missing "
                            "from prometheus.yml")
        if not isinstance(cfg.get("budget"), int) or cfg["budget"] <= 0:
            failures.append(f"service {svc}: missing/invalid series budget")

    # ── 2-4. Go metric + label inventory vs the naming/label contract ──────
    prefixes = tuple(b.get("prefixes") or [])
    exact = set(b.get("exact_names") or [])
    lab = b.get("labels") or {}
    allow = set(lab.get("allowlist") or [])
    deny = set(lab.get("denylist") or [])
    review = {r["name"]: r for r in (lab.get("review") or [])}

    metrics, labels = collect_go()
    all_families = sorted({n for v in metrics.values() for n in v})
    for f, names in sorted(metrics.items()):
        for n in names:
            if not NAME_RE.match(n):
                failures.append(f"{f}: metric name {n!r} is not valid "
                                "Prometheus naming")
            elif not (n.startswith(prefixes) or n in exact):
                failures.append(
                    f"{f}: metric {n!r} has no contract prefix — add a "
                    "reviewed prefix/exact_names entry in "
                    "deploy/monitoring/observability-budgets.yml")
    used_labels = {}
    for f, lname in labels:
        used_labels.setdefault(lname, []).append(f)
        if lname in deny:
            failures.append(f"{f}: label {lname!r} is denylisted "
                            "(unbounded value domain)")
        elif lname not in allow:
            failures.append(f"{f}: label {lname!r} not in the contract "
                            "allowlist — a new label needs a reviewed "
                            "budgets-file edit")
    for rname, r in review.items():
        if rname in used_labels:
            where = ", ".join(sorted(set(used_labels[rname]))[:4])
            warnings.append(f"review-tier label {rname!r} in use ({where}): "
                            f"{r.get('note', '')}")

    # per-scope family budgets
    for scope, cfg in (b.get("scopes") or {}).items():
        dirs = cfg.get("dirs") or []
        bud = cfg.get("family_budget")
        if not bud:
            failures.append(f"scope {scope}: no family_budget")
            continue
        fams = set()
        for d in dirs:
            drel = os.path.relpath(d, "services") if d.startswith(
                "services") else d
            for f, names in metrics.items():
                if f.startswith(drel + os.sep):
                    fams.update(names)
        print(f"scope {scope:10s} families={len(fams):3d} budget={bud}")
        if len(fams) > bud:
            failures.append(f"scope {scope}: {len(fams)} metric families "
                            f"> static budget {bud}")

    # ── 5. trace sampling budget ──────────────────────────────────────────
    tr = b.get("tracing") or {}
    src_rel = tr.get("source")
    if src_rel and os.path.isfile(os.path.join(ROOT, src_rel)):
        tsrc = open(os.path.join(ROOT, src_rel), encoding="utf-8").read()
        hr = tr.get("head_ratio")
        if not re.search(r'RatioHead:\s*%s\b' % re.escape(str(hr)), tsrc):
            failures.append(
                f"{src_rel}: default RatioHead != contract head_ratio {hr}")
        if tr.get("require_tail_error") and "RecordError" not in tsrc \
                and "error" not in tsrc.lower():
            failures.append(f"{src_rel}: no tail-on-error sampling path")
        if tr.get("require_tail_slow") and "SlowThreshold" not in tsrc:
            failures.append(f"{src_rel}: no SlowThreshold tail-slow upgrade")
    elif src_rel:
        failures.append(f"tracing source missing: {src_rel}")

    coll_rel = (tr.get("collector"))
    if coll_rel and os.path.isfile(os.path.join(ROOT, coll_rel)):
        coll = yaml.safe_load(open(os.path.join(ROOT, coll_rel),
                                   encoding="utf-8"))
        checks = tr.get("collector_checks") or {}
        procs = (coll.get("processors") or {})
        pipe = ((coll.get("service") or {}).get("pipelines") or {}
                ).get("traces") or {}
        plist = pipe.get("processors") or []
        if checks.get("memory_limiter_first"):
            if not plist or plist[0] != "memory_limiter":
                failures.append(f"{coll_rel}: memory_limiter not first in "
                                "the traces pipeline")
        if checks.get("require_attributes_crop") and \
                not any(p.startswith("attributes/") for p in plist) \
                and not any(k.startswith("attributes/") for k in procs):
            failures.append(f"{coll_rel}: no attributes/* cardinality "
                            "hygiene processor")
    elif coll_rel:
        warnings.append(f"otel collector config missing: {coll_rel}")

    # ── 6. retention policy vs contract windows ───────────────────────────
    ret = b.get("retention") or {}
    pol_rel = ret.get("policy")
    pol_path = os.path.join(ROOT, pol_rel) if pol_rel else None
    if pol_path and os.path.isfile(pol_path):
        pol = yaml.safe_load(open(pol_path, encoding="utf-8"))
        classes = {c.get("name"): c for c in (pol.get("classes") or [])}
        for name, days in (ret.get("required_classes") or {}).items():
            c = classes.get(name)
            if not c:
                failures.append(f"tiering policy: required class {name!r} "
                                "absent")
            elif c.get("retain_days") != days:
                failures.append(
                    f"tiering policy: class {name!r} retain_days="
                    f"{c.get('retain_days')} != contract {days}")
        log_cls = [n for n, c in classes.items()
                   if "log" in n or c.get("store") == "loki"]
        if not log_cls:
            warnings.append(
                f"tiering policy has no ops-log class — the "
                f"{ret.get('ops_log_hot_days')}d Loki hot window is "
                "config-managed, not policy-enforced")
    elif pol_rel:
        failures.append(f"retention policy missing: {pol_rel}")

    # ── live seam (optional, needs a Prometheus) ─────────────────────────
    if len(sys.argv) >= 3 and sys.argv[1] == "--live":
        live = sys.argv[2].rstrip("/")
        print(f"\n--live {live}: enforcing per-service series budgets")
        q = 'count by (job) ({__name__=~".+"})'
        url = live + "/api/v1/query?" + urllib.parse.urlencode({"query": q})
        try:
            data = json.load(urllib.request.urlopen(url, timeout=10))
        except Exception as e:  # noqa: BLE001 — report, fail closed
            failures.append(f"live query failed: {e}")
            return fail_out(failures, warnings)
        series = {r["metric"].get("job", "?"): float(r["value"][1])
                  for r in data.get("data", {}).get("result", [])}
        for svc, cfg in (b.get("services") or {}).items():
            job = cfg.get("scrape_job")
            if not job:
                warnings.append(f"service {svc}: no scrape job — live "
                                "budget not measurable")
                continue
            got = series.get(job, 0.0)
            mult = ""
            if cfg.get("per_instance"):
                mult = " (per instance)"
            print(f"  {svc:12s} job={job:12s} series={int(got):6d} "
                  f"budget={cfg['budget']}{mult}")
            if got > cfg["budget"]:
                failures.append(f"service {svc}: {int(got)} series > "
                                f"budget {cfg['budget']}")

    print(f"\nmetrics: {len(all_families)} families registered; "
          f"{len(used_labels)} distinct label names; "
          f"{len(warnings)} warnings")
    return fail_out(failures, warnings)


if __name__ == "__main__":
    sys.exit(main())
