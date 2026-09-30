package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Room is a provisioned war-room channel handle. Ref is the provider's
// own address (Slack channel id, file path, ...) — opaque to Manager.
type Room struct {
	Name  string `json:"name"` // inc-YYYYMMDD-NNNN (no '#')
	Topic string `json:"topic"`
	Ref   string `json:"ref"` // provider handle / adapter path
}

// Bridge is a conference-bridge handle (runbook §3.2).
type Bridge struct {
	URI string `json:"uri"` // dial-in / join handle
	Ref string `json:"ref"` // provider record reference
}

// ChatOps is the war-room provider seam — Slack in production, the
// file adapter in dev/tests. Implementations must be truthful: a
// CreateRoom that could not actually create anything returns an error;
// nothing may fabricate a room that does not exist.
type ChatOps interface {
	// CreateRoom provisions the incident channel; returns its handle.
	CreateRoom(ctx context.Context, room Room) (Room, error)
	// PostMessage appends a message to the room. Returns an error when
	// the message did not reach the room — callers record the failure
	// rather than pretend the stakeholder update landed.
	PostMessage(ctx context.Context, room Room, author, text string) error
	// OpenBridge spins up the conference bridge; returns its join handle.
	OpenBridge(ctx context.Context, room Room) (Bridge, error)
}

// ---------------------------------------------------------------------------
// FileChatOps — the shipped dev/test adapter. Everything it claims is a
// real artifact on disk:
//
//	<dir>/rooms/<name>/room.json       room record (idempotent create)
//	<dir>/rooms/<name>/messages.jsonl  posted messages, append-only
//	<dir>/rooms/<name>/bridge.json     bridge record — the file adapter
//	                                   cannot mint a real dial-in; it
//	                                   records the request and marks
//	                                   the bridge provider "manual" so
//	                                   the timeline stays truthful.
// ---------------------------------------------------------------------------

// FileChatOps persists rooms under Root.
type FileChatOps struct {
	Root string
	now  func() time.Time
	mu   sync.Mutex
}

// NewFileChatOps creates the adapter rooted at dir (rooms live under
// dir/rooms).
func NewFileChatOps(dir string) *FileChatOps {
	return &FileChatOps{Root: dir, now: time.Now}
}

func (f *FileChatOps) roomDir(name string) string {
	return filepath.Join(f.Root, "rooms", name)
}

// CreateRoom implements ChatOps.
func (f *FileChatOps) CreateRoom(_ context.Context, room Room) (Room, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.roomDir(room.Name)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return Room{}, fmt.Errorf("incident: chatops create room dir: %w", err)
	}
	rec := struct {
		Name      string `json:"name"`
		Topic     string `json:"topic"`
		Provider  string `json:"provider"`
		CreatedAt string `json:"created_at"`
	}{room.Name, room.Topic, "file", f.now().UTC().Format(time.RFC3339Nano)}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Room{}, fmt.Errorf("incident: chatops marshal room: %w", err)
	}
	if err := os.WriteFile(filepath.Join(d, "room.json"), append(raw, '\n'), 0o640); err != nil {
		return Room{}, fmt.Errorf("incident: chatops write room: %w", err)
	}
	room.Ref = d
	return room, nil
}

// PostMessage implements ChatOps — appends one JSONL record to the
// room's message log. A missing room errors (never silently dropped).
func (f *FileChatOps) PostMessage(_ context.Context, room Room, author, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.roomDir(room.Name)
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return fmt.Errorf("incident: chatops room %q does not exist", room.Name)
	}
	raw, err := json.Marshal(struct {
		At     string `json:"at"`
		Author string `json:"author"`
		Text   string `json:"text"`
	}{f.now().UTC().Format(time.RFC3339Nano), author, text})
	if err != nil {
		return fmt.Errorf("incident: chatops marshal message: %w", err)
	}
	fh, err := os.OpenFile(filepath.Join(d, "messages.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("incident: chatops open messages: %w", err)
	}
	defer fh.Close()
	if _, err := fh.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("incident: chatops append message: %w", err)
	}
	return nil
}

