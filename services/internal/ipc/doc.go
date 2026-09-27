// Package ipc connects Go services to the C++ matching core (Task 1.3.5).
//
// Two transports implement the same order-flow channel:
//   - Aeron `aeron:ipc` (preferred): cgo wrapper over the vendored C client in
//     internal/ipc/aeron. C++ core publishes `aeron:ipc?alias=orders_out` and
//     subscribes `aeron:ipc?alias=orders_in`; this side mirrors.
//   - Shared-memory SPSC rings (fallback, zero-copy):
//     /dev/shm/{base}_{shard}_{in,out} — see shm.go + channel.go.
//
// All payloads are FlatBuffers Event envelopes (core/proto/exchange.fbs,
// generated Go in internal/ipc/wire). Never HTTP/gRPC in the hot path
// (spec §2.3).
package ipc
