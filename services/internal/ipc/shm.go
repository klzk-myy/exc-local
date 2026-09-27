// Task 1.3.5 — shared-memory SPSC ring, Go endpoint.
//
// Byte-for-byte mirror of core/include/ipc/ShmRing.hpp — the layout is the
// ABI contract between the C++ core and Go services:
//
//	header (320B): head@0  tail@64  heartbeat_ns@128 (CLOCK_REALTIME)
//	               producer_pid@192  magic@256 u32, version@260 u32
//	               capacity@264 u64  slot_payload@272 u64  drops@280 u64
//	slots  @320:   stride = 64B slot header {seq u32, len u32, flags u32,pad}
//	               + slot_payload bytes
//
// One ring carries one direction; a Channel pairs "_in" (Go -> C++) with
// "_out" (C++ -> Go). Ordering discipline is the canonical SPSC release/
// acquire on head (producer) and tail (consumer); the per-slot seq field is
// diagnostic only. Pure syscall + atomics — no cgo in this file.
//
// Backpressure: TryWrite returns false and bumps drops when head-tail ==
// capacity. Liveness: ProducerAlive probes the stamped pid via kill(pid, 0);
// ProducerHeartbeatNs exposes the realtime-ns heartbeat (diagnostics).

package ipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ShmMagic   uint32 = 0x45584348 // "EXCH"
	ShmVersion uint32 = 1

	offHead        = 0
	offTail        = 64
	offHeartbeat   = 128
	offPid         = 192
	offMagic       = 256
	offVersion     = 260
	offCapacity    = 264
	offSlotPayload = 272
	offDrops       = 280
	offSlots       = 320

	slotHeaderBytes = 64

	// DefaultRingCapacity / DefaultRingSlotPayload mirror
	// ShmRing::kDefaultCapacity / kDefaultSlotPayload.
	DefaultRingCapacity    uint32 = 4096
	DefaultRingSlotPayload uint32 = 1024

	attachMagicTimeout = 5 * time.Second
)

// RingRole selects which end of the SPSC ring this process holds.
type RingRole int

const (
	RoleProducer RingRole = iota
	RoleConsumer
)

var (
	ErrRingNotOpen   = errors.New("ipc: ring not open")
	ErrNotProducer   = errors.New("ipc: ring endpoint is not the producer")
	ErrNotConsumer   = errors.New("ipc: ring endpoint is not the consumer")
	ErrMsgTooLarge   = errors.New("ipc: message exceeds slot payload")
	ErrAttachTimeout = errors.New("ipc: timed out waiting for ring init magic")
	ErrBadMagic      = errors.New("ipc: bad ring magic/version")
)

// Ring is one mmap'd SPSC ring on /dev/shm. Zero-copy: Peek returns a slice
// aliasing the slot payload (valid until Consume).
type Ring struct {
	f    *os.File
	m    []byte
	name string

	role        RingRole
	capacity    uint32
	mask        uint64
	slotPayload uint32
	slotStride  uint64
}

// OpenRing maps shm object `name` ("/dev/shm/"+name). `create` means this
// endpoint sizes and initializes the image; otherwise it attaches and spins
// (bounded) until the creator stamps the magic word. Header capacity/
// slot_payload are authoritative — an attaching endpoint adopts them.
func OpenRing(name string, role RingRole, create bool, capacity, slotPayload uint32) (*Ring, error) {
	if capacity == 0 || capacity&(capacity-1) != 0 {
		return nil, fmt.Errorf("ipc: capacity %d not a power of two", capacity)
	}
	if slotPayload == 0 || slotPayload%64 != 0 {
		return nil, fmt.Errorf("ipc: slot payload %d not a multiple of 64", slotPayload)
	}

	path := "/dev/shm/" + name
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("ipc: open %s: %w", path, err)
	}

	wantSize := int64(offSlots) + int64(capacity)*int64(slotHeaderBytes+slotPayload)

	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	needInit := create || st.Size() == 0
	if needInit {
		if err := f.Truncate(wantSize); err != nil {
			f.Close()
			return nil, fmt.Errorf("ipc: ftruncate %s: %w", path, err)
		}
	}
	mapLen := wantSize
	if st.Size() > wantSize {
		mapLen = st.Size()
	}
	m, err := unix.Mmap(int(f.Fd()), 0, int(mapLen),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("ipc: mmap %s: %w", path, err)
	}

	r := &Ring{f: f, m: m, name: name, role: role}

	if needInit {
		// Config first, magic last — the seq-cst magic store is the
		// init-complete barrier for attaching peers.
		atomic.StoreUint32(r.u32(offVersion), ShmVersion)
		atomic.StoreUint64(r.u64(offCapacity), uint64(capacity))
		atomic.StoreUint64(r.u64(offSlotPayload), uint64(slotPayload))
		atomic.StoreUint64(r.u64(offDrops), 0)
		atomic.StoreUint64(r.u64(offHead), 0)
		atomic.StoreUint64(r.u64(offTail), 0)
		atomic.StoreUint64(r.u64(offHeartbeat), 0)
		atomic.StoreUint64(r.u64(offPid), 0)
		atomic.StoreUint32(r.u32(offMagic), ShmMagic)
	} else {
		deadline := time.Now().Add(attachMagicTimeout)
		for atomic.LoadUint32(r.u32(offMagic)) != ShmMagic {
			if time.Now().After(deadline) {
				r.Close()
				return nil, ErrAttachTimeout
			}
		}
		if atomic.LoadUint32(r.u32(offVersion)) != ShmVersion {
			r.Close()
			return nil, ErrBadMagic
		}
		capacity = uint32(atomic.LoadUint64(r.u64(offCapacity)))
		slotPayload = uint32(atomic.LoadUint64(r.u64(offSlotPayload)))
		if int64(offSlots)+int64(capacity)*int64(slotHeaderBytes+slotPayload) > int64(len(m)) {
			r.Close()
			return nil, fmt.Errorf("ipc: ring %s larger than mapping", name)
		}
	}

	r.capacity = capacity
	r.mask = uint64(capacity) - 1
	r.slotPayload = slotPayload
	r.slotStride = uint64(slotHeaderBytes) + uint64(slotPayload)

	if role == RoleProducer {
		atomic.StoreUint64(r.u64(offPid), uint64(os.Getpid()))
		r.Beat()
	}
	return r, nil
}

