// Task 19.3.10 — Go mirror of the C++ BilateralCreditMatrix shared-memory
// object (core/include/risk/BilateralCreditMatrix.h, spec §3.4/§13.8).
//
// ============================= ABI CONTRACT =============================
// /dev/shm/exchange_credit_matrix, 8,388,864 bytes, little-endian:
//
//	offset  0  u64 magic        "EXCRDIT1" (bytes, not host-order int)
//	offset  8  u32 version      1
//	offset 12  u32 max_parties  1024
//	offset 16  u64 update_seq   release-stored AFTER every cell write
//	offset 24  u64 writer_pid   single-writer advisory (Go stamps os.Getpid)
//	offset 32  224B reserved    MUST stay zero
//	offset 256 matrix           row-major u64 cell[a][b] at
//	                            256 + (a*1024 + b)*8 — directed remaining
//	                            credit of grantor a toward grantee b,
//	                            1e8 notional ticks. 0 = no credit → the
//	                            engine fails closed.
//
// Concurrency contract (mirrored verbatim from the C++ header): Go is the
// single writer of cells; every cell write is a release-store and bumps
// update_seq (atomic increment, also release) so lock-free readers never
// observe a torn/tentative line. consume_or_skip debits BOTH directed
// cells per fill — cell[maker][taker] AND cell[taker][maker] — with a
// rollback CAS on the first when the second lacks headroom.
//
// CREDIT_UPDATE control frame (credit_ctl_decode parity), 56 bytes:
//
//	+0  u32 magic "CRDU"   +4  u8 type=1   +5  u8 flags
//	+6  u16 version        +8  u64 seq     +16 u64 ts_ns
//	+24 u32 len (payload)  +28 u32 rsvd
//	+32 u32 party_a        +36 u32 party_b +40 u64 new_limit_ticks
package ipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// CreditMatrixName is the shm object name (no leading slash) shared
	// with core/src/risk/BilateralCreditMatrix.cpp.
	CreditMatrixName = "exchange_credit_matrix"

	// creditMatrixMagic is the byte string "EXCRDIT1" — stored with a
	// raw copy, never byte-swapped.
	creditMatrixMagic = "EXCRDIT1"

	// CreditMatrixVersion / CreditMatrixParties are fixed ABI values —
	// changing either renames the object, matching the C++ contract.
	CreditMatrixVersion uint32 = 1
	CreditMatrixParties uint32 = 1024

	creditOffMagic       = 0
	creditOffVersion     = 8
	creditOffMaxParties  = 12
	creditOffUpdateSeq   = 16
	creditOffWriterPid   = 24
	creditReservedOff    = 32
	creditReservedBytes  = 224
	creditHeaderBytes    = 256
	creditCellBytes      = 8
	CreditMatrixFileSize = creditHeaderBytes +
		int(CreditMatrixParties)*int(CreditMatrixParties)*creditCellBytes // 8,388,864

	creditAttachSpin = 5 * time.Second // parity with the C++ reader
)

// CREDIT_UPDATE frame constants — offsets inside the 56-byte frame.
const (
	CreditCtlMagic       uint32 = 0x55445243 // 'C' 'R' 'D' 'U' little-endian
	CreditCtlType        uint8  = 1
	CreditCtlVersion     uint16 = 1
	CreditCtlLen         uint32 = 24
	CreditCtlBytes              = 56
	creditOffCtlA               = 32
	creditOffCtlB               = 36
	creditOffCtlNewLimit        = 40
)

// CreditCtlResult mirrors the C++ credit_ctl_decode return enum — the
// values are part of the cross-language contract.
type CreditCtlResult int

const (
	CreditCtlOK CreditCtlResult = iota
	CreditCtlTooShort
	CreditCtlBadMagic
	CreditCtlBadVersion
	CreditCtlUnknownType
	CreditCtlPartyOutOfRange
)

func (r CreditCtlResult) String() string {
	switch r {
	case CreditCtlOK:
		return "OK"
	case CreditCtlTooShort:
		return "TOO_SHORT"
	case CreditCtlBadMagic:
		return "BAD_MAGIC"
	case CreditCtlBadVersion:
		return "BAD_VERSION"
	case CreditCtlUnknownType:
		return "UNKNOWN_TYPE"
	case CreditCtlPartyOutOfRange:
		return "PARTY_OUT_OF_RANGE"
	}
	return "UNKNOWN"
}

