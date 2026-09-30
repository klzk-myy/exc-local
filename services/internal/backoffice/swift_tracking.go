// Task 24.3.4 — SWIFT Message Tracking (spec §17; §24 #13–#15).
//
// swift_messages (migration 035) is the immutable journal of every
// SWIFT / ISO 20022 message crossing the correspondent-bank boundary —
// MT103, MT202, MT900, MT910, pacs.009 and any further type the rails
// ingest. Insert-only by construction: the store exposes no update or
// delete method and migration 035 backs that with a BEFORE UPDATE OR
// DELETE trigger — the audit trail cannot be rewritten.
package backoffice

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	excerrors "exchange/pkg/errors"
)

// Swift direction values mirror swift_direction_enum.
const (
	SwiftIn  = "IN"
	SwiftOut = "OUT"
)

// SwiftMessage is one swift_messages row.
type SwiftMessage struct {
	ID               int64     `json:"id"`
	MessageType      string    `json:"message_type"` // MT103|MT202|MT900|MT910|PACS009|...
	Reference        string    `json:"reference"`    // :20: sender ref / MsgId
	RelatedReference string    `json:"related_reference,omitempty"`
	Direction        string    `json:"direction"` // IN|OUT
	Status           string    `json:"status"`    // RECEIVED|SENT|ACKED|FAILED|PARSED
	NostroAccountID  *int64    `json:"nostro_account_id,omitempty"`
	RawPayload       string    `json:"raw_payload,omitempty"`
	MsgTimestamp     time.Time `json:"msg_timestamp"`
	CreatedAt        time.Time `json:"created_at"`
}

// SwiftFilter scopes the query surface — From/To bound msg_timestamp,
// Type/Direction exact-match; cursor is the (created_at,id) keyset.
type SwiftFilter struct {
	From      *time.Time
	To        *time.Time
	Type      string
	Direction string
	Limit     int
	CursorTS  *time.Time
	CursorID  *int64
}

// SwiftStore is the tracking persistence seam — deliberately without
// any mutation path (immutable audit trail, §24 #15).
type SwiftStore interface {
	InsertMessage(ctx context.Context, m *SwiftMessage) error
	ListMessages(ctx context.Context, f SwiftFilter) ([]SwiftMessage, error)
}

// SwiftTracker records and queries the message journal.
type SwiftTracker struct {
	store SwiftStore
	clock func() time.Time
}

// NewSwiftTracker wires the tracker; the store is required.
func NewSwiftTracker(store SwiftStore) (*SwiftTracker, error) {
	if store == nil {
		return nil, fmt.Errorf("backoffice: nil swift store")
	}
	return &SwiftTracker{store: store, clock: time.Now}, nil
}

// WithClock overrides the clock (tests).
func (t *SwiftTracker) WithClock(c func() time.Time) *SwiftTracker {
	t.clock = c
	return t
}

// SwiftRecorder is the seam other backoffice services use to journal
// messages — *SwiftTracker satisfies it.
type SwiftRecorder interface {
	Record(ctx context.Context, m SwiftMessage) (*SwiftMessage, error)
}

// Record appends one message to the journal. Validation is fail-closed:
// a message without a type, direction or reference would be
// unanswerable in an audit, so it is rejected, never stored degraded.
func (t *SwiftTracker) Record(ctx context.Context, m SwiftMessage) (*SwiftMessage, error) {
	m.MessageType = strings.ToUpper(strings.TrimSpace(m.MessageType))
	m.Reference = strings.TrimSpace(m.Reference)
	m.RelatedReference = strings.TrimSpace(m.RelatedReference)
	m.Direction = strings.ToUpper(strings.TrimSpace(m.Direction))
	if m.MessageType == "" {
		return nil, excerrors.New("INVALID_REQUEST", "swift message_type is required")
	}
	if m.Reference == "" {
		return nil, excerrors.New("INVALID_REQUEST", "swift reference (:20:/MsgId) is required")
	}
	if m.Direction != SwiftIn && m.Direction != SwiftOut {
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("swift direction %q must be IN or OUT", m.Direction))
	}
	if m.Status == "" {
		if m.Direction == SwiftIn {
			m.Status = "RECEIVED"
		} else {
			m.Status = "SENT"
		}
	}
	if m.MsgTimestamp.IsZero() {
		m.MsgTimestamp = t.clock().UTC()
	}
	if err := t.store.InsertMessage(ctx, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Query lists messages for the admin surface
// (GET /api/v1/admin/swift-messages?from=&to=&type=).
func (t *SwiftTracker) Query(ctx context.Context, f SwiftFilter) ([]SwiftMessage, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 500
	}
	if f.Type != "" {
		f.Type = strings.ToUpper(strings.TrimSpace(f.Type))
	}
	if f.Direction != "" {
		f.Direction = strings.ToUpper(strings.TrimSpace(f.Direction))
		if f.Direction != SwiftIn && f.Direction != SwiftOut {
			return nil, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("direction %q must be IN or OUT", f.Direction))
		}
	}
	rows, err := t.store.ListMessages(ctx, f)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []SwiftMessage{}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// PgNostroStore implementation
// ---------------------------------------------------------------------------

const swiftCols = `id, message_type, reference, COALESCE(related_reference,''),
	direction::text, status, nostro_account_id, COALESCE(raw_payload,''),
	msg_timestamp, created_at`

func scanSwiftMessage(row pgx.Row) (*SwiftMessage, error) {
	var m SwiftMessage
	err := row.Scan(&m.ID, &m.MessageType, &m.Reference, &m.RelatedReference,
		&m.Direction, &m.Status, &m.NostroAccountID, &m.RawPayload,
		&m.MsgTimestamp, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *PgNostroStore) InsertMessage(ctx context.Context, m *SwiftMessage) error {
	row := s.Pool.QueryRow(ctx, `
		INSERT INTO swift_messages
		    (message_type, reference, related_reference, direction, status,
		     nostro_account_id, raw_payload, msg_timestamp)
		VALUES ($1,$2,NULLIF($3,''),$4::swift_direction_enum,$5,$6,
		        NULLIF($7,''),$8)
		RETURNING `+swiftCols,
		m.MessageType, m.Reference, m.RelatedReference, m.Direction,
		m.Status, m.NostroAccountID, m.RawPayload, m.MsgTimestamp)
	out, err := scanSwiftMessage(row)
	if err != nil {
		return fmt.Errorf("insert swift message: %w", err)
	}
	*m = *out
	return nil
}

func (s *PgNostroStore) ListMessages(ctx context.Context, f SwiftFilter) ([]SwiftMessage, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT `+swiftCols+`
		  FROM swift_messages
		 WHERE ($1::timestamptz IS NULL OR msg_timestamp >= $1)
		   AND ($2::timestamptz IS NULL OR msg_timestamp <  $2)
		   AND ($3 = '' OR message_type = $3)
		   AND ($4 = '' OR direction::text = $4)
		   AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $7`,
		f.From, f.To, f.Type, f.Direction, f.CursorTS,
		cursorIDOrZero(f.CursorID), f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SwiftMessage
	for rows.Next() {
		m, err := scanSwiftMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func cursorIDOrZero(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}
