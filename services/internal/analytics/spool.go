package analytics

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/shopspring/decimal"
)

// Spool is the §16.6 local-disk ingestion buffer (Task 20.3.11,
// §24 #322): when a ClickHouse bulk insert times out (>5s) or the cluster
// is unreachable, pending insert batches land here instead of
// back-pressuring the JetStream consumers.
//
// DECISION RECORDED (deviation for spec §27): §16.6 names an embedded
// RocksDB queue. This implementation uses github.com/cockroachdb/pebble —
// a pure-Go LSM from the same disk-format family — because the toolchain
// carries no CGO RocksDB binding (grocksdb needs librocksdb ≥6.16 and CGO
// everywhere). Pebble implements the same contract: ordered iteration,
// durable sync writes, embedded, no external process.
//
// Layout: data keys are "D" + %020d unix-nanos + ":" + %010d seq so plain
// byte order equals append order (FIFO drain). Meta keys use an "M"
// prefix (restart-safe ETL cursors — e.g. the income projection's last
// ledger_entries id). Values are gob-encoded spoolBatch{table, rows}.
//
// Cleanup policy (§16.6 F15): MaxBytes caps the spool (default 100GB);
// when an append would exceed it, the OLDEST entries are evicted until it
// fits (drop-oldest, counted via analytics_spool_dropped_total — the
// metrics gauge drives the L1 alert at 80GB).
type Spool struct {
	db       *pebble.DB
	dir      string
	maxBytes uint64
	seq      atomic.Uint64
	entries  atomic.Int64
}

// DefaultSpoolDir is the §16.6/Task-20.3.11 canonical buffer location;
// EXC_CH_SPOOL_DIR overrides it (dev laptops seldom can write /var/spool).
const DefaultSpoolDir = "/var/spool/exchange/clickhouse_buffer"

// DefaultSpoolMaxBytes is the §16.6 F15 cleanup bound (100GB).
const DefaultSpoolMaxBytes = 100 << 30

const (
	spoolDataPrefix = 'D' // data keys: "D%020d:%010d"
	spoolMetaPrefix = 'M' // meta keys: "M<name>"
	spoolMetaCursor = "income_cursor"
)

// spoolBatch is the serialized value: one pending columnar insert.
type spoolBatch struct {
	Table string
	Rows  [][]any
}

// spoolEntry couples a raw key with its decoded batch (key kept verbatim
// so drain deletes exactly what was read — at-least-once).
type spoolEntry struct {
	key   []byte
	batch spoolBatch
}

func init() {
	// gob needs every concrete type that can appear inside []any rows.
	gob.Register(decimal.Decimal{})
	gob.Register(time.Time{})
	gob.Register("")
	gob.Register(uint64(0))
	gob.Register(uint32(0))
	gob.Register(uint8(0))
	gob.Register(int64(0))
	gob.Register(float64(0))
	gob.Register(false)
	gob.Register([]byte{})
}

func dataKey(unixNano int64, seq uint64) []byte {
	return []byte(fmt.Sprintf("%c%020d:%010d", spoolDataPrefix, unixNano, seq))
}

func metaKey(name string) []byte { return append([]byte{spoolMetaPrefix}, name...) }

// OpenSpool opens (or creates) the spool at dir. maxBytes <= 0 applies the
// §16.6 default of 100GB. The pending-entry count is recovered by a key
// scan so gauges stay honest across restarts.
func OpenSpool(dir string, maxBytes uint64) (*Spool, error) {
	if dir == "" {
		dir = DefaultSpoolDir
	}
	if maxBytes <= 0 {
		maxBytes = DefaultSpoolMaxBytes
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("spool: mkdir %s: %w", dir, err)
	}
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("spool: open %s: %w", dir, err)
	}
	s := &Spool{db: db, dir: dir, maxBytes: maxBytes}
	// Recover pending count (dev-scale scan; a 100GB spool scans keys
	// only — acceptable at open, documented).
	it, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{spoolDataPrefix},
		UpperBound: []byte{spoolDataPrefix + 1},
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("spool: scan %s: %w", dir, err)
	}
	var n int64
	var maxSeq uint64
	for it.First(); it.Valid(); it.Next() {
		n++
		// Resume the seq counter past any pre-existing suffix so keys
		// never collide with entries already on disk.
		k := it.Key()
		if len(k) > 22 {
			var v uint64
			if _, err := fmt.Sscanf(string(k[22:]), "%010d", &v); err == nil && v > maxSeq {
				maxSeq = v
			}
		}
	}
	if err := it.Close(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("spool: scan %s: %w", dir, err)
	}
	s.entries.Store(n)
	s.seq.Store(maxSeq)
	return s, nil
}

// Dir reports the spool directory (for logging/metrics).
func (s *Spool) Dir() string { return s.dir }

// Entries reports the number of pending spool entries.
func (s *Spool) Entries() int64 { return s.entries.Load() }

