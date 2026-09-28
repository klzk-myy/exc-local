// Task 4.3.11 — snapshot checksum verification (spec §18.6.7, §24 #353).
//
// Go-side verifier for the Phase-02 FileSnapshotSink on-disk format
// (core/src/recovery/SnapshotStore.cpp — read-only reference; the C++
// writer is owned by another workstream):
//
//	[SnapFileHeader 32B packed LE][payload = serialized book blob]
//
//	SnapFileHeader: magic u32 'SNAP' (0x50414E53) | version u16 = 1 |
//	shard_id u16 | seq u64 (WAL cursor covered) | instrument_id u32 |
//	payload_len u32 | payload_crc u32 (CRC32C over payload) | _pad u32
//
// The C++ store currently writes NO trailer — load_latest() enforces the
// exact-size check file_size == 32 + payload_len and rejects any trailing
// bytes. Task 4.3.11 adds a CRC32C *file trailer*; this verifier consumes
// the self-describing trailer form:
//
//	[hdr][payload][trailer 8B = magic u32 'SNPT' + crc u32]
//
// where the trailer CRC32C covers every byte before the trailer
// (header + payload). Files without a trailer keep passing on the
// payload-CRC + exact-size contract; files with a well-formed SNPT
// trailer get the additional whole-file integrity proof; any other
// trailing bytes are corruption under both contracts and are rejected.
//
// HANDOFF: if the concurrent C++ change picks a different trailer
// framing, update orchSnapTrailerLen/orchTrailerCRCOffset here — the
// verifier is the single point that needs to track it.
package recovery

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"regexp"
	"strconv"
)

// On-disk constants mirrored from SnapshotStore.hpp (packed, little-endian).
const (
	orchSnapFileHeaderLen = 32
	orchSnapFileMagic     = 0x50414E53 // 'SNAP'
	orchSnapFileVersion   = 1
	orchSnapExtMagic      = 0x31455853 // 'SXE1' (payload-internal; not checked here)

	// Self-describing file trailer (Task 4.3.11): magic + CRC32C of all
	// preceding bytes. See package comment for the handoff note.
	orchSnapTrailerLen   = 8
	orchSnapTrailerMagic = 0x54504E53 // 'SNPT'
)

var orchCRC32CTable = crc32.MakeTable(crc32.Castagnoli)

// OrchSnapshotInfo describes a verified snapshot file.
type OrchSnapshotInfo struct {
	Path         string
	ShardID      uint16
	Seq          uint64 // WAL cursor covered (== WalBookSnapshotHeader.book_seq)
	InstrumentID uint32
	PayloadLen   uint32
	PayloadCRC   uint32
	HasTrailer   bool   // SNPT file trailer present and verified
	TrailerCRC   uint32 // whole-file CRC32C when HasTrailer
	FilenameSeq  uint64 // seq parsed from the snap_%020d.bin name
	FilenameOK   bool   // name matched the canonical pattern AND seq agreed
}

var orchSnapNameRe = regexp.MustCompile(`^snap_(\d{20})\.bin$`)

