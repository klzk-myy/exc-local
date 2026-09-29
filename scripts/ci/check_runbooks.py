#!/usr/bin/env python3
"""Validate the on-call runbook corpus (Phase-13.5 Task 13.5.3.4).

Corpus = the union of:

  1. docs/runbooks/*.md (README.md excluded — it is the index);
  2. every distinct file referenced by a `runbook:` annotation in the
     deployed alert packs (deploy/prometheus/*.yml + alertmanager);
  3. named operational runbooks outside docs/runbooks — ops procedures
     (docs/ops), the ClickHouse backup/DR runbook, the edge DDoS
     playbook, the GDPR erasure runbook.

Classified by use:

  alert-runbook   referenced by a deployed alert's runbook annotation —
                  must carry the README contract sections:
                  Symptom|Trigger, Diagnosis|Detection,
                  Mitigation|Remediation, Escalation.
  conditional     docs/runbooks/* not alert-linked (BCP modes etc.) and
                  ops/security runbooks — must still carry a trigger-ish,
                  an action-ish and an escalation-ish section.
  program/index   README.md, incident-escalation.md, dr-drill.md —
                  listed in the matrix, exempt from the section gate and
                  the >=47 count.

A fourth column reports Rollback/Recovery coverage as a WARN signal —
the README contract does not mandate it, but the Phase-13.5 drill brief
asks for it, so the matrix surfaces which runbooks carry a
rollback/recovery/revert path.

Exit 0 on pass, 1 on failure (missing required sections, broken alert
runbook links, or corpus < 47).
"""
import os
import re
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

MIN_RUNBOOKS = 47

# Files that are indexes/programs, not response runbooks.
PROGRAM_DOCS = {
    "docs/runbooks/README.md",
    "docs/runbooks/incident-escalation.md",
    "docs/runbooks/dr-drill.md",
}

# Operational runbooks living outside docs/runbooks (inclusion is
# explicit so a renamed/deleted file is a loud failure, not silent
# shrinkage of the corpus).
EXTRA_RUNBOOKS = [
    "docs/ops/dr.md",
    "docs/ops/redis-sentinel-failover.md",
    "docs/ops/daemon-supervision.md",
    "docs/ops/shard-binary-swap.md",
    "docs/ops/blue-green-deploy.md",
    "docs/ops/baremetal-provisioning.md",
    "docs/ops/dora-incident-reporting.md",
    "docs/security/gdpr-erasure-runbook.md",
    "deploy/clickhouse/RUNBOOK.md",
    "deploy/edge/ddos-playbook.md",
]

HEAD_RE = re.compile(r"^#{1,4}\s+(.+?)\s*$", re.M)
# Bold-label pseudo-headings — `**Entry conditions:**`, `**Exit:**` —
# carry the same contract weight in conditional runbooks.
BOLD_RE = re.compile(r"^\s*\*\*([^*]{3,60})\*\*[:.]?\s*$", re.M)

TRIGGER_RE = re.compile(
    r"symptom|trigger|entry condition|when to use|triage|scenario|"
    r"failure mode|detection|signal|intake|classification", re.I)
ACTION_RE = re.compile(
    r"mitigation|remediation|response|resolution|procedure|step|"
    r"restore|failover|rollback|recover|mode|action|fix|contain|"
    r"demotion|override|workflow|enforcement", re.I)
ESCALATION_RE = re.compile(
    r"escalat|post[- ]incident|handoff|on[- ]call|page|exit condition|"
    r"report|notify|audit", re.I)
DIAGNOSIS_RE = re.compile(r"diagnos|detec|investigat|identify|classif", re.I)
ROLLBACK_RE = re.compile(
    r"rollback|revert|recovery|restore|undo|back[- ]?out|exit condition|"
    r"stand[- ]?down|demotion", re.I)

# Inline-evidence fallbacks for conditional runbooks (the README
# heading contract governs alert-linked runbooks; ops procedure docs
# legitimately carry paging/escalation inside tables and prose).
INLINE_TRIGGER_RE = re.compile(
    r"entry condition|when to use|triggered|triage|intake|severity bar|"
    r"failure scenario", re.I)
INLINE_ESCALATION_RE = re.compile(
    r"escalat|→\s*page|\bpage\b.*(p[0-3]|ops|risk)|on[- ]?call|"
    r"incident ticket|notify", re.I)
INLINE_ACTION_RE = re.compile(
    r"procedure:|rollback procedure|restore steps|runbook", re.I)


def headings(path):
    with open(path, encoding="utf-8") as f:
        body = f.read()
    return HEAD_RE.findall(body) + BOLD_RE.findall(body), body


def has(heads, rx):
    return any(rx.search(h) for h in heads)


