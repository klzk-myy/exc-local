// Package fixsbe implements the Phase-18 ultra-low-latency binary
// order-entry gateway (Task 18.3.8, spec §9.5, §24 #128) and its
// production transport (Task 18.3.17, §24 #284/#289):
//
//   - schema/order_entry.xml — the canonical SBE schema (schema id 2,
//     version 1; schema 1 is the Phase-06 market-data feed). Fixed-layout
//     little-endian blocks, 8-byte aligned, no varData: decode is
//     positional reads into POD structs — zero deserialization.
//   - codec.go / message.go — the Go codec. Decoders honor
//     Header.BlockLength so newer-version tails skip cleanly; the
//     schema lifecycle reuses the Phase-06 registry
//     (internal/sbe.Registry.Negotiate → SBE_SCHEMA_RETIRED /
//     UNSUPPORTED_PROTOCOL_VERSION).
//   - gateway.go — dispatch onto the canonical orders.Service path
//     (OrderAPI seam): the binary surface shares admission, dedup and
//     audit with REST/FIX.
//   - transport.go — the production listener set: one accept loop
//     serving tag-value FIX and SBE binary frames (and the tag-value
//     request / SBE response combination), TLS 1.3 SNI-bound, Ed25519
//     session-key handshake.
//   - drain.go — the graceful maintenance drain: repeated News
//     advisories (SBE template 103 / FIX tag-value 35=B) naming the
//     replacement endpoint, cancels preserved.
//   - aeron.go — the Aeron channel binding (subscription → gateway →
//     response publication); session binding is performed at
//     subscription time after out-of-band Ed25519 negotiation.
//
// Ingress budget (Task 18.3.8 AC: <5µs). Measured on this build host
// (go1.26 linux/amd64, Intel i7-14700K), bench_test.go:
//
//	BenchmarkIngressDecode   ~25 ns/op   — frame → decoded message
//	BenchmarkIngressGateway  ~411 ns/op  — decode + entitlement +
//	    request mapping + response encode against a stubbed OrderAPI
//
// Both are far inside the 5µs budget. The NIC-arrival and
// Aeron-publication legs are deployment-measured (kernel-bypass NIC +
// media driver), not unit-benchable here — the honest number is the
// in-process slice above.
package fixsbe
