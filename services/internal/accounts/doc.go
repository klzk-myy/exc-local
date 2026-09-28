// Package accounts implements the Phase-05 account-state cluster:
//
//   - Task 5.3.11 — sub-account hierarchy & tiered ceilings
//     (subaccounts.go): master → sub-account tree (one level deep),
//     accounts.max_sub_accounts ceiling (migration 067: NULL = tier
//     default, T0/T1 → 20, T2 → 100, admin-set up to 1,000), aggregated
//     family views, and programmatic API-key provisioning scoped to a
//     sub-account (apikeys.go, api_keys table from migration 025).
//   - Task 5.3.12 — FROZEN legal-hold state (freeze.go): dual-control
//     freeze/unfreeze with the account_freeze_events audit trail
//     (migration 152) plus admin_audit_log rows. Frozen accounts reject
//     all mutations via AssertMutable.
//   - Task 5.3.33 — dead-man switch / countdown cancel-all
//     (countdown.go): per-account Redis timer (countdown:{account_id}
//     PX-TTL key + countdown:index ZSET for the sweeper); on expiry a
//     scoped mass-cancel fires through the OrderDispatcher seam.
//   - Task 5.3.36 — close-all positions (closeall.go): 2FA-gated,
//     scoped mass cancel followed by per-position reduce-only market
//     closes with slippage protection; partial failure surfaces
//     CLOSE_ALL_PARTIAL_FAILURE.
//
// Fail-closed (spec §2.7/§5.3): no code path in this package writes
// balances — position/balance reads go through plain SELECTs and all
// money movement stays inside the ledger/settlement services.
//
// Phase-5 stubs (per Task 5.3.12's dependency note): RBAC role checks
// and dual control are enforceable stubs — RoleResolver is injectable
// and freeze/unfreeze require a distinct second approver; Phase-07
// Tasks 7.3.1/7.3.2 replace the stubs with real RBAC middleware and
// four-eyes verification.
package accounts