func (r *Ring) Close() error {
	if r.m != nil {
		_ = unix.Munmap(r.m)
		r.m = nil
	}
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

func (r *Ring) Name() string      { return r.name }
func (r *Ring) Role() RingRole    { return r.role }
func (r *Ring) Capacity() uint32  { return r.capacity }
func (r *Ring) SlotPayload() int  { return int(r.slotPayload) }

func (r *Ring) u64(off int) *uint64 { return (*uint64)(unsafe.Pointer(&r.m[off])) }
func (r *Ring) u32(off int) *uint32 { return (*uint32)(unsafe.Pointer(&r.m[off])) }

func (r *Ring) slot(index uint64) []byte {
	start := int(offSlots) + int(index&r.mask)*int(r.slotStride)
	return r.m[start : start+int(r.slotStride)]
}

// TryWrite appends p to the ring. false => full (backpressure; Drops()
// increments) or message too large. Single-producer only.
func (r *Ring) TryWrite(p []byte) bool {
	if r.m == nil || r.role != RoleProducer || uint32(len(p)) > r.slotPayload {
		return false
	}
	h := atomic.LoadUint64(r.u64(offHead))
	t := atomic.LoadUint64(r.u64(offTail))
	if h-t >= uint64(r.capacity) {
		atomic.AddUint64(r.u64(offDrops), 1)
		return false
	}
	slot := r.slot(h)
	binary.LittleEndian.PutUint32(slot[4:], uint32(len(p))) // len
	binary.LittleEndian.PutUint32(slot[8:], 0)             // flags
	binary.LittleEndian.PutUint32(slot[0:], uint32(h))     // seq (diagnostic)
	copy(slot[slotHeaderBytes:], p)
	r.Beat()
	// Seq-cst store: slot contents are visible to the consumer before head
	// advances (release semantics on every supported arch).
	atomic.StoreUint64(r.u64(offHead), h+1)
	return true
}

// WriteWait spins until the write lands or the deadline passes; for tests and
// controlled producers — the hot path uses TryWrite + backpressure policy.
func (r *Ring) WriteWait(p []byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !r.TryWrite(p) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// Beat refreshes the producer heartbeat (realtime ns, wall clock so the
// C++ peer can compare).
func (r *Ring) Beat() {
	atomic.StoreUint64(r.u64(offHeartbeat), uint64(time.Now().UnixNano()))
}

// Peek returns the next inbound payload aliasing the slot (zero-copy; valid
// until Consume). nil when drained or when the slot is corrupt (len field
// out of bounds — fail closed).
func (r *Ring) Peek() []byte {
	if r.m == nil || r.role != RoleConsumer {
		return nil
	}
	t := atomic.LoadUint64(r.u64(offTail))
	h := atomic.LoadUint64(r.u64(offHead))
	if t == h {
		return nil
	}
	slot := r.slot(t)
	n := binary.LittleEndian.Uint32(slot[4:])
	if n > r.slotPayload {
		return nil
	}
	return slot[slotHeaderBytes : slotHeaderBytes+int(n)]
}

// Consume commits the slot returned by Peek. Single-consumer only.
func (r *Ring) Consume() {
	atomic.AddUint64(r.u64(offTail), 1)
}

// Poll copies the next inbound message into buf (slot -> caller destination,
// no intermediate buffer). Returns bytes copied, 0 when drained, -1 if buf
// is too small (slot left pending).
func (r *Ring) Poll(buf []byte) int {
	p := r.Peek()
	if p == nil {
		return 0
	}
	if len(p) > len(buf) {
		return -1
	}
	n := copy(buf, p)
	r.Consume()
	return n
}

// Occupancy returns current fill (head - tail).
func (r *Ring) Occupancy() uint64 {
	return atomic.LoadUint64(r.u64(offHead)) - atomic.LoadUint64(r.u64(offTail))
}

// Drops counts producer write attempts refused because the ring was full.
func (r *Ring) Drops() uint64 { return atomic.LoadUint64(r.u64(offDrops)) }

// ProducerPid is the pid stamped by the far-end producer.
func (r *Ring) ProducerPid() uint64 { return atomic.LoadUint64(r.u64(offPid)) }

// ProducerHeartbeatNs is the realtime-ns timestamp the producer last wrote.
func (r *Ring) ProducerHeartbeatNs() uint64 {
	return atomic.LoadUint64(r.u64(offHeartbeat))
}

// ProducerAlive reports whether the stamped pid still exists (kill(pid, 0)).
func (r *Ring) ProducerAlive() bool {
	pid := r.ProducerPid()
	if pid == 0 {
		return false
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || err == syscall.EPERM
}
