// aeronmon.go — Aeron media-driver counters reader + monitor.
// Task 7.3.8, spec §2.3/§2.3.1/§19.3.
//
// The C++ engine does not emit a dedicated stats event, so the real
// observable channel is the media driver's CnC file (cnc.dat) — the same
// memory-mapped counters table `aeron-stat`/`aeron-samples` render. This
// file implements a pure-Go read of that layout (verified against
// core/third_party/aeron/include/c/aeron_cnc_file_descriptor.h and
// concurrent/aeron_counters_manager.h):
//
//	header 128B: cnc_version i32, to_driver_len i32, to_clients_len i32,
//	             counter_metadata_len i32, counter_values_len i32,
//	             error_log_len i32, liveness_timeout i64,
//	             start_timestamp i64, pid i64, file_page_size i32
//	counters metadata @ 128 + to_driver + to_clients:
//	  512B record: state i32 (1=allocated), type_id i32,
//	  deadline_ms i64, key[112], label_len i32, label[380]
//	counters values @ + counter_metadata_len:
//	  128B record: value i64, registration_id i64, owner_id i64,
//	  reference_id i64
//
// Every allocated counter exports as
// aeron_driver_counter{counter_id,type_id,label}; well-known type ids
// (sub-pos=4, pub-pos=12, snd-bpe=13, naks=19/20, client-heartbeat=11)
// additionally feed the derived aeron_subscriber_lag_bytes gauge.
package observability

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

// Aeron CnC layout constants — mirror the vendored headers. Do not change
// without checking the driver's descriptor version.
const (
	cncHeaderLen        = 128 // AERON_CNC_VERSION_AND_META_DATA_LENGTH
	cncMetadataRecLen   = 512 // sizeof(aeron_counter_metadata_descriptor_t)
	cncValueRecLen      = 128 // sizeof(aeron_counter_value_descriptor_t)
	cncRecordAllocated  = 1   // AERON_COUNTER_RECORD_ALLOCATED
	cncKeyOffset        = 16
	cncKeyLen           = 112
	cncLabelLenOffset   = 128
	cncLabelOffset      = 132
	cncLabelMax         = 380
	cncFileName         = "cnc.dat"
	cncVersionMaskMajor = 0xFFFF0000 // semantic major occupies bits 16-31
)

// Well-known counter type ids (aeron_counters.h).
const (
	aeronTypeSystemCounter    = 0  // labels carry names (Bytes sent, NAKs…)
	aeronTypePublisherLimit   = 1  // pub-lmt
	aeronTypeSenderPosition   = 2  // snd-pos
	aeronTypeReceiverHWM      = 3  // rcv-hwm
	aeronTypeSubscriberPos    = 4  // sub-pos
	aeronTypeReceiverPos      = 5  // rcv-pos
	aeronTypeSenderLimit      = 9  // snd-lmt
	aeronTypeClientHeartbeat  = 11 // client-heartbeat (value = ms ts)
	aeronTypePublisherPos     = 12 // pub-pos (sampled)
	aeronTypeSenderBPE        = 13 // snd-bpe (backpressure events)
	aeronTypeSenderNAKsRx     = 19 // snd-naks-received
	aeronTypeReceiverNAKsSent = 20 // rcv-naks-sent
)

// CnCCounter is one allocated counter record.
type CnCCounter struct {
	ID     int32
	TypeID int32
	Value  int64
	Label  string
	Key    []byte // raw 112-byte key for callers decoding channel/stream
}

// StreamPosKey is the decoded aeron_stream_position_counter_key_layout_t
// used by sub-pos/pub-pos/rcv-pos/snd-pos/rcv-hwm/pub-lmt/snd-lmt.
type StreamPosKey struct {
	RegistrationID int64
	SessionID      int32
	StreamID       int32
	Channel        string
}

// ParseStreamPosKey decodes the counter key for position/limit counters.
// Returns false when the key does not fit the layout (bad channel length).
func ParseStreamPosKey(key []byte) (StreamPosKey, bool) {
	var k StreamPosKey
	if len(key) < cncKeyLen {
		return k, false
	}
	k.RegistrationID = int64(binary.LittleEndian.Uint64(key[0:8]))
	k.SessionID = int32(binary.LittleEndian.Uint32(key[8:12]))
	k.StreamID = int32(binary.LittleEndian.Uint32(key[12:16]))
	clen := int(binary.LittleEndian.Uint32(key[16:20]))
	if clen < 0 || clen > cncKeyLen-20 {
		return k, false
	}
	k.Channel = string(key[20 : 20+clen])
	return k, true
}

