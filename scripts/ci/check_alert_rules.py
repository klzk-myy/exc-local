#!/usr/bin/env python3
"""Validate the Prometheus alert pack (Phase-13 Task 13.3.3 gate).

Checks, in order:
  1. prometheus.yml + every file in rule_files parses as YAML.
  2. Total alert-rule count across loaded files >= 47.
  3. All 12 Task 13.3.3 domains covered (via `domain:` labels in
     alerts.yml plus the sentinel/capacity packs' coverage mapping).
  4. Every alert has a `severity` label (p0-p3) and a `runbook`
     annotation resolving to a repo file; `#anchor` fragments must
     match a markdown heading (GitHub slug rules).
  5. alertmanager.yml has a route + receiver for every severity used.
  6. PromQL sanity: balanced delimiters, no stray tabs/CRs.

Usage: python3 scripts/ci/check_alert_rules.py  (run from repo root)
Exit 0 on pass, 1 on any failure.
"""
import os
import re
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
PROM_DIR = os.path.join(ROOT, "deploy", "prometheus")

DOMAINS = {
    "latency", "throughput", "queue-depth", "wal-lag", "memory", "cpu",
    "degradation", "circuit-breaker", "reconciliation", "dr",
    "settlement", "funding",
}

# Rules in the Phase-07/09 files predating the `domain:` label — mapped
# by alertname so the domain-coverage check reflects what is loaded,
# not just what alerts.yml carries.
LEGACY_DOMAIN = {
    "L0ErrorObserved": "degradation",          # error-tier/halt domain
    "L1ErrorsSustained": "degradation",
    "L2RejectionSpike": "throughput",
    "ErrorRateAnomaly": "throughput",
    "AlertDispatchErrors": "dr",
    "AeronSubscriberLag": "queue-depth",
    "AeronDriverDown": "dr",
    "BridgeBufferDepthHigh": "queue-depth",
    "BridgeHeartbeatStale": "dr",
    "BridgeSubscriptionDisconnected": "dr",
    "NATSConsumerPendingHigh": "queue-depth",
    "DLQEntriesAccumulating": "queue-depth",
    "AvailabilityBurnFast": "latency",
    "AvailabilityBurnSlow": "latency",
    "DegradationModeActive": "degradation",
    "CircuitBreakerOpen": "circuit-breaker",
    "ReconciliationMismatch": "reconciliation",
    "WALLagGrowing": "wal-lag",
    "IPCRingSaturated": "queue-depth",
    "IPCRingCritical": "queue-depth",
    "PTPClockOffsetExceeded": "dr",
    "PTPNotSynchronized": "dr",
    "PTPStale": "dr",
    "PTPUnavailable": "dr",
    "EdgeDeniesSustained": "throughput",
    "WAFBlocksSurge": "throughput",
    "WAFChallengesSustained": "throughput",
    "EdgeDDoSSuspected": "throughput",
    # monitoring packs
    "CapacityCPUHeadroom": "cpu",
    "CapacityMemoryHeadroom": "memory",
    "CapacityDiskHeadroom": "memory",
    "CapacityNetworkHeadroom": "throughput",
    "HotTierGrowthAnomaly": "memory",
    "RetentionPolicyViolation": "reconciliation",
    "SentinelQuorumLost": "dr",
    "SentinelNodeDown": "dr",
    "RedisMasterDown": "dr",
    "RedisReplicationLag": "dr",
    "RedisReplicationRPORisk": "dr",
    "SentinelFailoverInProgress": "dr",
    "SentinelNoReplicas": "dr",
}


def gh_anchor(heading):
    """GitHub-flavored markdown anchor for a heading line."""
    s = heading.strip().lower()
    s = re.sub(r"[^\w\- ]", "", s)
    return s.replace(" ", "-")


def collect_anchors(md_path):
    anchors = set()
    for line in open(md_path, encoding="utf-8"):
        m = re.match(r"^#{1,6}\s+(.*)$", line)
        if m:
            anchors.add(gh_anchor(m.group(1)))
    return anchors