def alert_runbook_refs():
    """Distinct runbook annotation targets from deployed alert packs."""
    refs = set()
    dirs = [os.path.join(ROOT, "deploy", "prometheus"),
            os.path.join(ROOT, "deploy", "prometheus", "rules"),
            os.path.join(ROOT, "deploy", "monitoring"),
            os.path.join(ROOT, "deploy", "alertmanager")]
    for d in dirs:
        if not os.path.isdir(d):
            continue
        for fn in os.listdir(d):
            if not fn.endswith((".yml", ".yaml")):
                continue
            with open(os.path.join(d, fn), encoding="utf-8") as f:
                doc = yaml.safe_load(f)
            for target in walk_annotations(doc):
                tgt = str(target).split("#", 1)[0].strip()
                if tgt and (tgt.endswith(".md") or tgt.endswith(".sh")):
                    refs.add(tgt)
    return refs


def walk_annotations(node):
    if isinstance(node, dict):
        for k, v in node.items():
            if k == "runbook":
                yield v
            else:
                yield from walk_annotations(v)
    elif isinstance(node, list):
        for item in node:
            yield from walk_annotations(item)


def main():
    failures = []
    warnings = []
    matrix = []

    # --- corpus enumeration -------------------------------------------------
    corpus = set()
    rb_dir = os.path.join(ROOT, "docs", "runbooks")
    for fn in sorted(os.listdir(rb_dir)):
        if fn.endswith(".md"):
            corpus.add("docs/runbooks/" + fn)

    refs = alert_runbook_refs()
    corpus |= refs
    corpus |= set(EXTRA_RUNBOOKS)
    corpus -= PROGRAM_DOCS

    # Missing-file check first — a dangling alert runbook link or a
    # vanished EXTRA_RUNBOOKS entry is a failure.
    for rel in sorted(corpus):
        if not os.path.isfile(os.path.join(ROOT, rel)):
            failures.append(f"{rel}: referenced runbook does not exist")
    corpus = {c for c in corpus if os.path.isfile(os.path.join(ROOT, c))}

    # --- per-file section validation ----------------------------------------
    covered = 0
    for rel in sorted(corpus):
        heads, body = headings(os.path.join(ROOT, rel))
        alert_linked = rel in refs and rel.startswith("docs/runbooks/")
        kind = "alert-runbook" if alert_linked else "conditional"

        trig = has(heads, TRIGGER_RE)
        diag = has(heads, DIAGNOSIS_RE)
        mit = has(heads, ACTION_RE)
        esc = has(heads, ESCALATION_RE)
        rb = has(heads, ROLLBACK_RE)
        # Conditional runbooks additionally accept inline contract
        # evidence (escalation pointers inside tables/prose, entry
        # conditions as bold labels) — marked 'i' in the matrix.
        if not alert_linked:
            if not trig:
                trig = bool(INLINE_TRIGGER_RE.search(body))
            if not mit:
                mit = bool(INLINE_ACTION_RE.search(body))
            if not esc:
                esc = bool(INLINE_ESCALATION_RE.search(body))
        if not rb:
            rb = bool(ROLLBACK_RE.search(body))

        missing = []
        if not trig:
            missing.append("TRIGGER/SYMPTOM")
        if not mit:
            missing.append("MITIGATION")
        if not esc:
            missing.append("ESCALATION")
        if alert_linked and not diag:
            missing.append("DIAGNOSIS(alert contract)")

        status = "PASS" if not missing else "FAIL"
        if not missing:
            covered += 1
        else:
            failures.append(f"{rel}: missing {', '.join(missing)}")
        if not rb:
            warnings.append(f"{rel}: no rollback/recovery section")
        matrix.append((rel, kind, status,
                       "Y" if trig else "-", "Y" if diag else "-",
                       "Y" if mit else "-", "Y" if esc else "-",
                       "Y" if rb else "-"))

    # --- corpus size ---------------------------------------------------------
    if covered < MIN_RUNBOOKS:
        failures.append(f"runbook corpus too small: {covered} conforming "
                        f"(need >= {MIN_RUNBOOKS})")

    # --- report ---------------------------------------------------------------
    print("TASK 13.5.3.4 — runbook coverage matrix")
    print(f"{'file':<58} {'kind':<14} {'st':<4} {'T':<2} {'D':<2} "
          f"{'M':<2} {'E':<2} {'R':<2}")
    for rel, kind, status, t, d, m, e, rb in matrix:
        print(f"{rel:<58} {kind:<14} {status:<4} {t:<2} {d:<2} "
              f"{m:<2} {e:<2} {rb:<2}")
    for rel in sorted(PROGRAM_DOCS):
        print(f"{rel:<58} {'program/index':<14} n/a")
    print()
    print(f"runbooks checked={len(matrix)} conforming={covered} "
          f"min_required={MIN_RUNBOOKS}")
    print(f"alert-referenced files={len(refs)}")
    for w in warnings:
        print(f"WARN: {w}")
    for f_ in failures:
        print(f"FAIL: {f_}")
    print("check_runbooks:", "OK" if not failures else "FAILED")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