// ReadCnCCounters parses the driver's cnc.dat in dir and returns every
// allocated counter. The file is read with two ReadAt windows (header,
// then the counters region) — torn reads are possible while the driver
// writes, which is acceptable for a 15s monitoring cadence: counters are
// idempotent values, not a transaction log.
func ReadCnCCounters(dir string) (version int32, counters []CnCCounter, err error) {
	path := filepath.Join(dir, cncFileName)
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, fmt.Errorf("aeron cnc: %w", err)
	}
	defer f.Close()

	var header [cncHeaderLen]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return 0, nil, fmt.Errorf("aeron cnc: header read: %w", err)
	}
	version = int32(binary.LittleEndian.Uint32(header[0:4]))
	if version <= 0 {
		return version, nil, fmt.Errorf("aeron cnc: driver not ready (version %d)", version)
	}
	toDriver := binary.LittleEndian.Uint32(header[4:8])
	toClients := binary.LittleEndian.Uint32(header[8:12])
	metaLen := binary.LittleEndian.Uint32(header[12:16])
	valLen := binary.LittleEndian.Uint32(header[16:20])
	if metaLen%cncMetadataRecLen != 0 || valLen%cncValueRecLen != 0 {
		return version, nil, fmt.Errorf("aeron cnc: corrupt buffer lengths meta=%d values=%d", metaLen, valLen)
	}

	metaOff := int64(cncHeaderLen) + int64(toDriver) + int64(toClients)
	valOff := metaOff + int64(metaLen)

	meta := make([]byte, metaLen)
	if _, err := f.ReadAt(meta, metaOff); err != nil {
		return version, nil, fmt.Errorf("aeron cnc: metadata read: %w", err)
	}
	vals := make([]byte, valLen)
	if _, err := f.ReadAt(vals, valOff); err != nil {
		return version, nil, fmt.Errorf("aeron cnc: values read: %w", err)
	}

	nMeta := int(metaLen) / cncMetadataRecLen
	nVals := int(valLen) / cncValueRecLen
	for i := 0; i < nMeta && i < nVals; i++ {
		rec := meta[i*cncMetadataRecLen:]
		state := int32(binary.LittleEndian.Uint32(rec[0:4]))
		if state != cncRecordAllocated {
			continue
		}
		labelLen := int(binary.LittleEndian.Uint32(rec[cncLabelLenOffset : cncLabelLenOffset+4]))
		if labelLen < 0 || labelLen > cncLabelMax {
			labelLen = 0 // torn record — still emit the value
		}
		vrec := vals[i*cncValueRecLen:]
		counters = append(counters, CnCCounter{
			ID:     int32(i),
			TypeID: int32(binary.LittleEndian.Uint32(rec[4:8])),
			Value:  int64(binary.LittleEndian.Uint64(vrec[0:8])),
			Label:  sanitizeLabel(string(rec[cncLabelOffset : cncLabelOffset+labelLen])),
			Key:    append([]byte(nil), rec[cncKeyOffset:cncKeyOffset+cncKeyLen]...),
		})
	}
	return version, counters, nil
}

