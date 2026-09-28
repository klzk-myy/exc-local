# Runbook: `SECRET_ROTATION_OVERDUE` — secret past rotation SLA

**Severity:** P2 (14-day alert bound per Task 9.3.29; HTTP 503 on dependent paths when the secret is required) · **Domain:** spec §19.14 secrets inventory — migration 089 `secrets_inventory` (**pending**) holds one row per secret: owner, TTL, rotation procedure, last-rotated; store of record is Vault/KMS (Phase-13.5 Task 13.5.3.5).

## Symptom

`SECRET_ROTATION_OVERDUE` raised for a secret past its rotation SLA — banking API keys, FIX mTLS certs, OAuth client secrets, KMS grants. At this stage the secret still works; the alert fires because the *process* failed, which is how leaks go unnoticed.

## Diagnosis

1. Identify the secret + owner: `secrets_inventory` row (pending migration 089 — until then, the inventory procedure is [../ops/secrets-inventory.md](../ops/secrets-inventory.md) and the source of truth is the Vault/KMS audit log).
2. Why rotation failed: scheduled job error vs procedure never written vs blocked on third party (bank-side key reissue, counterparty cert chain).
3. Exposure check: has the secret's TTL been exceeded by days or months? Cross-check Vault/KMS `last-rotated` against the alert timestamp. >30 days overdue escalates internally regardless of the 14-day P2 label.
4. DR coverage: every DR-critical secret (KMS grants, FIX mTLS certs, banking API keys) must carry a tested secondary-region copy — verify the DR copy rotated too, or the next DR drill will fail its decrypt-in-secondary gate (Task 9.3.29 item 5).

## Mitigation

1. Rotate now via the secret's recorded procedure (owner's runbook column in the inventory): re-issue at source → write to Vault/KMS → rolling reload of consumers (K8s secret re-mount / service restart; FIX mTLS cert swap needs the §19.6 deploy window for engine-adjacent consumers).
2. Verify consumers: `gateway` (:8080), `fix` (:8082), `bridge`, `settlement` read secrets at boot/resolution — confirm post-rotation auth still succeeds (FIX session logon, banking rail call, OAuth token mint).
3. Mark `last_rotated` + procedure outcome in the inventory row; alert auto-clears on next evaluator pass (alert evaluator/enforcer pending Task 9.3.29 — interim tracking is the inventory doc + Vault audit).
4. **Leak-triggered emergency rotation** (different clock — hours, not days): rotate immediately, revoke the old credential at the issuer, invalidate sessions/tokens minted under it, and open a security incident — break-glass procedure per Task 7.3.12 (break-glass path, pending Phase-07) with post-review mandatory.
5. Update the DR secondary copy and re-run the decrypt check.

## Escalation

- P2 → owning team + Security. Escalate to P1 if the overdue secret protects client money movement (banking API keys, settlement rail credentials) or is suspected compromised — then it is the emergency path above, not this runbook.
- Rotation chronically failing on the same secret → the *procedure* is the defect; file against the owner in the inventory.
