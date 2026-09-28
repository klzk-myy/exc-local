// Package bridge implements the Aeron-to-NATS Bridge Service
// (Task 3.3.10, spec §2.3.1, §24 #210).
//
// One Bridge instance runs per matching-engine shard, NUMA-colocated with
// the engine process. It consumes the engine's outbound Aeron IPC stream
// (`aeron:ipc?alias=orders_out`, stream 1002) via a FragmentHandler with
// zero-copy decode of the flatbuffers Event envelope, then republishes each
// event to the NATS JetStream cold-path backbone on subjects of the form
// "{stream}.{shard_id}.{symbol}" (e.g. "trades.0.EUR-USD").
//
// Failure isolation (spec §2.3.1): a bounded in-memory ring (default 100k
// events) absorbs temporary NATS unavailability; the head-of-line is
// retried until the connection recovers, then buffered events replay in
// order. When the buffer is full the oldest event is evicted — the C++
// engine is never blocked by cold-path backpressure, and every eviction is
// counted (bridge_events_dropped_total) so consumers can detect the seq
// gap.
//
// This package is pure Go: the Aeron subscription and the NATS connection
// are injected (see cmd/bridge), so unit tests drive HandleFragment and a
// fake Publisher without cgo or a live cluster.
package bridge