// sanitizeLabel keeps counter labels printable and single-line for the
// exposition format.
func sanitizeLabel(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			out = append(out, '?')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// SubscriberLag pairs pub-pos (sampled, type 12) with sub-pos (type 4) on
// the same (channel, stream_id) and returns pub−sub per pair. A positive
// value means the subscriber is behind the publisher by that many bytes.
func SubscriberLag(counters []CnCCounter) map[string]int64 {
	type pairKey struct{ channel, stream string }
	pubs := map[pairKey]int64{}
	subs := map[pairKey]int64{}
	for _, c := range counters {
		if c.TypeID != aeronTypePublisherPos && c.TypeID != aeronTypeSubscriberPos {
			continue
		}
		k, ok := ParseStreamPosKey(c.Key)
		if !ok {
			continue
		}
		pk := pairKey{channel: k.Channel, stream: strconv.Itoa(int(k.StreamID))}
		if c.TypeID == aeronTypePublisherPos {
			if v := pubs[pk]; c.Value > v {
				pubs[pk] = c.Value // max publisher position
			}
		} else {
			if v, seen := subs[pk]; !seen || c.Value < v {
				subs[pk] = c.Value // min subscriber position (worst lag)
			}
		}
	}
	out := map[string]int64{}
	for pk, pub := range pubs {
		if sub, ok := subs[pk]; ok {
			out[pk.channel+"|"+pk.stream] = pub - sub
		}
	}
	return out
}

// CnCMonitor samples the media-driver counters file on an interval and
// mirrors them into the registry.
type CnCMonitor struct {
	Dir      string
	Interval time.Duration
	Log      *slog.Logger
	// MaxCounters caps exported series (cardinality budget, spec §19.14).
	// Counters beyond the cap are dropped after deterministic ordering.
	MaxCounters int

	reg      *Registry
	up       *GaugeVec
	version  *GaugeVec
	counter  *GaugeVec
	lag      *GaugeVec
	exported *GaugeVec
}

// NewCnCMonitor builds the monitor; interval<=0 defaults to 15s (the
// Task 7.3.4 scrape cadence), dir "" falls back to $AERON_DIR then the
// driver default /dev/shm/aeron-<user>.
func NewCnCMonitor(reg *Registry, dir string, log *slog.Logger) *CnCMonitor {
	if dir == "" {
		dir = os.Getenv("AERON_DIR")
	}
	if dir == "" {
		if u, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join("/dev/shm", "aeron-"+filepath.Base(u))
		} else {
			dir = "/dev/shm/aeron"
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &CnCMonitor{
		Dir:         dir,
		Interval:    15 * time.Second,
		Log:         log,
		MaxCounters: 2000,
		reg:         reg,
		up: reg.Gauge("aeron_driver_up",
			"1 when the media driver's CnC file was readable on the last sample."),
		version: reg.Gauge("aeron_cnc_version",
			"CnC file semantic version observed on the last sample."),
		counter: reg.Gauge("aeron_driver_counter",
			"Media-driver counter value by counter_id/type_id/label."),
		lag: reg.Gauge("aeron_subscriber_lag_bytes",
			"Publisher minus slowest subscriber position per channel|stream (bytes)."),
		exported: reg.Gauge("aeron_driver_counters_exported",
			"Number of driver counters mirrored on the last sample."),
	}
}

// SampleOnce reads the CnC file and updates all gauges. Exported for the
// Run loop and unit tests.
func (m *CnCMonitor) SampleOnce() error {
	version, counters, err := ReadCnCCounters(m.Dir)
	if err != nil {
		m.up.With().Set(0)
		return err
	}
	m.up.With().Set(1)
	m.version.With().Set(float64(version))
	sort.Slice(counters, func(i, j int) bool { return counters[i].ID < counters[j].ID })
	n := 0
	for _, c := range counters {
		if n >= m.MaxCounters {
			break
		}
		m.counter.With(
			"counter_id", strconv.Itoa(int(c.ID)),
			"type_id", strconv.Itoa(int(c.TypeID)),
			"label", c.Label,
		).Set(float64(c.Value))
		n++
	}
	m.exported.With().Set(float64(n))
	for k, lag := range SubscriberLag(counters) {
		sep := 0
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '|' {
				sep = i
				break
			}
		}
		channel, stream := k[:sep], k[sep+1:]
		m.lag.With("channel", channel, "stream_id", stream).Set(float64(lag))
	}
	return nil
}

// SubscriberLagValue returns the worst observed subscriber lag in bytes —
// the Task 7.3.8 alert source (lag > 1000 → P2).
func (m *CnCMonitor) SubscriberLagValue() float64 {
	version, counters, err := ReadCnCCounters(m.Dir)
	if err != nil || version <= 0 {
		return -1 // unreadable — treat as no data, not zero lag
	}
	var worst int64
	for _, lag := range SubscriberLag(counters) {
		if lag > worst {
			worst = lag
		}
	}
	return float64(worst)
}

// Run samples until ctx is cancelled; read errors are logged at debug
// (a missing driver is a normal condition on hosts without the engine).
func (m *CnCMonitor) Run(ctx context.Context) {
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		if err := m.SampleOnce(); err != nil {
			m.Log.Debug("aeron cnc sample failed", "dir", m.Dir, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
