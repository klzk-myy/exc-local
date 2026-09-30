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
	"math"
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

// OpenRing maps shm object `name` ("/dev/shm/"+name). `create` is advisory:
// whichever endpoint finds a fresh (zero-size) image initializes it; a live
// image (magic stamped) is attached as-is — never re-initialized — so a
// late-joining peer can never wipe in-flight messages. Header capacity/
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
		_ = f.Close()
		return nil, err
	}
	needInit := st.Size() == 0
	if st.Size() > 0 && st.Size() < offSlots {
		_ = f.Close()
		return nil, fmt.Errorf("ipc: %s truncated image (%d bytes)", path, st.Size())
	}
	if needInit {
		if err := f.Truncate(wantSize); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("ipc: ftruncate %s: %w", path, err)
		}
	}
	// Fresh images map the size we just truncated; attaches map exactly the
	// existing file (never past EOF — avoids SIGBUS on a truncated image).
	mapLen := wantSize
	if st.Size() > 0 {
		mapLen = st.Size()
	}
	m, err := unix.Mmap(int(f.Fd()), 0, int(mapLen),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
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
				_ = r.Close()
				return nil, ErrAttachTimeout
			}
		}
		if atomic.LoadUint32(r.u32(offVersion)) != ShmVersion {
			_ = r.Close()
			return nil, ErrBadMagic
		}
		capacity = uint32(atomic.LoadUint64(r.u64(offCapacity)))
		slotPayload = uint32(atomic.LoadUint64(r.u64(offSlotPayload)))
		if int64(offSlots)+int64(capacity)*int64(slotHeaderBytes+slotPayload) > int64(len(m)) {
			_ = r.Close()
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

func (r *Ring) Name() string     { return r.name }
func (r *Ring) Role() RingRole   { return r.role }
func (r *Ring) Capacity() uint32 { return r.capacity }
func (r *Ring) SlotPayload() int { return int(r.slotPayload) }

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
	binary.LittleEndian.PutUint32(slot[8:], 0)              // flags
	binary.LittleEndian.PutUint32(slot[0:], uint32(h))      // seq (diagnostic)
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
	return PidAlive(r.ProducerPid())
}

// PidAlive probes a stamped producer pid via kill(pid, 0). pid 0 (never
// stamped) reports false — an absent pid is unproven liveness, not life.
func PidAlive(pid uint64) bool {
	if pid == 0 || pid > math.MaxInt32 {
		return false
	}
	err := syscall.Kill(int(pid), 0)
	return err == nil || err == syscall.EPERM
}

// RingHeader is a point-in-time snapshot of one ring's header — the
// read-only view a supervisor (exchange-watchdogd, spec §19.13.3 tier 3)
// needs to detect a hung producer without joining the SPSC protocol.
type RingHeader struct {
	Head        uint64 // producer write sequence
	Tail        uint64 // consumer read sequence
	HeartbeatNs uint64 // CLOCK_REALTIME ns; 0 = producer never beat
	ProducerPid uint64
	Drops       uint64
	Capacity    uint32
	SlotPayload uint32
	Version     uint32
}

// Occupancy is the pending unread message count (head - tail).
func (h RingHeader) Occupancy() uint64 { return h.Head - h.Tail }

// Utilization is Occupancy/Capacity in [0,1] (1 when capacity is unknown —
// a header that validated has capacity > 0, so this is defensive only).
func (h RingHeader) Utilization() float64 {
	if h.Capacity == 0 {
		return 1
	}
	return float64(h.Occupancy()) / float64(h.Capacity)
}

// ErrRingUninitialized is returned when the image exists but the producer
// never stamped the init magic — created-but-not-yet-configured.
var ErrRingUninitialized = errors.New("ipc: ring image present but init magic not stamped")

// ReadRingHeader maps an existing ring image read-only and snapshots its
// header. Unlike OpenRing it never creates or initializes the object — a
// supervisor must observe liveness, never fabricate it (spec §2.7
// fail-closed). Missing files, truncated images, un-stamped magic and
// version mismatches are all errors.
func ReadRingHeader(path string) (RingHeader, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return RingHeader{}, fmt.Errorf("ipc: open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return RingHeader{}, fmt.Errorf("ipc: stat %s: %w", path, err)
	}
	if st.Size() < offSlots {
		return RingHeader{}, fmt.Errorf("ipc: %s truncated image (%d bytes)", path, st.Size())
	}
	m, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return RingHeader{}, fmt.Errorf("ipc: mmap %s: %w", path, err)
	}
	defer func() { _ = unix.Munmap(m) }()

	u64 := func(off int) uint64 { return atomic.LoadUint64((*uint64)(unsafe.Pointer(&m[off]))) }
	u32 := func(off int) uint32 { return atomic.LoadUint32((*uint32)(unsafe.Pointer(&m[off]))) }

	switch magic := u32(offMagic); magic {
	case 0:
		return RingHeader{}, ErrRingUninitialized
	case ShmMagic:
	default:
		return RingHeader{}, fmt.Errorf("ipc: %s bad magic %#x: %w", path, magic, ErrBadMagic)
	}
	if v := u32(offVersion); v != ShmVersion {
		return RingHeader{}, fmt.Errorf("ipc: %s version %d: %w", path, v, ErrBadMagic)
	}
	return RingHeader{
		Head:        u64(offHead),
		Tail:        u64(offTail),
		HeartbeatNs: u64(offHeartbeat),
		ProducerPid: u64(offPid),
		Drops:       u64(offDrops),
		Capacity:    uint32(u64(offCapacity)),
		SlotPayload: uint32(u64(offSlotPayload)),
		Version:     u32(offVersion),
	}, nil
}
