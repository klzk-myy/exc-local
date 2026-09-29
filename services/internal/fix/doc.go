// Package fix implements the Phase-18 FIX protocol gateway: FIX 4.4
// session management over quickfixgo (Task 18.3.1), order entry mapped
// onto the canonical orders.Service pipeline (Task 18.3.2), session
// entitlement + cancel-on-disconnect + per-session throttling
// (Task 18.3.9), and the shared dead-man countdown over UserRequest
// 35=BE (Task 18.3.16).
//
// Naming deviation (documented): the phase plan names the binary
// services/cmd/fixgateway; the repo's cmd/ convention is single-word
// service names (cmd/gateway, cmd/bridge, cmd/marketdata …) and the
// scaffold already lives at services/cmd/fix — the binary stays cmd/fix.
// The plan's services/internal/fix/* file locations are followed as
// written.
//
// Seams exposed for sibling Phase-18 tasks:
//
//   - Drop copy (Tasks 18.3.4/18.3.6): App.WithReportTap fans every
//     emitted ExecutionReport (35=8) to registered taps — attach a
//     drop-copy session relay.
//   - Mass quoting / MM flow (Task 18.3.7/.10): App's OrderFlow seam
//     (Submit/Cancel/…) plus AeronSubmitter are reusable for quote-side
//     order submission.
//   - Failover & gap fill (Task 18.3.12): Store.SeqState /
//     Store.RefreshSession expose the persisted sequence numbers the
//     hot-standby resync needs; the PG-backed quickfix.MessageStore is
//     already shared-state safe.
//   - Recovery orchestrator (Phase-04.5): Gateway implements
//     recovery.OrchFixBroadcaster (BroadcastTradingSessionStatus /
//     ResolveGaps) stubs for Task 18.3.15/18.3.18 to fill.
package fix
