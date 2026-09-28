// Package recovery — Go recovery service (Phase-04 Task 4.3.1, spec §3.5):
// persists matching-engine book snapshots into PostgreSQL book_snapshots
// (migration 023) and acknowledges each persisted snapshot_seq back to the
// engine, whose ack drain then trims sealed WAL segments below it.
//
// Wire seam (mirrors core/include/recovery/SnapshotManager.hpp):
//
//	{base}_{shard}_snap      core -> Go   SnapReadyMsg (296B packed record)
//	{base}_{shard}_snap_ack  Go -> core   SnapAckMsg   (32B packed record)
//
// The multi-MB snapshot payload never crosses the ring: the engine's
// FileSnapshotSink writes {snap_root}/i{iid}/snap_{seq}.bin atomically
// (rename after fsync), and SnapReadyMsg carries only the descriptor —
// rel path, byte size, CRC32C (Castagnoli) of the whole file. This service
// re-reads the file, verifies size + CRC, upserts the raw file bytes into
// snapshot_data (the SnapFileHeader rides inside, so the stored row is
// byte-identical to the durable file), then emits SnapAckMsg.
//
// The ring notification is only the low-latency hint: ScanOnce sweeps the
// snap dir for snap_*.bin files not yet persisted, so a dropped
// notification (ring full, service restart) is caught on the next sweep.
// Upserts are idempotent via UNIQUE (shard_id, instrument_id,
// snapshot_seq); a conflicting byte length is an integrity incident and is
// NOT acked.
package recovery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ipc"
)

// --- Wire contract (keep byte-for-byte with SnapshotManager.hpp) -----------

const (
	readyMagic   uint32 = 0x59445253 // 'SRDY'
	ackMagic     uint32 = 0x4B434153 // 'SACK'
	snapMsgVer   uint16 = 1
	ackPersisted uint32 = 0

	relPathCap  = 256
	readyMsgLen = 296
	ackMsgLen   = 32

	// Ring geometry: small — ready/ack traffic is one message per snapshot
	// (100k trades or 5min cadence). Slot payload must hold SnapReadyMsg.
	ringCapacity    uint32 = 128
	ringSlotPayload uint32 = 1024
)

// ReadyRingName / AckRingName are the canonical shm object names
// (snap_ready_name / snap_ack_name in the C++ endpoint).
func ReadyRingName(base string, shard uint16) string {
	return fmt.Sprintf("%s_%d_snap", base, shard)
}
func AckRingName(base string, shard uint16) string {
	return fmt.Sprintf("%s_%d_snap_ack", base, shard)
}

// ReadyMsg is the decoded SnapReadyMsg descriptor.
type ReadyMsg struct {
	ShardID      uint16
	InstrumentID uint32
	SnapshotSeq  uint64
	ByteSize     uint64
	FileCRC32C   uint32
	RelPath      string
}

// ParseReadyMsg decodes a SnapReadyMsg slot payload. ok=false on any
// structural violation — callers count and drop, never panic.
func ParseReadyMsg(b []byte) (m ReadyMsg, ok bool) {
	if len(b) != readyMsgLen {
		return m, false
	}
	if binary.LittleEndian.Uint32(b[0:]) != readyMagic ||
		binary.LittleEndian.Uint16(b[4:]) != snapMsgVer {
		return m, false
	}
	m.ShardID = binary.LittleEndian.Uint16(b[6:])
	m.InstrumentID = binary.LittleEndian.Uint32(b[8:])
	// b[12:16] reserved
	m.SnapshotSeq = binary.LittleEndian.Uint64(b[16:])
	m.ByteSize = binary.LittleEndian.Uint64(b[24:])
	m.FileCRC32C = binary.LittleEndian.Uint32(b[32:])
	n := binary.LittleEndian.Uint32(b[36:])
	if n > relPathCap || n == 0 {
		return m, false
	}
	m.RelPath = string(b[40 : 40+n])
	return m, true
}

// EncodeAck serializes SnapAckMsg for the ack ring.
func EncodeAck(shardID uint16, instrumentID uint32, snapshotSeq,
	snapshotID uint64, status uint32) []byte {
	b := make([]byte, ackMsgLen)
	binary.LittleEndian.PutUint32(b[0:], ackMagic)
	binary.LittleEndian.PutUint16(b[4:], snapMsgVer)
	binary.LittleEndian.PutUint16(b[6:], shardID)
	binary.LittleEndian.PutUint32(b[8:], instrumentID)
	binary.LittleEndian.PutUint32(b[12:], status)
	binary.LittleEndian.PutUint64(b[16:], snapshotSeq)
	binary.LittleEndian.PutUint64(b[24:], snapshotID)
	return b
}

