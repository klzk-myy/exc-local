package recovery

// waldir.go — Task 4.3.9 offline WAL directory primitives for the
// wal-recovery CLI. Mirrors RecoveryManager's Phase-1 prescan in
// core/src/recovery/RecoveryManager.cpp exactly: same {seq}.wal segment
// enumeration, the same CRC32C scan ladder (ScanSegment above), the same
// lost-range / snapshot-covered-gap semantics, and the same level-2 rebase
// marker ({snapshot_seq}.wal holding one BOOK_SNAPSHOT anchor entry).
//
// Every exported symbol is Rec-prefixed — this package is concurrently
// owned by the archive/replay/orchestrator files (Orch* namespace); the
// prefix keeps the namespaces disjoint per the task contract.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RecLostRange is one span of seqs no readable segment can provide — a
// plain gap (trimmed/missing segment) or the unreadable tail of a corrupt
// sealed segment. Recoverable iff End <= every bound book's snapshot_seq.
type RecLostRange struct {
	Begin         uint64 `json:"begin"`
	End           uint64 `json:"end"` // exclusive
	CorruptCaused bool   `json:"corrupt_caused"`
}

// RecSegmentInfo mirrors the C++ SegmentInfo: scan facts for one segment.
type RecSegmentInfo struct {
	Path          string `json:"path"`
	Base          uint64 `json:"base"` // filename stem; ^uint64(0) if unparseable
	Shard         uint16 `json:"shard"`
	HeaderOK      bool   `json:"header_ok"`
	HasEntries    bool   `json:"has_entries"`
	Entries       uint64 `json:"entries"`
	FirstSeq      uint64 `json:"first_seq"`
	LastSeq       uint64 `json:"last_seq"`
	ValidEnd      uint64 `json:"valid_end"`
	Corrupt       bool   `json:"corrupt"`
	CorruptOffset uint64 `json:"corrupt_offset"`
	Size          int64  `json:"size"`
}

// RecWalPrescan is the whole-directory verdict — the level-1/2 inputs.
type RecWalPrescan struct {
	Dir          string           `json:"dir"`
	Segments     []RecSegmentInfo `json:"segments"`
	HasEntries   bool             `json:"has_entries"`
	StreamBase   uint64           `json:"stream_base"`
	WalTail      uint64           `json:"wal_tail"` // last valid seq + 1; 0 = empty stream
	WalEntries   uint64           `json:"wal_entries"`
	LastValidSeq int64            `json:"last_valid_seq"` // -1 = none
	TailCorrupt  bool             `json:"tail_corrupt"`
	TailPath     string           `json:"tail_path,omitempty"`
	TailValidEnd uint64           `json:"tail_valid_end,omitempty"`
	SealedDamage bool             `json:"sealed_damage"` // corrupt non-tail segment(s)
	LostRanges   []RecLostRange   `json:"lost_ranges,omitempty"`
	Divergence   string           `json:"divergence"` // "" | "seq_gap" | "overlap"
}