// SizeBytes reports the approximate on-disk footprint (pebble file usage;
// memtable contents surface on the next flush — the §16.6 gauge is a
// monitor, not an exact accounting).
func (s *Spool) SizeBytes() int64 {
	return int64(s.db.Metrics().DiskSpaceUsage())
}

// Append persists one insert batch durably (Sync write). If the append
// would push the spool past maxBytes the oldest entries are evicted
// first — drop-oldest is the §16.6 F15 policy, counted via dropped.
// Returns the number of entries evicted to make room.
func (s *Spool) Append(table string, rows [][]any) (evicted int, err error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(spoolBatch{Table: table, Rows: rows}); err != nil {
		return 0, fmt.Errorf("spool: encode %s batch: %w", table, err)
	}
	key := dataKey(time.Now().UnixNano(), s.seq.Add(1))
	if evicted, err = s.evictFor(uint64(buf.Len())); err != nil {
		return evicted, err
	}
	if err := s.db.Set(key, buf.Bytes(), pebble.Sync); err != nil {
		return evicted, fmt.Errorf("spool: set %s: %w", table, err)
	}
	s.entries.Add(1)
	return evicted, nil
}

// evictFor drops oldest entries until dropping appendBytes more stays
// under maxBytes. Fail-closed: an unreadable iterator aborts the append.
func (s *Spool) evictFor(appendBytes uint64) (int, error) {
	if s.maxBytes == 0 {
		return 0, nil
	}
	used := s.db.Metrics().DiskSpaceUsage()
	if used+appendBytes <= s.maxBytes {
		return 0, nil
	}
	it, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{spoolDataPrefix},
		UpperBound: []byte{spoolDataPrefix + 1},
	})
	if err != nil {
		return 0, fmt.Errorf("spool: evict scan: %w", err)
	}
	defer it.Close()
	var victims [][]byte
	for it.First(); it.Valid(); it.Next() {
		if used+appendBytes <= s.maxBytes {
			break
		}
		k := append([]byte(nil), it.Key()...)
		victims = append(victims, k)
		used -= uint64(len(k) + len(it.Value()))
	}
	if len(victims) == 0 {
		return 0, nil
	}
	b := s.db.NewBatch()
	for _, k := range victims {
		if err := b.Delete(k, nil); err != nil {
			_ = b.Close()
			return 0, fmt.Errorf("spool: evict delete: %w", err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		_ = b.Close()
		return 0, fmt.Errorf("spool: evict commit: %w", err)
	}
	_ = b.Close()
	s.entries.Add(-int64(len(victims)))
	return len(victims), nil
}

// Peek returns up to maxEntries oldest spool entries without deleting
// them (FIFO — data keys order by append time).
func (s *Spool) Peek(maxEntries int) ([]spoolEntry, error) {
	it, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{spoolDataPrefix},
		UpperBound: []byte{spoolDataPrefix + 1},
	})
	if err != nil {
		return nil, fmt.Errorf("spool: peek: %w", err)
	}
	defer it.Close()
	out := make([]spoolEntry, 0, min(maxEntries, 1024))
	for it.First(); it.Valid() && len(out) < maxEntries; it.Next() {
		var b spoolBatch
		if err := gob.NewDecoder(bytes.NewReader(it.Value())).Decode(&b); err != nil {
			return out, fmt.Errorf("spool: decode entry: %w", err)
		}
		out = append(out, spoolEntry{
			key:   append([]byte(nil), it.Key()...),
			batch: b,
		})
	}
	return out, nil
}

// Delete removes acked keys after their ClickHouse insert succeeded —
// the at-least-once boundary (never delete before the insert acks).
func (s *Spool) Delete(keys [][]byte) error {
	if len(keys) == 0 {
		return nil
	}
	b := s.db.NewBatch()
	for _, k := range keys {
		if err := b.Delete(k, nil); err != nil {
			_ = b.Close()
			return fmt.Errorf("spool: delete: %w", err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		_ = b.Close()
		return fmt.Errorf("spool: delete commit: %w", err)
	}
	_ = b.Close()
	s.entries.Add(-int64(len(keys)))
	return nil
}

// SetMeta stores a restart-safe cursor value (meta keyspace is disjoint
// from the FIFO data keyspace).
func (s *Spool) SetMeta(name string, v []byte) error {
	return s.db.Set(metaKey(name), v, pebble.Sync)
}

// GetMeta reads a cursor value; missing returns (nil, nil).
func (s *Spool) GetMeta(name string) ([]byte, error) {
	v, closer, err := s.db.Get(metaKey(name))
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return append([]byte(nil), v...), nil
}

// SetCursor stores the income-projection ledger_entries cursor.
func (s *Spool) SetCursor(id uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return s.SetMeta(spoolMetaCursor, b[:])
}

// Cursor reads the income-projection cursor (0 = start of ledger).
func (s *Spool) Cursor() (uint64, error) {
	v, err := s.GetMeta(spoolMetaCursor)
	if err != nil || len(v) == 0 {
		return 0, err
	}
	return binary.BigEndian.Uint64(v), nil
}

// Close flushes and closes the pebble DB.
func (s *Spool) Close() error { return s.db.Close() }
