// aeronmon_test.go — CnC file parse + subscriber-lag derivation +
// monitor gauge emission against a synthesized cnc.dat.
package observability

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildCnC writes a synthetic cnc.dat implementing the vendored layout:
// 128B header, toDriver, toClients, counters metadata, counters values.
// counters: (typeID, value, label, key)
func buildCnC(t *testing.T, dir string, counters []struct {
	typeID int32
	value  int64
	label  string
	key    []byte
}) {
	t.Helper()
	const toDriver, toClients = 1024, 1024
	nMeta := len(counters) + 2 // include an unused + a reclaimed record
	metaLen := nMeta * cncMetadataRecLen
	valLen := nMeta * cncValueRecLen
	errLen := 4096

	buf := make([]byte, cncHeaderLen+toDriver+toClients+metaLen+valLen+errLen)
	binary.LittleEndian.PutUint32(buf[0:], uint32(512)) // version 0.2.0
	binary.LittleEndian.PutUint32(buf[4:], toDriver)
	binary.LittleEndian.PutUint32(buf[8:], toClients)
	binary.LittleEndian.PutUint32(buf[12:], uint32(metaLen))
	binary.LittleEndian.PutUint32(buf[16:], uint32(valLen))
	binary.LittleEndian.PutUint32(buf[20:], uint32(errLen))
	binary.LittleEndian.PutUint64(buf[24:], 10000)   // liveness timeout ms
	binary.LittleEndian.PutUint64(buf[32:], 1700000) // start ts
	binary.LittleEndian.PutUint64(buf[40:], 4242)    // pid

	metaOff := cncHeaderLen + toDriver + toClients
	valOff := metaOff + metaLen
	// record 0: unused (state 0); last record: reclaimed (state -1)
	binary.LittleEndian.PutUint32(buf[metaOff+nMeta*cncMetadataRecLen-cncMetadataRecLen:],
		uint32(0xFFFFFFFF))
	for i, c := range counters {
		rec := metaOff + (i+1)*cncMetadataRecLen
		binary.LittleEndian.PutUint32(buf[rec:], cncRecordAllocated)
		binary.LittleEndian.PutUint32(buf[rec+4:], uint32(c.typeID))
		if len(c.key) > 0 {
			copy(buf[rec+cncKeyOffset:], c.key)
		}
		binary.LittleEndian.PutUint32(buf[rec+cncLabelLenOffset:], uint32(len(c.label)))
		copy(buf[rec+cncLabelOffset:], c.label)
		binary.LittleEndian.PutUint64(buf[valOff+(i+1)*cncValueRecLen:], uint64(c.value))
	}
	if err := os.WriteFile(filepath.Join(dir, cncFileName), buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// streamPosKey encodes an aeron_stream_position_counter_key_layout_t.
func streamPosKey(regID int64, sessionID, streamID int32, channel string) []byte {
	k := make([]byte, cncKeyLen)
	binary.LittleEndian.PutUint64(k[0:], uint64(regID))
	binary.LittleEndian.PutUint32(k[8:], uint32(sessionID))
	binary.LittleEndian.PutUint32(k[12:], uint32(streamID))
	binary.LittleEndian.PutUint32(k[16:], uint32(len(channel)))
	copy(k[20:], channel)
	return k
}

func TestReadCnCCounters(t *testing.T) {
	dir := t.TempDir()
	buildCnC(t, dir, []struct {
		typeID int32
		value  int64
		label  string
		key    []byte
	}{
		{aeronTypeSystemCounter, 12345, "Bytes sent", nil},
		{aeronTypeReceiverNAKsSent, 7, "rcv-naks-sent 0: aeron:udp?endpoint=1.2.3.4:40456", nil},
		{aeronTypePublisherPos, 8000, "pub-pos (sampled) 9: aeron:ipc?alias=orders_out",
			streamPosKey(9, 42, 1002, "aeron:ipc?alias=orders_out")},
		{aeronTypeSubscriberPos, 6500, "sub-pos 10: aeron:ipc?alias=orders_out",
			streamPosKey(10, 42, 1002, "aeron:ipc?alias=orders_out")},
		{aeronTypeClientHeartbeat, 1700000000000, "client-heartbeat: 12345", nil},
		{aeronTypeSenderBPE, 3, "snd-bpe 0: aeron:udp?x=1",
			streamPosKey(11, 42, 1003, "aeron:udp?x=1")},
	})

	version, counters, err := ReadCnCCounters(dir)
	if err != nil {
		t.Fatal(err)
	}
	if version != 512 {
		t.Fatalf("version = %d want 512", version)
	}
	if len(counters) != 6 {
		t.Fatalf("counters = %d want 6", len(counters))
	}
	byID := map[int32]CnCCounter{}
	for _, c := range counters {
		byID[c.ID] = c
	}
	if byID[1].Value != 12345 || byID[1].Label != "Bytes sent" {
		t.Fatalf("counter 1 = %+v", byID[1])
	}
	if byID[4].TypeID != aeronTypeSubscriberPos || byID[4].Value != 6500 {
		t.Fatalf("counter 4 = %+v", byID[4])
	}

	// Subscriber lag: pub 8000 − sub 6500 = 1500 bytes on the ipc channel.
	lags := SubscriberLag(counters)
	lag, ok := lags["aeron:ipc?alias=orders_out|1002"]
	if !ok || lag != 1500 {
		t.Fatalf("lag = %v (found=%v) lags=%v", lag, ok, lags)
	}
}

func TestCnCMonitorGaugesAndMissing(t *testing.T) {
	// Missing file → aeron_driver_up 0, error returned.
	reg := New()
	mon := NewCnCMonitor(reg, t.TempDir(), nil)
	mon.Interval = time.Second
	if err := mon.SampleOnce(); err == nil {
		t.Fatal("expected error for missing cnc.dat")
	}
	_, samples := parseExposition(t, reg.String())
	if s, ok := findSample(samples, "aeron_driver_up", nil); !ok || s.value != 0 {
		t.Fatalf("aeron_driver_up = %v", s.value)
	}
	if mon.SubscriberLagValue() != -1 {
		t.Fatal("lag should be -1 with no driver")
	}

	// Populated file → up=1, counters mirrored, lag gauge set.
	dir := t.TempDir()
	buildCnC(t, dir, []struct {
		typeID int32
		value  int64
		label  string
		key    []byte
	}{
		{aeronTypeSystemCounter, 1, "NAKs received", nil},
		{aeronTypePublisherPos, 100, "pub-pos (sampled) 1: aeron:ipc?alias=out",
			streamPosKey(1, 7, 1002, "aeron:ipc?alias=out")},
		{aeronTypeSubscriberPos, 40, "sub-pos 2: aeron:ipc?alias=out",
			streamPosKey(2, 7, 1002, "aeron:ipc?alias=out")},
	})
	mon = NewCnCMonitor(reg, dir, nil)
	if err := mon.SampleOnce(); err != nil {
		t.Fatal(err)
	}
	if got := mon.SubscriberLagValue(); got != 60 {
		t.Fatalf("SubscriberLagValue = %v want 60", got)
	}
	_, samples = parseExposition(t, reg.String())
	if s, ok := findSample(samples, "aeron_driver_up", nil); !ok || s.value != 1 {
		t.Fatalf("aeron_driver_up = %v", s.value)
	}
	s, ok := findSample(samples, "aeron_driver_counter", map[string]string{
		"type_id": "0", "label": "NAKs received"})
	if !ok || s.value != 1 {
		t.Fatalf("aeron_driver_counter = %v (found=%v)", s.value, ok)
	}
	s, ok = findSample(samples, "aeron_subscriber_lag_bytes", map[string]string{
		"channel": "aeron:ipc?alias=out", "stream_id": "1002"})
	if !ok || s.value != 60 {
		t.Fatalf("lag gauge = %v (found=%v)", s.value, ok)
	}
}
