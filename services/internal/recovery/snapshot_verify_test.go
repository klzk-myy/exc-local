package recovery

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// orchWriteSnap builds a snap_{seq:020}.bin in dir with the given payload
// and (optionally) a valid SNPT trailer. Corruption variants are produced
// by the caller mutating header fields before write — see helpers below.
func orchWriteSnap(t *testing.T, dir string, shard uint16, seq uint64,
	instrument uint32, payload []byte, trailer bool) string {
	t.Helper()
	var hdr [orchSnapFileHeaderLen]byte
	binary.LittleEndian.PutUint32(hdr[0:4], orchSnapFileMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], orchSnapFileVersion)
	binary.LittleEndian.PutUint16(hdr[6:8], shard)
	binary.LittleEndian.PutUint64(hdr[8:16], seq)
	binary.LittleEndian.PutUint32(hdr[16:20], instrument)
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[24:28], crc32.Checksum(payload, orchCRC32CTable))

	body := append(append([]byte{}, hdr[:]...), payload...)
	if trailer {
		var tl [orchSnapTrailerLen]byte
		binary.LittleEndian.PutUint32(tl[0:4], orchSnapTrailerMagic)
		binary.LittleEndian.PutUint32(tl[4:8], crc32.Checksum(body, orchCRC32CTable))
		body = append(body, tl[:]...)
	}
	name := filepath.Join(dir, snapName(seq))
	if err := os.WriteFile(name, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func snapName(seq uint64) string {
	s := "00000000000000000000" + itoa(seq)
	return "snap_" + s[len(s)-20:] + ".bin"
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func TestVerifySnapshotFileValid(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("fake-book-blob")
	path := orchWriteSnap(t, dir, 2, 12345, 7, payload, false)

	info, err := VerifySnapshotFile(path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.ShardID != 2 || info.Seq != 12345 || info.InstrumentID != 7 {
		t.Fatalf("bad header parse: %+v", info)
	}
	if info.HasTrailer {
		t.Fatal("no trailer written but HasTrailer set")
	}
	if !info.FilenameOK || info.FilenameSeq != 12345 {
		t.Fatal("filename seq not verified")
	}
}

func TestVerifySnapshotFileWithTrailer(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("blob")
	path := orchWriteSnap(t, dir, 0, 99, 3, payload, true)

	info, err := VerifySnapshotFile(path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !info.HasTrailer {
		t.Fatal("trailer not detected")
	}
}

func TestVerifySnapshotFileCorruptPayload(t *testing.T) {
	dir := t.TempDir()
	path := orchWriteSnap(t, dir, 0, 42, 1, []byte("payload-bytes"), false)
	raw, _ := os.ReadFile(path)
	raw[orchSnapFileHeaderLen+2] ^= 0xFF // flip a payload bit
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySnapshotFile(path); err == nil ||
		!strings.Contains(err.Error(), "payload crc") {
		t.Fatalf("corrupt payload accepted: %v", err)
	}
}

func TestVerifySnapshotFileTornAndForeign(t *testing.T) {
	dir := t.TempDir()
	// Truncated file.
	p := filepath.Join(dir, snapName(7))
	if err := os.WriteFile(p, []byte{0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySnapshotFile(p); err == nil {
		t.Fatal("torn file accepted")
	}
	// Bad magic.
	p2 := orchWriteSnap(t, dir, 0, 8, 1, []byte("x"), false)
	raw, _ := os.ReadFile(p2)
	binary.LittleEndian.PutUint32(raw[0:4], 0xDEADBEEF)
	if err := os.WriteFile(p2, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySnapshotFile(p2); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("bad magic accepted: %v", err)
	}
	// Trailing junk (no valid trailer) — the C++ loader rejects this too.
	p3 := orchWriteSnap(t, dir, 0, 9, 1, []byte("x"), false)
	f, _ := os.OpenFile(p3, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write([]byte("junk"))
	_ = f.Close()
	if _, err := VerifySnapshotFile(p3); err == nil {
		t.Fatal("trailing junk accepted")
	}
	// Filename seq != header seq.
	p4 := orchWriteSnap(t, dir, 0, 11, 1, []byte("x"), false)
	renamed := filepath.Join(dir, snapName(12))
	if err := os.Rename(p4, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySnapshotFile(renamed); err == nil ||
		!strings.Contains(err.Error(), "filename seq") {
		t.Fatalf("filename/header divergence accepted: %v", err)
	}
}

// TestVerifyRealRepoSnapshot exercises the verifier against the C++
// FileSnapshotSink's real on-disk output when the repo's snapshots/
// directory is present (spec-level check against current framing: no
// trailer, header payload_crc + exact size).
func TestVerifyRealRepoSnapshot(t *testing.T) {
	root := filepath.Join("..", "..", "..", "snapshots")
	if _, err := os.Stat(root); err != nil {
		t.Skip("repo snapshots/ not present")
	}
	var tested int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasPrefix(d.Name(), "snap_") || !strings.HasSuffix(d.Name(), ".bin") {
			return nil
		}
		info, verr := VerifySnapshotFile(path)
		if verr != nil {
			t.Fatalf("real snapshot %s rejected: %v", path, verr)
		}
		if info.HasTrailer {
			t.Logf("%s: trailer present (concurrent C++ trailer work landed)", path)
		}
		tested++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tested == 0 {
		t.Skip("no snap_*.bin under snapshots/")
	}
	t.Logf("verified %d real snapshot files", tested)
}