// CreditUpdateMsg is the decoded CREDIT_UPDATE payload.
type CreditUpdateMsg struct {
	Seq      uint64
	PartyA   uint32
	PartyB   uint32
	NewLimit uint64 // 1e8 notional ticks
}

// EncodeCreditUpdate builds a canonical 56-byte CREDIT_UPDATE frame.
// Exported for tests and for any in-process publisher that must emit the
// exact wire bytes (the Go manager normally writes cells directly).
func EncodeCreditUpdate(seq uint64, partyA, partyB uint32, newLimit uint64) []byte {
	buf := make([]byte, CreditCtlBytes)
	binary.LittleEndian.PutUint32(buf[0:], CreditCtlMagic)
	buf[4] = CreditCtlType
	binary.LittleEndian.PutUint16(buf[6:], CreditCtlVersion)
	binary.LittleEndian.PutUint64(buf[8:], seq)
	binary.LittleEndian.PutUint32(buf[24:], CreditCtlLen)
	binary.LittleEndian.PutUint32(buf[creditOffCtlA:], partyA)
	binary.LittleEndian.PutUint32(buf[creditOffCtlB:], partyB)
	binary.LittleEndian.PutUint64(buf[creditOffCtlNewLimit:], newLimit)
	return buf
}

// DecodeCreditUpdate validates a control frame exactly like the C++
// credit_ctl_decode: header → magic → version → type → len → party range.
// Nothing is applied by this function — callers see either OK + a fully
// populated message or a failure code (no partial application).
func DecodeCreditUpdate(buf []byte) (*CreditUpdateMsg, CreditCtlResult) {
	if len(buf) < CreditCtlBytes {
		return nil, CreditCtlTooShort
	}
	if binary.LittleEndian.Uint32(buf[0:]) != CreditCtlMagic {
		return nil, CreditCtlBadMagic
	}
	if binary.LittleEndian.Uint16(buf[6:]) != CreditCtlVersion {
		return nil, CreditCtlBadVersion
	}
	if buf[4] != CreditCtlType {
		return nil, CreditCtlUnknownType
	}
	if binary.LittleEndian.Uint32(buf[24:]) != CreditCtlLen {
		return nil, CreditCtlTooShort
	}
	a := binary.LittleEndian.Uint32(buf[creditOffCtlA:])
	b := binary.LittleEndian.Uint32(buf[creditOffCtlB:])
	if a >= CreditMatrixParties || b >= CreditMatrixParties {
		return nil, CreditCtlPartyOutOfRange
	}
	return &CreditUpdateMsg{
		Seq:    binary.LittleEndian.Uint64(buf[8:]),
		PartyA: a, PartyB: b,
		NewLimit: binary.LittleEndian.Uint64(buf[creditOffCtlNewLimit:]),
	}, CreditCtlOK
}

// CreditCellOffset returns the file offset of cell[a][b] — exported so
// tests assert the ABI layout directly.
func CreditCellOffset(a, b uint32) uint64 {
	return creditHeaderBytes + (uint64(a)*uint64(CreditMatrixParties)+uint64(b))*creditCellBytes
}

// CreditMatrix is the single-writer Go handle onto
// /exchange_credit_matrix. nil-guarded like the rest of internal/ipc —
// an absent matrix disables publication, never fabricates credit.
type CreditMatrix struct {
	f    *os.File
	map_ []byte
	seq  uint64 // local counter for emitted control frames
}

