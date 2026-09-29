// Package delegation implements Phase-12 Task 12.3.11 — institutional
// delegated logins and client multi-validator (M-of-N) controls.
//
// This is CLIENT-side workforce RBAC, deliberately distinct from the
// venue-admin RBAC of internal/admin (migration 090): the admin role
// vocabulary is never reused here. Delegated principals register in
// principal_role_systems with role_system='CLIENT_DELEGATED' — the
// migration-090 disjoint-system exclusion makes a principal incapable of
// holding both role systems.
//
// Data model (migration 074):
//
//	client_delegated_users    named human logins under a master account
//	client_role_bindings      CLIENT_* role + explicit account/instrument
//	                          scope (one ACTIVE binding per delegate)
//	client_approval_policies  per-(master, operation) M-of-N policy with
//	                          amount threshold and approval-window expiry
//	client_approval_requests  PENDING → APPROVED/REJECTED/EXPIRED; an
//	                          APPROVED row is CONSUMED when the gated
//	                          operation replays through the funding seam
//	client_approval_decisions one vote per (request, approver)
//	client_delegation_events  append-only audit: delegation, login,
//	                          action, approval, revocation, scope change
//
// Enforcement surfaces this package provides:
//
//   - ResolveIdentity / Authorize — the scope-check helper the gateway
//     invokes for delegated sessions (cluster-1 owns the login flow;
//     it calls RecordLogin after credential verification).
//   - CheckApprovalRequired / RequestApproval / Decide — the M-of-N
//     engine; funding.WithdrawalApprovalGate adapts CheckWithdrawal
//     into the Phase-11 withdrawal create path.
//   - Revoke / RevokeAll — emergency master revocation terminating
//     delegated sessions through the injected SessionTerminator seam.
//
// Fail-closed (spec §2.7): absent bindings deny, expired bindings deny,
// an absent policy denies nothing (opt-in controls), a policy with too
// few eligible approvers cannot be requested, and approvers can never
// approve their own requests.
package delegation
