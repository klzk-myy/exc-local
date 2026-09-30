package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Store is the incident-ledger seam. Implementations must keep every
// write durable before returning (the post-mortem is built from this
// ledger — losing it is losing the evidence).
type Store interface {
	// NextID allocates the day's next incident number (INC-<day>-NNNN).
	// Must be unique under concurrency — implementations use mkdir/O_EXCL
	// or equivalent; a collision retries, never silently overwrites.
	NextID(ctx context.Context, day string) (string, error)
	Create(ctx context.Context, inc *Incident) error
	Update(ctx context.Context, inc *Incident) error
	Get(ctx context.Context, id string) (*Incident, error)
	List(ctx context.Context) ([]*Incident, error)
	AppendTimeline(ctx context.Context, id string, e TimelineEntry) error
	Timeline(ctx context.Context, id string) ([]TimelineEntry, error)
}

// ---------------------------------------------------------------------------
// MemStore — tests and in-process wiring.
// ---------------------------------------------------------------------------

// MemStore is the in-memory Store — unit tests and single-process dev.
type MemStore struct {
	mu     sync.Mutex
	incs   map[string]*Incident
	order  []string
	seq    map[string]int
	tlines map[string][]TimelineEntry
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		incs:   map[string]*Incident{},
		seq:    map[string]int{},
		tlines: map[string][]TimelineEntry{},
	}
}

// NextID implements Store.
func (m *MemStore) NextID(_ context.Context, day string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq[day]++
	return fmt.Sprintf("INC-%s-%04d", day, m.seq[day]), nil
}

// Create implements Store.
func (m *MemStore) Create(_ context.Context, inc *Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.incs[inc.ID]; dup {
		return fmt.Errorf("incident: duplicate id %s", inc.ID)
	}
	c := *inc
	m.incs[inc.ID] = &c
	m.order = append(m.order, inc.ID)
	return nil
}

// Update implements Store.
func (m *MemStore) Update(_ context.Context, inc *Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.incs[inc.ID]; !ok {
		return fmt.Errorf("incident: %s not found", inc.ID)
	}
	c := *inc
	m.incs[inc.ID] = &c
	return nil
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, id string) (*Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inc, ok := m.incs[id]
	if !ok {
		return nil, fmt.Errorf("incident: %s not found", id)
	}
	c := *inc
	return &c, nil
}

// List implements Store (declaration order).
func (m *MemStore) List(_ context.Context) ([]*Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Incident, 0, len(m.order))
	for _, id := range m.order {
		c := *m.incs[id]
		out = append(out, &c)
	}
	return out, nil
}

// AppendTimeline implements Store.
func (m *MemStore) AppendTimeline(_ context.Context, id string, e TimelineEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.incs[id]; !ok {
		return fmt.Errorf("incident: %s not found", id)
	}
	m.tlines[id] = append(m.tlines[id], e)
	return nil
}

// Timeline implements Store.
func (m *MemStore) Timeline(_ context.Context, id string) ([]TimelineEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]TimelineEntry(nil), m.tlines[id]...)
	return out, nil
}

// ---------------------------------------------------------------------------
// FileStore — the dev/disaster adapter: a real directory tree per incident,
// executable with no external accounts:
//
//	<dir>/inc-20260927-0003/incident.json   (record, rewritten atomically)
//	<dir>/inc-20260927-0003/timeline.jsonl  (append-only ledger)
//
// ID allocation uses mkdir(O_EXCL) semantics — the directory itself is
// the reservation, so concurrent declarers cannot collide.
// ---------------------------------------------------------------------------

// FileStore persists incidents under Root.
type FileStore struct {
	Root string
	mu   sync.Mutex // serializes writers within the process
}

// NewFileStore creates Root if needed and returns the store.
func NewFileStore(root string) (*FileStore, error) {
	if root == "" {
		return nil, fmt.Errorf("incident: file store root required")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("incident: file store mkdir: %w", err)
	}
	return &FileStore{Root: root}, nil
}

func (f *FileStore) dir(id string) string {
	return filepath.Join(f.Root, strings.ToLower(id))
}

// NextID scans the day's directories and claims the first free number —
// the claim is the Mkdir in Create, so NextID+Create is retried by the
// caller on collision.
func (f *FileStore) NextID(_ context.Context, day string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.Root)
	if err != nil {
		return "", fmt.Errorf("incident: list store: %w", err)
	}
	prefix := "inc-" + day + "-"
	max := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		if n, err := strconv.Atoi(name[len(prefix):]); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("INC-%s-%04d", day, max+1), nil
}

// Create reserves the incident directory (fail on collision — the id is
// the reservation) and writes the record.
func (f *FileStore) Create(_ context.Context, inc *Incident) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.dir(inc.ID)
	if err := os.Mkdir(d, 0o750); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("incident: duplicate id %s", inc.ID)
		}
		return fmt.Errorf("incident: create dir: %w", err)
	}
	return f.writeRecord(d, inc)
}

// writeRecord rewrites incident.json atomically (tmp + rename).
func (f *FileStore) writeRecord(d string, inc *Incident) error {
	raw, err := json.MarshalIndent(inc, "", "  ")
	if err != nil {
		return fmt.Errorf("incident: marshal record: %w", err)
	}
	tmp := filepath.Join(d, ".incident.json.tmp")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o640); err != nil {
		return fmt.Errorf("incident: write record: %w", err)
	}
	return os.Rename(tmp, filepath.Join(d, "incident.json"))
}

// Update implements Store.
func (f *FileStore) Update(_ context.Context, inc *Incident) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.dir(inc.ID)
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return fmt.Errorf("incident: %s not found", inc.ID)
	}
	return f.writeRecord(d, inc)
}

// Get implements Store.
func (f *FileStore) Get(_ context.Context, id string) (*Incident, error) {
	raw, err := os.ReadFile(filepath.Join(f.dir(id), "incident.json"))
	if err != nil {
		return nil, fmt.Errorf("incident: %s not found: %w", id, err)
	}
	var inc Incident
	if err := json.Unmarshal(raw, &inc); err != nil {
		return nil, fmt.Errorf("incident: %s corrupt record: %w", id, err)
	}
	return &inc, nil
}

// List implements Store (sorted by id — id order == declaration order).
func (f *FileStore) List(_ context.Context) ([]*Incident, error) {
	entries, err := os.ReadDir(f.Root)
	if err != nil {
		return nil, fmt.Errorf("incident: list store: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "inc-") {
			ids = append(ids, strings.ToUpper(e.Name()))
		}
	}
	sort.Strings(ids)
	out := make([]*Incident, 0, len(ids))
	for _, id := range ids {
		inc, err := f.Get(context.Background(), id)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, nil
}

// AppendTimeline appends one JSONL row — the ledger is append-only.
func (f *FileStore) AppendTimeline(_ context.Context, id string, e TimelineEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.dir(id)
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return fmt.Errorf("incident: %s not found", id)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("incident: marshal timeline: %w", err)
	}
	fh, err := os.OpenFile(filepath.Join(d, "timeline.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("incident: open timeline: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("incident: append timeline: %w", err)
	}
	return nil
}

// Timeline implements Store.
func (f *FileStore) Timeline(_ context.Context, id string) ([]TimelineEntry, error) {
	raw, err := os.ReadFile(filepath.Join(f.dir(id), "timeline.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("incident: read timeline: %w", err)
	}
	var out []TimelineEntry
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var e TimelineEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("incident: %s corrupt timeline: %w", id, err)
		}
		out = append(out, e)
	}
	return out, nil
}