def main():
    failures = []

    prom = yaml.safe_load(open(os.path.join(PROM_DIR, "prometheus.yml")))
    rule_files = prom.get("rule_files", [])
    if not rule_files:
        failures.append("prometheus.yml: no rule_files")

    alerts = []          # (file, alertname, labels, annotations, expr)
    for rf in rule_files:
        path = os.path.normpath(os.path.join(PROM_DIR, rf))
        if not os.path.isfile(path):
            failures.append(f"rule_file missing: {rf}")
            continue
        try:
            doc = yaml.safe_load(open(path))
        except yaml.YAMLError as e:
            failures.append(f"{rf}: YAML parse error: {e}")
            continue
        for grp in doc.get("groups", []):
            for rule in grp.get("rules", []):
                if "alert" in rule:
                    alerts.append((rf, rule["alert"],
                                   rule.get("labels", {}),
                                   rule.get("annotations", {}),
                                   rule.get("expr", "")))

    # 2. count
    if len(alerts) < 47:
        failures.append(f"only {len(alerts)} alert rules (<47)")

    # 3. domain coverage
    covered = set()
    per_domain = {}
    for rf, name, labels, _ann, _expr in alerts:
        dom = labels.get("domain") or LEGACY_DOMAIN.get(name)
        if dom:
            covered.add(dom)
            per_domain[dom] = per_domain.get(dom, 0) + 1
        else:
            per_domain.setdefault("(unmapped)", []).append(name)
    missing = DOMAINS - covered
    if missing:
        failures.append(f"domains not covered: {sorted(missing)}")

    # 4. severity + runbook
    anchor_cache = {}
    for rf, name, labels, ann, expr in alerts:
        sev = labels.get("severity")
        if sev not in ("p0", "p1", "p2", "p3"):
            failures.append(f"{rf}:{name}: missing/bad severity {sev!r}")
        rb = ann.get("runbook")
        if not rb:
            failures.append(f"{rf}:{name}: no runbook annotation")
        else:
            fpath, _, frag = rb.partition("#")
            md = os.path.join(ROOT, fpath)
            if not os.path.isfile(md):
                failures.append(f"{rf}:{name}: runbook file missing {fpath}")
            elif frag:
                if md not in anchor_cache:
                    anchor_cache[md] = collect_anchors(md)
                if frag not in anchor_cache[md]:
                    failures.append(
                        f"{rf}:{name}: runbook anchor #{frag} not in {fpath}")
        if not ann.get("summary"):
            failures.append(f"{rf}:{name}: no summary annotation")
        # 6. expr sanity
        if not isinstance(expr, str) or not expr.strip():
            failures.append(f"{rf}:{name}: empty expr")
        else:
            for a, b in (("(", ")"), ("[", "]"), ("{", "}")):
                if expr.count(a) != expr.count(b):
                    failures.append(f"{rf}:{name}: unbalanced '{a}{b}'")
            if "\t" in expr or "\r" in expr:
                failures.append(f"{rf}:{name}: tab/CR in expr")

    # 5. alertmanager routing
    am = yaml.safe_load(open(os.path.join(PROM_DIR, "alertmanager.yml")))
    receivers = {r["name"] for r in am.get("receivers", [])}
    routes = am.get("route", {})
    routed = {}

    def walk(node):
        for k, v in (node.get("match") or {}).items():
            if k == "severity":
                routed[v] = node.get("receiver")
        for k, v in (node.get("match_re") or {}).items():
            if k == "severity":
                routed[v] = node.get("receiver")
        for child in node.get("routes", []):
            walk(child)

    walk(routes)
    used_sevs = {l.get("severity") for _f, _n, l, _a, _e in alerts}
    for sev in sorted(s for s in used_sevs if s):
        recv = routed.get(sev)
        if not recv:
            failures.append(f"alertmanager: no route for severity={sev}")
        elif recv not in receivers:
            failures.append(
                f"alertmanager: severity={sev} routes to undefined "
                f"receiver '{recv}'")
        else:
            has_pd = any(
                "pagerduty_configs" in r or "pagerduty_config" in r
                for r in am["receivers"] if r["name"] == recv)
            if not has_pd:
                failures.append(
                    f"alertmanager: receiver '{recv}' for {sev} has no "
                    "pagerduty_configs")

    # report
    print(f"rule files: {len(rule_files)}  alerts: {len(alerts)}")
    print("domain coverage:")
    for d in sorted(DOMAINS):
        print(f"  {d:16s} {per_domain.get(d, 0)}")
    unm = per_domain.get("(unmapped)")
    if unm:
        print(f"  unmapped: {unm}")
    print(f"severities routed: {sorted(routed.items())}")
    if failures:
        print("\nFAILURES:")
        for f in failures:
            print("  -", f)
        return 1
    print("\nOK: all checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