// OpenCreditMatrix maps shm object `name` ("/dev/shm/"+name). `create`
// is advisory like OpenRing: whichever endpoint finds a fresh
// (zero-size) image initialises it — header fields first, magic LAST so
// attaching readers never observe a half-built file. A live image is
// attached as-is and validated (magic → version → max_parties).
func OpenCreditMatrix(name string, create bool) (*CreditMatrix, error) {
	path := "/dev/shm/" + name
	flag := os.O_RDWR | os.O_CREATE
	if create {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return nil, fmt.Errorf("credit matrix open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("credit matrix stat %s: %w", path, err)
	}
	needInit := st.Size() == 0
	if st.Size() > 0 && st.Size() != int64(CreditMatrixFileSize) {
		_ = f.Close()
		return nil, fmt.Errorf("credit matrix: %s size %d != ABI size %d",
			path, st.Size(), CreditMatrixFileSize)
	}
	if needInit {
		if err := unix.Ftruncate(int(f.Fd()), int64(CreditMatrixFileSize)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("credit matrix truncate: %w", err)
		}
	}
	m := &CreditMatrix{f: f}
	m.map_, err = unix.Mmap(int(f.Fd()), 0, CreditMatrixFileSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("credit matrix mmap: %w", err)
	}
	if needInit {
		m.initHeader()
		return m, nil
	}
	if err := m.attachCheck(); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

// initHeader writes version/max_parties and stamps writer_pid, zeroes the
// reserved region, then publishes the magic with a release-ordered store —
// readers spin on magic. The matrix area comes up zeroed by ftruncate:
// cell==0 ⇒ no credit, which is the contract's closed state.
func (m *CreditMatrix) initHeader() {
	for i := range m.map_ {
		m.map_[i] = 0 // page-fault the object fully + deterministic zero
	}
	m.stampWriterPid()
	binary.LittleEndian.PutUint32(m.map_[creditOffVersion:], CreditMatrixVersion)
	binary.LittleEndian.PutUint32(m.map_[creditOffMaxParties:], CreditMatrixParties)
	atomic.StoreUint64(m.updateSeqPtr(), 0)
	copy(m.map_[creditOffMagic:creditOffMagic+8], creditMatrixMagic)
	// Release fence — every byte above must be visible before the magic.
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&m.map_[creditOffMagic])),
		binary.LittleEndian.Uint32(m.map_[creditOffMagic:]))
}

// attachCheck waits for the single writer's magic then validates the
// fixed header fields — parity with the C++ constructor's 5s spin.
func (m *CreditMatrix) attachCheck() error {
	deadline := time.Now().Add(creditAttachSpin)
	for !m.magicOK() {
		if time.Now().After(deadline) {
			return errors.New("credit matrix: magic EXCRDIT1 not observed within 5s")
		}
		time.Sleep(time.Millisecond)
	}
	if got := binary.LittleEndian.Uint32(m.map_[creditOffVersion:]); got != CreditMatrixVersion {
		return fmt.Errorf("credit matrix: version %d != %d", got, CreditMatrixVersion)
	}
	if got := binary.LittleEndian.Uint32(m.map_[creditOffMaxParties:]); got != CreditMatrixParties {
		return fmt.Errorf("credit matrix: max_parties %d != %d", got, CreditMatrixParties)
	}
	return nil
}

func (m *CreditMatrix) magicOK() bool {
	return string(m.map_[creditOffMagic:creditOffMagic+8]) == creditMatrixMagic
}

func (m *CreditMatrix) cellPtr(a, b uint32) *uint64 {
	return (*uint64)(unsafe.Pointer(&m.map_[CreditCellOffset(a, b)]))
}

func (m *CreditMatrix) updateSeqPtr() *uint64 {
	return (*uint64)(unsafe.Pointer(&m.map_[creditOffUpdateSeq]))
}

// StampWriterPid writes the single-writer advisory pid (offset 24) — the
// C++ header's writers_stamp_pid equivalent. Idempotent.
func (m *CreditMatrix) stampWriterPid() {
	binary.LittleEndian.PutUint64(m.map_[creditOffWriterPid:], uint64(os.Getpid()))
}

// WriterPid returns the stamped single-writer pid (diagnostics).
func (m *CreditMatrix) WriterPid() uint64 {
	if m == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(m.map_[creditOffWriterPid:])
}

// ApplyUpdate release-writes cell[a][b]=newLimit then bumps update_seq —
// the exact two-step the C++ apply_update performs. Safe for the matrix
// owner; readers see either the old or the new limit, never a tear.
func (m *CreditMatrix) ApplyUpdate(a, b uint32, newLimit uint64) bool {
	if m == nil || a >= CreditMatrixParties || b >= CreditMatrixParties {
		return false
	}
	atomic.StoreUint64(m.cellPtr(a, b), newLimit)
	atomic.AddUint64(m.updateSeqPtr(), 1)
	return true
}