// snapRelPath is the canonical relative name the engine writes and the
// descriptor must name exactly (FileSnapshotSink layout).
func snapRelPath(instrumentID uint32, seq uint64) string {
	return fmt.Sprintf("i%d/snap_%020d.bin", instrumentID, seq)
}

// --- Store seam --------------------------------------------------------------

// SnapshotStore persists the verified snapshot bytes. Production:
// PgxSnapshotStore; tests substitute a fake.
type SnapshotStore interface {
	// UpsertBookSnapshot inserts (shard, instrument, seq, data) and
	// returns (row id, inserted=true). On a pre-existing row it returns
	// that row's id with inserted=false after verifying the stored length
	// matches — a length mismatch is an integrity error.
	UpsertBookSnapshot(ctx context.Context, shardID uint16,
		instrumentID uint32, snapshotSeq uint64,
		data []byte) (id int64, inserted bool, err error)
}

// PgxSnapshotStore implements SnapshotStore over book_snapshots.
type PgxSnapshotStore struct {
	pool *pgxpool.Pool
}

// NewPgxSnapshotStore wraps a pgx pool.
func NewPgxSnapshotStore(pool *pgxpool.Pool) (*PgxSnapshotStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("recovery: nil pgx pool")
	}
	return &PgxSnapshotStore{pool: pool}, nil
}