// OpenBridge implements ChatOps. The file adapter cannot spin up a real
// conference bridge — it writes the request record with
// provider="manual" so ops sees the bridge is still owed, and returns
// the record path as the handle. Production wires the voice/chat
// provider's adapter here instead.
func (f *FileChatOps) OpenBridge(_ context.Context, room Room) (Bridge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.roomDir(room.Name)
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return Bridge{}, fmt.Errorf("incident: chatops room %q does not exist", room.Name)
	}
	rec := struct {
		Provider  string `json:"provider"` // "manual" — operator spins the real bridge
		Requested string `json:"requested_at"`
		Room      string `json:"room"`
	}{"manual", f.now().UTC().Format(time.RFC3339Nano), room.Name}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Bridge{}, fmt.Errorf("incident: chatops marshal bridge: %w", err)
	}
	p := filepath.Join(d, "bridge.json")
	if err := os.WriteFile(p, append(raw, '\n'), 0o640); err != nil {
		return Bridge{}, fmt.Errorf("incident: chatops write bridge: %w", err)
	}
	return Bridge{URI: "manual:" + room.Name, Ref: p}, nil
}

// Messages returns the room's posted messages (tests/inspection).
func (f *FileChatOps) Messages(name string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(f.roomDir(name), "messages.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var m struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, err
		}
		out = append(out, m.Text)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// LogChatOps — last-resort sink so a provider outage never silences the
// war-room record. It does NOT claim to create real rooms: Ref is the
// log line itself and callers must pair it with a real adapter (or
// accept a degraded, clearly-logged war room).
// ---------------------------------------------------------------------------

// LogChatOps logs every operation at error level.
type LogChatOps struct{ Log *slog.Logger }

// CreateRoom implements ChatOps.
func (l LogChatOps) CreateRoom(_ context.Context, room Room) (Room, error) {
	l.log().Error("chatops room (log-only)", "room", room.Name, "topic", room.Topic)
	room.Ref = "log:" + room.Name
	return room, nil
}

// PostMessage implements ChatOps.
func (l LogChatOps) PostMessage(_ context.Context, room Room, author, text string) error {
	l.log().Error("chatops message (log-only)", "room", room.Name, "author", author, "text", text)
	return nil
}

// OpenBridge implements ChatOps.
func (l LogChatOps) OpenBridge(_ context.Context, room Room) (Bridge, error) {
	l.log().Error("chatops bridge (log-only, operator must spin the real bridge)", "room", room.Name)
	return Bridge{URI: "manual:" + room.Name, Ref: "log"}, nil
}

func (l LogChatOps) log() *slog.Logger {
	if l.Log == nil {
		return slog.Default()
	}
	return l.Log
}

// FanoutChatOps mirrors observability.FanoutSink: every call runs
// against every adapter; the first error is returned but all adapters
// are attempted (a partial outage degrades, never disappears).
type FanoutChatOps []ChatOps

// CreateRoom implements ChatOps — uses the first adapter's Room handle.
func (f FanoutChatOps) CreateRoom(ctx context.Context, room Room) (Room, error) {
	var first error
	var out Room
	for i, c := range f {
		r, err := c.CreateRoom(ctx, room)
		if err != nil && first == nil {
			first = err
		}
		if i == 0 {
			out = r
		}
	}
	return out, first
}

// PostMessage implements ChatOps.
func (f FanoutChatOps) PostMessage(ctx context.Context, room Room, author, text string) error {
	var first error
	for _, c := range f {
		if err := c.PostMessage(ctx, room, author, text); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// OpenBridge implements ChatOps — uses the first adapter's Bridge.
func (f FanoutChatOps) OpenBridge(ctx context.Context, room Room) (Bridge, error) {
	var first error
	var out Bridge
	for i, c := range f {
		b, err := c.OpenBridge(ctx, room)
		if err != nil && first == nil {
			first = err
		}
		if i == 0 {
			out = b
		}
	}
	return out, first
}
