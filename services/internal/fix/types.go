package fix

import (
	"context"
	"time"

	"exchange/internal/accounts"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
)

// Session is the fix_sessions row — entitlement, sequence state and
// lifecycle for one FIX BeginString:Sender->Target pair (spec §5.20).
type Session struct {
	ID                 int64
	SessionID          string // "FIX.4.4:<venue>-><client>" (quickfixgo SessionID.String)
	ProtocolVersion    string // FIX.4.4 | FIX.5.0SP2
	SenderSeqNum       int64
	TargetSeqNum       int64
	Status             string // ACTIVE | DISCONNECTED | LOGGED_OUT
	LastHeartbeatAt    *time.Time
	AccountID          *int64 // nil = drop-copy (read-only) session
	APIKeyID           *int64 // bound logon credential (api_keys.id)
	AllowedInstruments string // "" = all instruments; else comma list
	CancelOnDisconnect bool
	MaxMsgsPerSec      int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Entitled reports whether the session may trade instrument `symbol`:
// drop-copy sessions (no bound account) may never submit; a blank
// allowed_instruments means all instruments (spec §5.20 NULL=all).
func (s *Session) Entitled(symbol string) bool {
	if s.AccountID == nil {
		return false
	}
	if s.AllowedInstruments == "" {
		return true
	}
	for _, tok := range splitCSV(s.AllowedInstruments) {
		if tok == symbol {
			return true
		}
	}
	return false
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			tok := s[start:i]
			for len(tok) > 0 && (tok[0] == ' ' || tok[0] == '\t') {
				tok = tok[1:]
			}
			for len(tok) > 0 && (tok[len(tok)-1] == ' ' || tok[len(tok)-1] == '\t') {
				tok = tok[:len(tok)-1]
			}
			if tok != "" {
				out = append(out, tok)
			}
			start = i + 1
		}
	}
	return out
}

// Store is the persistence seam for fix_sessions + the api_keys
// credential check. *PgStore satisfies it; tests use the in-memory fake.
type Store interface {
	// SessionByID loads the row keyed by the canonical session_id
	// (quickfixgo SessionID.String()).
	SessionByID(ctx context.Context, sessionID string) (*Session, error)
	// CreateSession registers a new session row (first-seen logon of a
	// pre-provisioned pair or admin provisioning).
	CreateSession(ctx context.Context, s *Session) (*Session, error)
	// SetStatus updates lifecycle status (and last_heartbeat_at when set).
	SetStatus(ctx context.Context, sessionID, status string, heartbeatAt *time.Time) error
	// SetSeq persists the outgoing/incoming counters.
	SetSeq(ctx context.Context, sessionID string, sender, target int64) error
	// SeqState reads back the persisted counters (failover seam,
	// Task 18.3.12).
	SeqState(ctx context.Context, sessionID string) (sender, target int64, ok bool, err error)
	// RefreshSession is a no-op marker for hot-standby rehydration —
	// kept in the interface so Task 18.3.12's secondary-gateway path is
	// already expressible.
	RefreshSession(ctx context.Context, sessionID string) error
	// SaveMessage archives one outbound frame for resend replay.
	SaveMessage(ctx context.Context, sessionID string, seqNum int64, msg []byte) error
	// Messages returns archived frames for [begin, end] (0=open end).
	Messages(ctx context.Context, sessionID string, begin, end int64) ([][]byte, error)
	// PurgeMessages drops the archive (sequence reset).
	PurgeMessages(ctx context.Context, sessionID string) error
	// VerifyAPIKey resolves a logon credential: returns the bound
	// api_keys row's account and whether keyID+secret verifies ACTIVE.
	VerifyAPIKey(ctx context.Context, keyID int64, keyName, secret string) (accountID int64, ok bool, err error)
	// UpdateEntitlement is the admin write path for
	// PUT /admin/fix-sessions/{id} (Task 18.3.9 item 5).
	UpdateEntitlement(ctx context.Context, sessionID string, u EntitlementUpdate) (*Session, error)
}

// EntitlementUpdate is the admin-editable surface of a session row.
// Nil fields are left untouched.
type EntitlementUpdate struct {
	AccountID          *int64
	ClearAccount       bool // drop-copy conversion: set account_id NULL
	APIKeyID           *int64
	ClearAPIKey        bool    // session-lock: set api_key_id NULL
	AllowedInstruments *string // pointer to "" clears (all instruments)
	SetAllInstruments  bool    // allowed_instruments = NULL (all)
	CancelOnDisconnect *bool
	MaxMsgsPerSec      *int
}

// OrderFlow is the slice of orders.Service the FIX order-entry path
// needs. *orders.Service satisfies it; the seam keeps the gateway unit-
// testable and is the shared surface Task 18.3.7 (mass quoting) and any
// later FIX-order sibling reuse.
type OrderFlow interface {
	Submit(ctx context.Context, acct *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error)
	Cancel(ctx context.Context, acct *orders.Account, orderID int64,
		actor, requestID, ip string) (*orders.Ack, error)
	CancelReplace(ctx context.Context, acct *orders.Account, orderID int64,
		req *orders.CancelReplaceRequest, actor, requestID, ip string) (*orders.Order, error)
	// CancelOnDisconnect mass-cancels orders attributed to one session
	// (Task 5.3.25 item 4 semantics — session_id scope, reason
	// "cancel_on_disconnect").
	CancelOnDisconnect(ctx context.Context, accountID int64,
		sessionID string) (*orders.MassCancelResult, error)
	AccountByID(ctx context.Context, id int64) (*orders.Account, error)
	GetOrder(ctx context.Context, acct *orders.Account, orderID int64) (*orders.Order, error)
}

// OrderRead is the read-model slice used to resolve OrigClOrdID and to
// re-hydrate orders for async ExecutionReports.
type OrderRead interface {
	DedupLookup(ctx context.Context, accountID int64, clientOrderID string) (*orders.DedupRow, error)
	GetOrder(ctx context.Context, orderID int64) (*orders.Order, error)
	ApplyCancel(ctx context.Context, orderID int64) error
	ApplyFill(ctx context.Context, orderID int64, price, qty decimal.Decimal) error
}

// DeadManTimer is the canonical countdown-cancel account timer shared
// across REST/WS/FIX (Phase-05 Task 5.3.33 machinery — spec §24 #257).
// *accounts.DeadManService satisfies it.
type DeadManTimer interface {
	Set(ctx context.Context, accountID int64, countdownMs int64, renew bool) (*accounts.CountdownAck, error)
	Disable(ctx context.Context, accountID int64) (*accounts.CountdownAck, error)
	Status(ctx context.Context, accountID int64) (*accounts.CountdownAck, error)
}

// SubAccountChecker verifies Tag-1 Account sub-account entitlement
// (spec §9.3: session's bound account "or a sub-account of the bound
// account"). accounts.SubAccountService satisfies it in production;
// nil → strict equality only.
type SubAccountChecker interface {
	IsSubAccountOf(ctx context.Context, masterID, subID int64) (bool, error)
}
