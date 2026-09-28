// Package sbe implements the institutional SBE (Simple Binary Encoding)
// market-data surface of Phase 06:
//
//	Task 6.3.6  — institutional A/B multicast feed & recovery
//	              (spec §10.4, §10.6 item 3, §24 #166/#305, §27.2 catalog
//	              "SBE Multicast (A/B)")
//	Task 6.3.18 — negotiated SBE for REST, WebSocket and private streams
//	              with the six-month machine-readable schema lifecycle
//	              (spec §8.6, §23 SBE_SCHEMA_RETIRED /
//	              UNSUPPORTED_PROTOCOL_VERSION, §24 #284)
//
// Wire design (spec §27.2): dual independently routed UDP multicast feeds —
// Feed A 239.255.0.1:10001, Feed B 239.255.0.2:10002 — carry MoldUDP64-style
// framed packets stamped with a 64-bit monotonic channel sequence. Both
// feeds emit byte-identical packets: the publisher encodes each packet once
// and sends the same datagram on both transports. Consumers join both feeds,
// arbitrate by sequence (first-arriving copy wins), suppress duplicates, and
// detect byte divergence for the same sequence as SBE_FEED_A_DESYNC (L1
// alert per the §27.2 component catalog — an internal observability event,
// not a §23 HTTP code).
//
// Recovery (spec §10.4/§10.6): every published packet is appended to a
// bounded retained journal; a TCP replay service answers [fromSeq, toSeq]
// range requests for bounded gaps. When the gap precedes the journal horizon
// the request fails with SBE_REPLAY_GAP_EXCEEDED (L3 event) and the consumer
// falls back to the snapshot service (SBE_MULTICAST_RECOVERY, §10.6), which
// returns the full state at a lastSeq; incrementals that arrived meanwhile
// are queued and applied only once prev-seq continuity converges exactly —
// the assembler never emits a partially rebuilt stream.
//
// Transports are behind Sender/Receiver interfaces; LoopbackBus provides an
// in-memory deterministic transport for tests. Real multicast join is a
// deploy-time verification step (see UDPSender/UDPReceiver).
//
// Wave-2 seam: the publisher accepts already-shaped Message values and the
// snapshot path delegates full-state construction to a SnapshotSource
// implementation. internal/marketdata (in flight) wires live L2/L3/trade
// events and authoritative book state into those two interfaces.
package sbe
