package fix

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
)

// PgMessageStoreFactory builds quickfix.MessageStore instances whose
// sequence numbers live in fix_sessions and whose resend archive lives
// in fix_messages (spec §9.3 "sequence number persistence: PostgreSQL
// fix_sessions table"). One store instance per session pair.
type PgMessageStoreFactory struct {
	st  Store
	now func() time.Time
}

// NewPgMessageStoreFactory wires the factory.
func NewPgMessageStoreFactory(st Store) *PgMessageStoreFactory {
	return &PgMessageStoreFactory{st: st, now: time.Now}
}

// Create hydrates (or provisions) the session's sequence state.
// quickfixgo calls this once per session lifetime — for acceptor
// dynamic sessions, at first inbound Logon.
func (f *PgMessageStoreFactory) Create(sessionID quickfix.SessionID) (quickfix.MessageStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := sessionID.String()
	row, err := f.st.SessionByID(ctx, key)
	if err != nil {
		return nil, err
	}
	if row == nil {
		// Provision the row so sequence persistence has somewhere to
		// land even before the session row is entitled (auth may still
		// reject the logon later — the row is harmless state).
		row, err = f.st.CreateSession(ctx, &Session{
			SessionID:       key,
			ProtocolVersion: sessionID.BeginString,
			Status:          "DISCONNECTED",
		})
		if err != nil {
			return nil, err
		}
	}
	return &pgMessageStore{
		st:        f.st,
		key:       key,
		senderSeq: row.SenderSeqNum,
		targetSeq: row.TargetSeqNum,
		created:   row.CreatedAt,
	}, nil
}

// pgMessageStore implements quickfix.MessageStore over PostgreSQL.
// quickfixgo drives it single-threaded per session, but the mutex
// keeps Refresh/reset races well-defined anyway.
type pgMessageStore struct {
	st  Store
	key string

	mu        sync.Mutex
	senderSeq int64 // next OUTGOING seq num (fix_sessions.sender_seq_num)
	targetSeq int64 // next INCOMING seq num (fix_sessions.target_seq_num)
	created   time.Time
}

var _ quickfix.MessageStore = (*pgMessageStore)(nil)

func (s *pgMessageStore) NextSenderMsgSeqNum() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.senderSeq)
}

func (s *pgMessageStore) NextTargetMsgSeqNum() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.targetSeq)
}

// persist writes the in-memory pair to fix_sessions. Callers hold mu.
func (s *pgMessageStore) persist() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.st.SetSeq(ctx, s.key, s.senderSeq, s.targetSeq)
}

func (s *pgMessageStore) IncrNextSenderMsgSeqNum() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.senderSeq++
	return s.persist()
}

func (s *pgMessageStore) IncrNextTargetMsgSeqNum() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetSeq++
	return s.persist()
}

func (s *pgMessageStore) SetNextSenderMsgSeqNum(next int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.senderSeq = int64(next)
	return s.persist()
}

func (s *pgMessageStore) SetNextTargetMsgSeqNum(next int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetSeq = int64(next)
	return s.persist()
}

func (s *pgMessageStore) CreationTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.created
}

func (s *pgMessageStore) SetCreationTime(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = t
}

func (s *pgMessageStore) SaveMessage(seqNum int, msg []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cp := make([]byte, len(msg))
	copy(cp, msg)
	return s.st.SaveMessage(ctx, s.key, int64(seqNum), cp)
}

func (s *pgMessageStore) SaveMessageAndIncrNextSenderMsgSeqNum(seqNum int, msg []byte) error {
	if err := s.SaveMessage(seqNum, msg); err != nil {
		return err
	}
	return s.IncrNextSenderMsgSeqNum()
}

func (s *pgMessageStore) GetMessages(beginSeqNum, endSeqNum int) ([][]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.st.Messages(ctx, s.key, int64(beginSeqNum), int64(endSeqNum))
}

func (s *pgMessageStore) IterateMessages(beginSeqNum, endSeqNum int,
	cb func([]byte) error) error {
	msgs, err := s.GetMessages(beginSeqNum, endSeqNum)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if err := cb(m); err != nil {
			return err
		}
	}
	return nil
}

// Refresh reloads the persisted counters — the failover/RefreshOnLogon
// path (spec §9.8: the secondary gateway rehydrates from fix_sessions).
func (s *pgMessageStore) Refresh() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sender, target, ok, err := s.st.SeqState(ctx, s.key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("fix: session row %q vanished during refresh", s.key)
	}
	s.mu.Lock()
	s.senderSeq, s.targetSeq = sender, target
	s.mu.Unlock()
	return nil
}

// Reset drops the archive and zeroes both counters — the daily
// sequence reset and ResetSeqNumFlag=Y logon path (spec §9.3/§9.8).
func (s *pgMessageStore) Reset() error {
	s.mu.Lock()
	s.senderSeq, s.targetSeq = 1, 1
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.st.PurgeMessages(ctx, s.key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persist()
}

func (s *pgMessageStore) Close() error { return nil }