// RecScanWalDir enumerates {dir}/*.wal, scans each segment read-only and
// orders them by first entry seq — the exact prescan the C++ ladder runs.
// wantShard < 0 accepts any shard id; >= 0 enforces the header's shard.
// Fail-closed: a bad file header or foreign shard is an error, never a
// skipped file.
func RecScanWalDir(dir string, wantShard int) (*RecWalPrescan, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal dir %s: %w", dir, err)
	}
	p := &RecWalPrescan{Dir: dir, LastValidSeq: -1}
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".wal") {
			continue
		}
		path := filepath.Join(dir, de.Name())
		si := RecSegmentInfo{Path: path, Base: ^uint64(0)}
		stem := strings.TrimSuffix(de.Name(), ".wal")
		if b, perr := strconv.ParseUint(stem, 10, 64); perr == nil {
			si.Base = b
		}
		st, serr := os.Stat(path)
		if serr != nil {
			return nil, fmt.Errorf("wal segment %s: %w", path, serr)
		}
		si.Size = st.Size()
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("wal segment %s: %w", path, rerr)
		}
		scan, entries, serr := ScanSegment(data)
		if serr != nil {
			// Short file (< 8-byte frame) or bad magic/version — the same
			// WalOpenFailed surface as the C++ prescan.
			return nil, fmt.Errorf("wal segment %s: %w", path, serr)
		}
		si.HeaderOK = scan.HeaderOK
		si.Shard = scan.Shard
		si.Entries = scan.Entries
		si.HasEntries = scan.Entries > 0
		if si.HasEntries {
			si.FirstSeq = entries[0].Seq
			si.LastSeq = entries[len(entries)-1].Seq
		}
		si.ValidEnd = scan.ValidEnd
		si.Corrupt = scan.Corrupt
		si.CorruptOffset = scan.CorruptOffset
		if wantShard >= 0 && si.Shard != uint16(wantShard) {
			return nil, fmt.Errorf("wal segment %s: shard %d != want %d",
				path, si.Shard, wantShard)
		}
		p.Segments = append(p.Segments, si)
	}
	// Order by first entry seq; empties carry no seqs — sort last (same as
	// the C++ comparator).
	sort.Slice(p.Segments, func(i, j int) bool {
		a, b := p.Segments[i], p.Segments[j]
		if a.HasEntries != b.HasEntries {
			return a.HasEntries
		}
		if a.HasEntries && b.HasEntries && a.FirstSeq != b.FirstSeq {
			return a.FirstSeq < b.FirstSeq
		}
		return a.Path < b.Path
	})

	// Stream accounting + lost ranges (prescan_tail == last_seq + 1).
	var tail uint64
	started := false
	lastIdx := -1
	for i, si := range p.Segments {
		if !si.HasEntries {
			continue
		}
		if !started {
			started = true
			p.StreamBase = si.FirstSeq
			tail = si.FirstSeq
		}
		if si.FirstSeq > tail {
			// Plain gap OR the lost tail of a corrupt predecessor.
			corruptCaused := false
			for j := i - 1; j >= 0; j-- {
				if p.Segments[j].HasEntries {
					corruptCaused = p.Segments[j].Corrupt
					break
				}
			}
			p.LostRanges = append(p.LostRanges,
				RecLostRange{Begin: tail, End: si.FirstSeq,
					CorruptCaused: corruptCaused})
			if p.Divergence == "" {
				p.Divergence = "seq_gap"
			}
		} else if si.FirstSeq < tail {
			p.Divergence = "overlap" // divergent journal copies
		}
		tail = si.LastSeq + 1
		p.WalEntries += si.Entries
		lastIdx = i
	}
	p.HasEntries = started
	if started {
		p.WalTail = tail
		p.LastValidSeq = int64(tail - 1)
	}

	// Tail = the has-entries segment with the greatest seq span.
	if lastIdx >= 0 && p.Segments[lastIdx].Corrupt {
		p.TailCorrupt = true
		p.TailPath = p.Segments[lastIdx].Path
		p.TailValidEnd = p.Segments[lastIdx].ValidEnd
	}
	for i, si := range p.Segments {
		if i != lastIdx && si.Corrupt {
			p.SealedDamage = true
		}
	}
	return p, nil
}

// fsyncParentDirDurably fsyncs a file's directory so a create/rename/
// truncate survives a crash — same durability contract as the C++ emitter.
func fsyncParentDirDurably(path string) error {
	d := filepath.Dir(path)
	fd, err := os.Open(d)
	if err != nil {
		return err
	}
	defer func() { _ = fd.Close() }()
	return fd.Sync()
}

// RecRepairTail performs level-1 torn-tail repair: truncate the segment at
// the scanner's valid_end (the offset after the last CRC-valid record),
// fsync file + parent dir. No-op (returns false) when the file is already
// exactly that size.
func RecRepairTail(path string, validEnd uint64) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("repair %s: %w", path, err)
	}
	if uint64(st.Size()) == validEnd {
		return false, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false, fmt.Errorf("repair %s: %w", path, err)
	}
	if err := f.Truncate(int64(validEnd)); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("repair %s: truncate: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("repair %s: fsync: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("repair %s: close: %w", path, err)
	}
	if err := fsyncParentDirDurably(path); err != nil {
		return true, fmt.Errorf("repair %s: dir fsync: %w", path, err)
	}
	return true, nil
}

// RecRebaseMarkerPayload encodes the BOOK_SNAPSHOT marker payload — the
// WalBookSnapshotHeader layout from core/include/wal/WalEntry.hpp
// (u32 instrument_id, u32 level_count, u64 order_count, u64 book_seq).
// instrument_id 0 + zero counts mark it as a shard-level rebase anchor.
func RecRebaseMarkerPayload(seq uint64) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint32(p[0:4], 0)
	binary.LittleEndian.PutUint32(p[4:8], 0)
	binary.LittleEndian.PutUint64(p[8:16], 0)
	binary.LittleEndian.PutUint64(p[16:24], seq)
	return p
}