// CreditLimit reads cell[a][b] atomically (diagnostics + tests).
func (m *CreditMatrix) CreditLimit(a, b uint32) uint64 {
	if m == nil || a >= CreditMatrixParties || b >= CreditMatrixParties {
		return 0
	}
	return atomic.LoadUint64(m.cellPtr(a, b))
}

// UpdatesApplied reads update_seq — how many cell writes readers have
// been shown. Mirrors the C++ updates_applied().
func (m *CreditMatrix) UpdatesApplied() uint64 {
	if m == nil {
		return 0
	}
	return atomic.LoadUint64(m.updateSeqPtr())
}

// HasHeadroom mirrors the C++ reader check: BOTH directed cells must
// cover notionalTicks. Parity helper for tests and non-commit paths.
func (m *CreditMatrix) HasHeadroom(maker, taker uint32, notionalTicks uint64) bool {
	if m == nil || maker >= CreditMatrixParties || taker >= CreditMatrixParties {
		return false
	}
	return atomic.LoadUint64(m.cellPtr(maker, taker)) >= notionalTicks &&
		atomic.LoadUint64(m.cellPtr(taker, maker)) >= notionalTicks
}

// ConsumeOrSkip mirrors the C++ try_debit: CAS cell[maker][taker], then
// cell[taker][maker], rolling the first back on failure. Two concurrent
// calls can interleave the same way the C++ version can (second CAS on
// the already-debited cell fails → rollback restores) — identical
// semantics, used by the Go manager's debit-parity tests and as the
// fail-closed fallback when control messages arrive in-process.
func (m *CreditMatrix) ConsumeOrSkip(maker, taker uint32, notionalTicks uint64) bool {
	if m == nil || maker >= CreditMatrixParties || taker >= CreditMatrixParties {
		return false
	}
	fwd := m.cellPtr(maker, taker)
	rev := m.cellPtr(taker, maker)
	for {
		cur := atomic.LoadUint64(fwd)
		if cur < notionalTicks {
			return false
		}
		if atomic.CompareAndSwapUint64(fwd, cur, cur-notionalTicks) {
			break
		}
	}
	for {
		cur := atomic.LoadUint64(rev)
		if cur < notionalTicks {
			atomic.AddUint64(fwd, notionalTicks) // rollback
			return false
		}
		if atomic.CompareAndSwapUint64(rev, cur, cur-notionalTicks) {
			atomic.AddUint64(m.updateSeqPtr(), 1)
			return true
		}
	}
}

// NextCtlSeq returns the seq for the next emitted CREDIT_UPDATE frame.
func (m *CreditMatrix) NextCtlSeq() uint64 {
	return atomic.AddUint64(&m.seq, 1)
}

// EmitCreditUpdate builds the 56-byte control frame for a limit change —
// the bytes a publisher would hand to the C++ control channel if the
// deployment flows CREDIT_UPDATE over Aeron instead of direct cell
// writes. The Go manager writes cells itself; this keeps the codec wired
// and testable end to end.
func (m *CreditMatrix) EmitCreditUpdate(partyA, partyB uint32, newLimit uint64) []byte {
	return EncodeCreditUpdate(m.NextCtlSeq(), partyA, partyB, newLimit)
}

// OnControlMessage decodes an inbound CREDIT_UPDATE frame and applies it
// atomically — returns the decode verdict so callers can count malformed
// traffic (C++ parity: never partially apply).
func (m *CreditMatrix) OnControlMessage(buf []byte) CreditCtlResult {
	msg, res := DecodeCreditUpdate(buf)
	if res != CreditCtlOK {
		return res
	}
	if !m.ApplyUpdate(msg.PartyA, msg.PartyB, msg.NewLimit) {
		return CreditCtlPartyOutOfRange
	}
	return CreditCtlOK
}

// Close unmaps and closes the file. The shm object itself survives —
// readers keep working from the published cells.
func (m *CreditMatrix) Close() error {
	if m == nil {
		return nil
	}
	var err error
	if m.map_ != nil {
		err = unix.Munmap(m.map_)
		m.map_ = nil
	}
	if m.f != nil {
		if cerr := m.f.Close(); err == nil {
			err = cerr
		}
		m.f = nil
	}
	return err
}