// VerifySnapshotFile validates a snapshot file before the loader touches
// it: canonical filename ↔ header seq agreement, magic/version, exact-size
// check, payload CRC32C, and — when present — the SNPT whole-file trailer.
// Fail-closed: any anomaly is an error; there is no "best effort" mode.
func VerifySnapshotFile(path string) (*OrchSnapshotInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot verify %s: %w", path, err)
	}
	size := st.Size()
	if size < orchSnapFileHeaderLen {
		return nil, fmt.Errorf("snapshot verify %s: size %d < header %d (torn)", path, size, orchSnapFileHeaderLen)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot verify %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var hdr [orchSnapFileHeaderLen]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, fmt.Errorf("snapshot verify %s: header read: %w", path, err)
	}
	info := &OrchSnapshotInfo{
		Path:         path,
		ShardID:      binary.LittleEndian.Uint16(hdr[6:8]),
		Seq:          binary.LittleEndian.Uint64(hdr[8:16]),
		InstrumentID: binary.LittleEndian.Uint32(hdr[16:20]),
		PayloadLen:   binary.LittleEndian.Uint32(hdr[20:24]),
		PayloadCRC:   binary.LittleEndian.Uint32(hdr[24:28]),
	}
	if magic := binary.LittleEndian.Uint32(hdr[0:4]); magic != orchSnapFileMagic {
		return nil, fmt.Errorf("snapshot verify %s: bad magic 0x%08x (want 0x%08x)", path, magic, orchSnapFileMagic)
	}
	if v := binary.LittleEndian.Uint16(hdr[4:6]); v != orchSnapFileVersion {
		return nil, fmt.Errorf("snapshot verify %s: bad version %d (want %d)", path, v, orchSnapFileVersion)
	}

	// Canonical filename ↔ header seq agreement (mirrors the C++ load check
	// fh.seq == best_seq). Non-canonical names are rejected outright — a tmp
	// remnant must never be verified-and-served as a snapshot.
	if m := orchSnapNameRe.FindStringSubmatch(st.Name()); m != nil {
		fseq, _ := strconv.ParseUint(m[1], 10, 64)
		info.FilenameSeq = fseq
		if fseq != info.Seq {
			return nil, fmt.Errorf("snapshot verify %s: filename seq %d != header seq %d", path, fseq, info.Seq)
		}
		info.FilenameOK = true
	} else {
		return nil, fmt.Errorf("snapshot verify %s: non-canonical name %q (want snap_{seq:020}.bin)", path, st.Name())
	}

	// Exact-size contract: either header+payload (no trailer) or
	// header+payload+SNPT trailer. Anything else is torn/corrupt.
	wantNoTrailer := int64(orchSnapFileHeaderLen) + int64(info.PayloadLen)
	var trailer []byte
	switch size {
	case wantNoTrailer:
		// Current format — no trailer.
	case wantNoTrailer + orchSnapTrailerLen:
		trailer = make([]byte, orchSnapTrailerLen)
	default:
		return nil, fmt.Errorf("snapshot verify %s: size %d != %d (+ optional %d-byte trailer)", path, size, wantNoTrailer, orchSnapTrailerLen)
	}

	// Stream the payload through a CRC32C hasher; also accumulate the
	// whole-file CRC (header + payload) for the trailer check.
	payloadCRC := crc32.New(orchCRC32CTable)
	fileCRC := crc32.New(orchCRC32CTable)
	_, _ = fileCRC.Write(hdr[:])
	if _, err := io.CopyN(io.MultiWriter(payloadCRC, fileCRC), f, int64(info.PayloadLen)); err != nil {
		return nil, fmt.Errorf("snapshot verify %s: payload read: %w", path, err)
	}
	if got := payloadCRC.Sum32(); got != info.PayloadCRC {
		return nil, fmt.Errorf("snapshot verify %s: payload crc 0x%08x != header 0x%08x", path, got, info.PayloadCRC)
	}

	if trailer != nil {
		if _, err := io.ReadFull(f, trailer); err != nil {
			return nil, fmt.Errorf("snapshot verify %s: trailer read: %w", path, err)
		}
		if magic := binary.LittleEndian.Uint32(trailer[0:4]); magic != orchSnapTrailerMagic {
			return nil, fmt.Errorf("snapshot verify %s: bad trailer magic 0x%08x (want 'SNPT' 0x%08x)", path, magic, orchSnapTrailerMagic)
		}
		stored := binary.LittleEndian.Uint32(trailer[4:8])
		if got := fileCRC.Sum32(); got != stored {
			return nil, fmt.Errorf("snapshot verify %s: file crc 0x%08x != trailer 0x%08x", path, got, stored)
		}
		info.HasTrailer = true
		info.TrailerCRC = stored
	}
	return info, nil
}

// OrchSnapshotVerifyDir is a convenience: verify the newest canonical
// snapshot in dir ({root}/i{instrument}/snap_*.bin naming). Returns
// (nil, nil) on a clean cold start (no snapshots), matching the C++ sink's
// has_snapshot=false contract.
func OrchVerifyLatestSnapshot(dir string) (*OrchSnapshotInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapshot verify dir %s: %w", dir, err)
	}
	var bestSeq uint64
	var best string
	found := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := orchSnapNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue // tmp remnants and foreign files are never candidates
		}
		seq, _ := strconv.ParseUint(m[1], 10, 64)
		if !found || seq > bestSeq {
			found, bestSeq, best = true, seq, e.Name()
		}
	}
	if !found {
		return nil, nil
	}
	return VerifySnapshotFile(dir + string(os.PathSeparator) + best)
}