// RecWriteRebaseMarker writes the level-2 rebase segment {seq}.wal holding
// one BOOK_SNAPSHOT marker entry at seq — identical bytes/semantics to the
// C++ write_rebase_marker(): the filename stem is the resume floor and the
// marker anchors wal_tail = seq+1 so the seq space resumes past snapshot
// coverage. Idempotent (O_EXCL — an existing file is left untouched).
func RecWriteRebaseMarker(dir string, shard uint16, seq uint64) (string, bool, error) {
	path := filepath.Join(dir, strconv.FormatUint(seq, 10)+".wal")
	if _, err := os.Stat(path); err == nil {
		return path, false, nil // already present
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return path, false, nil
		}
		return path, false, fmt.Errorf("rebase marker %s: %w", path, err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(FileHeader(shard)); err != nil {
		return path, false, fmt.Errorf("rebase marker %s: header: %w", path, err)
	}
	entry := EncodeEntry(seq, uint64(time.Now().UnixNano()),
		EvBookSnapshot, RecRebaseMarkerPayload(seq))
	if _, err := f.Write(entry); err != nil {
		return path, false, fmt.Errorf("rebase marker %s: entry: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return path, false, fmt.Errorf("rebase marker %s: fsync: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return path, false, fmt.Errorf("rebase marker %s: close: %w", path, err)
	}
	ok = true
	if err := fsyncParentDirDurably(path); err != nil {
		return path, true, fmt.Errorf("rebase marker %s: dir fsync: %w", path, err)
	}
	return path, true, nil
}

// RecLatestVerifiedSnapshot returns the newest verified snapshot under the
// sink layout {root}/i{instrument}/snap_{seq:020}.bin — with the same
// generation-fallback contract as FileSnapshotSink (newest, then prior
// retained generation on integrity failure). fellBack=true means the
// newest file failed verification and the prior generation was used.
// (nil, false, nil) on a clean cold start.
func RecLatestVerifiedSnapshot(root string, instrument uint32) (*OrchSnapshotInfo, bool, error) {
	dir := filepath.Join(root, fmt.Sprintf("i%d", instrument))
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("snapshot dir %s: %w", dir, err)
	}
	var paths []string
	for _, de := range ents {
		if de.IsDir() || !orchSnapNameRe.MatchString(de.Name()) {
			continue // tmp/partial files never shadow a canonical name
		}
		paths = append(paths, filepath.Join(dir, de.Name()))
	}
	if len(paths) == 0 {
		return nil, false, nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths))) // %020d zero-pads: lexical == seq order
	var firstErr error
	for i, p := range paths {
		if i > 1 {
			break // retention contract: latest + one prior generation only
		}
		info, verr := VerifySnapshotFile(p)
		if verr == nil {
			return info, i > 0, nil
		}
		if firstErr == nil {
			firstErr = verr
		}
	}
	return nil, false, firstErr
}

// JournaledTrade is one TRADE entry recovered from an engine WAL —
// the authoritative journal record of a fill whose out-ring emission
// may never have reached consumers (the emit→commit loss window: shm
// wipe, unread-ring rebuild, pre-consumer outage). The wire fields are
// already 1e8-scaled — the same fixed point the TradeFill frame carries.
type JournaledTrade struct {
	TradePayload
	Shard uint16 // segment-header shard (an explicit EXC_WAL_DIRS list can hold foreign shards)
	Seq   uint64 // journal sequence — per-shard monotone
	TsNs  uint64 // journal timestamp
}

// ScanTrades decodes every TRADE entry journaled in dir's *.wal
// segments, in journal order (numeric {seq}.wal stem). A corrupt or
// unreadable segment fails closed — a partial journal must never
// masquerade as complete coverage (same posture as the recon WAL scan).
func ScanTrades(dir string) ([]JournaledTrade, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal dir %s: %w", dir, err)
	}
	type segFile struct {
		path string
		base uint64
	}
	var files []segFile
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".wal") {
			continue
		}
		base := ^uint64(0) // unparseable stems sort last
		if b, perr := strconv.ParseUint(strings.TrimSuffix(de.Name(), ".wal"), 10, 64); perr == nil {
			base = b
		}
		files = append(files, segFile{filepath.Join(dir, de.Name()), base})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].base < files[j].base })

	var out []JournaledTrade
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			return nil, fmt.Errorf("wal segment %s: %w", f.path, err)
		}
		scan, entries, err := ScanSegment(data)
		if err != nil {
			return nil, fmt.Errorf("wal segment %s: %w", f.path, err)
		}
		if scan.Corrupt {
			return nil, fmt.Errorf("wal segment %s corrupt at offset %d", f.path, scan.CorruptOffset)
		}
		for _, e := range entries {
			if e.Type != EvTrade {
				continue
			}
			p, err := DecodeTrade(e.Payload)
			if err != nil {
				return nil, fmt.Errorf("wal segment %s seq %d: %w", f.path, e.Seq, err)
			}
			out = append(out, JournaledTrade{
				TradePayload: p, Shard: scan.Shard, Seq: e.Seq, TsNs: e.TimestampNs,
			})
		}
	}
	return out, nil
}