func (s *PgxSnapshotStore) UpsertBookSnapshot(ctx context.Context,
	shardID uint16, instrumentID uint32, snapshotSeq uint64,
	data []byte) (int64, bool, error) {
	if snapshotSeq > math.MaxInt64 {
		return 0, false, fmt.Errorf(
			"recovery: snapshot_seq %d overflows BIGINT", snapshotSeq)
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO book_snapshots
		    (shard_id, instrument_id, snapshot_seq, snapshot_data)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (shard_id, instrument_id, snapshot_seq) DO NOTHING
		RETURNING snapshot_id`,
		int16(shardID), int32(instrumentID), int64(snapshotSeq),
		data).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false,
			fmt.Errorf("recovery: insert book_snapshots: %w", err)
	}
	// Conflict — idempotent redelivery path. Verify the stored row is the
	// same snapshot (length match) before acking; a divergence is a
	// fail-closed integrity incident, never acked.
	var storedLen int64
	qerr := s.pool.QueryRow(ctx, `
		SELECT snapshot_id, octet_length(snapshot_data)
		FROM book_snapshots
		WHERE shard_id = $1 AND instrument_id = $2 AND snapshot_seq = $3`,
		int16(shardID), int32(instrumentID), int64(snapshotSeq)).
		Scan(&id, &storedLen)
	if qerr != nil {
		return 0, false,
			fmt.Errorf("recovery: re-read book_snapshots: %w", qerr)
	}
	if storedLen != int64(len(data)) {
		return 0, false, fmt.Errorf(
			"recovery: snapshot divergence shard=%d iid=%d seq=%d stored=%dB file=%dB",
			shardID, instrumentID, snapshotSeq, storedLen, len(data))
	}
	return id, false, nil
}

// --- Service -----------------------------------------------------------------

// Config for the snapshot persistence service.
type Config struct {
	ShardID uint16
	IPCBase string // shm base (ipc.DefaultShmBase convention)
	SnapDir string // engine FileSnapshotSink root for this shard
	// ScanInterval is the snap-dir catch-up sweep period (notification-loss
	// safety net); <=0 defaults to 5s.
	ScanInterval time.Duration
}

// Validate the config — fail closed on anything unusable.
func (c Config) Validate() error {
	if c.IPCBase == "" {
		return fmt.Errorf("recovery: ipc base required")
	}
	if c.SnapDir == "" {
		return fmt.Errorf("recovery: snap dir required")
	}
	return nil
}

// Service consumes SnapReadyMsg descriptors, persists snapshots, and emits
// SnapAckMsg confirmations. Construct via Open (rings) or New (injected
// rings, for tests).
type Service struct {
	cfg   Config
	store SnapshotStore
	log   *slog.Logger

	ready *ipc.Ring // consumer: core -> Go
	ack   *ipc.Ring // producer: Go -> core

	// seen keys are canonical rel paths ("i{iid}/snap_{seq}.bin") already
	// persisted+acked this process lifetime — suppresses re-reads and
	// re-acks on every scan tick (upsert is still idempotent across
	// restarts; the engine dedups re-acked seqs via max-confirmed).
	seen map[string]struct{}

	persisted    atomic.Uint64
	acked        atomic.Uint64
	malformed    atomic.Uint64
	verifyFailed atomic.Uint64
	storeFailed  atomic.Uint64
	ackDropped   atomic.Uint64
	scanPersist  atomic.Uint64
}

// New wires the service with already-open rings (ready=consumer,
// ack=producer). Production path is Open; tests inject rings directly.
func New(cfg Config, store SnapshotStore, ready, ack *ipc.Ring,
	log *slog.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("recovery: snapshot store must not be nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, store: store, ready: ready, ack: ack,
			log: log, seen: map[string]struct{}{}},
		nil
}

// Open maps the ring endpoints (attach-or-create — advisory create lets the
// service boot before or after the engine) and returns a ready Service.
func Open(cfg Config, store SnapshotStore, log *slog.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ready, err := ipc.OpenRing(ReadyRingName(cfg.IPCBase, cfg.ShardID),
		ipc.RoleConsumer, true, ringCapacity, ringSlotPayload)
	if err != nil {
		return nil, fmt.Errorf("recovery: open ready ring: %w", err)
	}
	ack, err := ipc.OpenRing(AckRingName(cfg.IPCBase, cfg.ShardID),
		ipc.RoleProducer, true, ringCapacity, ringSlotPayload)
	if err != nil {
		_ = ready.Close()
		return nil, fmt.Errorf("recovery: open ack ring: %w", err)
	}
	return New(cfg, store, ready, ack, log)
}

// Close releases both rings.
func (s *Service) Close() error {
	e1, e2 := error(nil), error(nil)
	if s.ready != nil {
		e1 = s.ready.Close()
	}
	if s.ack != nil {
		e2 = s.ack.Close()
	}
	if e1 != nil {
		return e1
	}
	return e2
}

// Metrics is a point-in-time counter snapshot.
type Metrics struct {
	Persisted    uint64
	Acked        uint64
	Malformed    uint64
	VerifyFailed uint64
	StoreFailed  uint64
	AckDropped   uint64
	ScanPersist  uint64
}

// Metrics returns the current counters.
func (s *Service) Metrics() Metrics {
	return Metrics{
		Persisted:    s.persisted.Load(),
		Acked:        s.acked.Load(),
		Malformed:    s.malformed.Load(),
		VerifyFailed: s.verifyFailed.Load(),
		StoreFailed:  s.storeFailed.Load(),
		AckDropped:   s.ackDropped.Load(),
		ScanPersist:  s.scanPersist.Load(),
	}
}

// Run drains the ready ring and sweeps the snap dir until ctx is done.
// Run is fully cold-path: per-iteration it drains all pending descriptors
// then sleeps until the next scan tick (or ctx cancel).
func (s *Service) Run(ctx context.Context) error {
	interval := s.cfg.ScanInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		// Drain before sleeping and after waking — steady state the ring
		// path is the fast notification and the sweep is the backstop.
		s.PumpOnce(ctx)
		if err := s.ScanOnce(ctx); err != nil {
			s.log.Error("recovery: snap dir scan failed", "err", err)
		}
		select {
		case <-ctx.Done():
			s.PumpOnce(ctx) // last drain — don't strand a just-written file
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// PumpOnce drains every pending SnapReadyMsg from the ring. Returns the
// number of descriptors consumed (well- or malformed alike — a malformed
// slot is always consumed so it cannot wedge the SPSC ring).
func (s *Service) PumpOnce(ctx context.Context) int {
	if s.ready == nil {
		return 0
	}
	n := 0
	for {
		p := s.ready.Peek()
		if p == nil {
			return n
		}
		cp := make([]byte, len(p))
		copy(cp, p) // slot aliases ring memory — copy before Consume
		s.ready.Consume()
		n++
		m, ok := ParseReadyMsg(cp)
		if !ok || m.ShardID != s.cfg.ShardID {
			s.malformed.Add(1)
			continue
		}
		if err := s.persistFile(ctx, m.InstrumentID, m.SnapshotSeq,
			&m); err != nil {
			s.log.Error("recovery: snapshot persist failed",
				"iid", m.InstrumentID, "seq", m.SnapshotSeq, "err", err)
			continue // no ack — the dir scan retries
		}
	}
}

// persistFile reads, verifies, stores and acks one snapshot file.
// ready is nil on the dir-scan path (size/CRC then come from the file's
// own SnapFileHeader instead of the descriptor).
func (s *Service) persistFile(ctx context.Context, instrumentID uint32,
	seq uint64, ready *ReadyMsg) error {

	rel := snapRelPath(instrumentID, seq)
	if _, done := s.seen[rel]; done {
		return nil // already persisted+acked this lifetime
	}
	if ready != nil && ready.RelPath != rel {
		s.verifyFailed.Add(1)
		return fmt.Errorf("recovery: rel_path %q != canonical %q",
			ready.RelPath, rel)
	}
	path := filepath.Join(s.cfg.SnapDir, filepath.FromSlash(rel))
	// Path hygiene: rel is canonical by construction, but reject anything
	// escaping the snap root before touching the filesystem.
	clean := filepath.Clean(path)
	root := filepath.Clean(s.cfg.SnapDir)
	if clean != filepath.Join(root, filepath.FromSlash(rel)) {
		s.verifyFailed.Add(1)
		return fmt.Errorf("recovery: path escapes snap root: %q", path)
	}

	// File-internal integrity on BOTH paths — VerifySnapshotFile
	// (snapshot_verify.go, Task 4.3.11) enforces magic/version, canonical
	// filename↔header seq agreement, exact size vs payload_len, payload
	// CRC32C, and the optional SNPT whole-file trailer.
	info, err := VerifySnapshotFile(clean)
	if err != nil {
		s.verifyFailed.Add(1)
		return err
	}
	if info.ShardID != s.cfg.ShardID || info.InstrumentID != instrumentID ||
		info.Seq != seq {
		s.verifyFailed.Add(1)
		return fmt.Errorf(
			"recovery: %s header shard=%d iid=%d seq=%d, want %d/%d/%d",
			clean, info.ShardID, info.InstrumentID, info.Seq,
			s.cfg.ShardID, instrumentID, seq)
	}

	data, err := os.ReadFile(clean)
	if err != nil {
		return fmt.Errorf("recovery: read %s: %w", clean, err)
	}
	if ready != nil {
		// Descriptor verification: size + whole-file CRC32C.
		if uint64(len(data)) != ready.ByteSize {
			s.verifyFailed.Add(1)
			return fmt.Errorf("recovery: %s size %d != descriptor %d",
				clean, len(data), ready.ByteSize)
		}
		if CRC32C(data) != ready.FileCRC32C {
			s.verifyFailed.Add(1)
			return fmt.Errorf("recovery: %s crc mismatch", clean)
		}
	}

	id, inserted, err := s.store.UpsertBookSnapshot(ctx, s.cfg.ShardID,
		instrumentID, seq, data)
	if err != nil {
		s.storeFailed.Add(1)
		return err
	}
	if inserted {
		s.persisted.Add(1)
	}
	s.sendAck(instrumentID, seq, uint64(id), ackPersisted)
	s.seen[rel] = struct{}{}
	return nil
}

// sendAck emits SnapAckMsg on the ack ring. A full/absent ring drops the
// ack — counted, safe: the engine only loses a trim opportunity it retries
// on the next confirmed snapshot.
func (s *Service) sendAck(instrumentID uint32, seq, snapshotID uint64,
	status uint32) {
	if s.ack == nil {
		s.ackDropped.Add(1)
		return
	}
	if !s.ack.TryWrite(EncodeAck(s.cfg.ShardID, instrumentID, seq,
		snapshotID, status)) {
		s.ackDropped.Add(1)
		return
	}
	s.acked.Add(1)
}

var snapDirRe = regexp.MustCompile(`^i(\d+)$`)

// ScanOnce sweeps {SnapDir}/i*/snap_*.bin for snapshots not yet persisted
// (the catch-up path for dropped/missed ready notifications). Each
// candidate goes through the same verify+persist+ack pipeline; store
// idempotency makes re-delivery a cheap no-op.
func (s *Service) ScanOnce(ctx context.Context) error {
	entries, err := os.ReadDir(s.cfg.SnapDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no snapshots yet — not an error
		}
		return fmt.Errorf("recovery: read snap dir: %w", err)
	}
	for _, e := range entries {
		m := snapDirRe.FindStringSubmatch(e.Name())
		if !e.IsDir() || m == nil {
			continue
		}
		iid, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		sub := filepath.Join(s.cfg.SnapDir, e.Name())
		files, ferr := os.ReadDir(sub)
		if ferr != nil {
			return fmt.Errorf("recovery: read %s: %w", sub, ferr)
		}
		for _, f := range files {
			// orchSnapNameRe (snapshot_verify.go): canonical snap_*.bin.
			fm := orchSnapNameRe.FindStringSubmatch(f.Name())
			if f.IsDir() || fm == nil {
				continue
			}
			seq, serr := strconv.ParseUint(fm[1], 10, 64)
			if serr != nil {
				continue
			}
			// ready == nil: verification falls back to the file's own
			// SnapFileHeader (magic/shard/instrument/seq/payload CRC).
			before := s.persisted.Load()
			if perr := s.persistFile(ctx, uint32(iid), seq, nil); perr != nil {
				s.log.Warn("recovery: scan persist failed",
					"path", filepath.Join(sub, f.Name()), "err", perr)
				continue
			}
			if s.persisted.Load() > before {
				s.scanPersist.Add(1)
			}
		}
	}
	return nil
}
