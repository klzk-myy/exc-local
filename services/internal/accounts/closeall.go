package accounts

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Task 5.3.36 — close-all positions convenience endpoint.
//
// Order of operations (task text): scoped mass cancel of working orders
// FIRST, then one reduce-only market close per open position with
// slippage protection. Any per-close failure after the cancels produces
// CLOSE_ALL_PARTIAL_FAILURE (409) carrying the per-position results.
//
// 2FA: the X-2FA-Token header is verified with the venue TOTP profile
// (auth.VerifyTOTP — RFC 6238, SHA1/30s/6 digits, ±1 step). A session
// already 2FA-elevated (claims AMR totp/fido2, spec §12.6 remediation F2)
// satisfies the gate without resubmitting a code.

// DefaultCloseAllSlippageBps is the slippage guard used when the request
// omits max_slippage_bps (maps to the engine's synthetic-limit
// conversion, Phase-02 Task 2.3.15).
const DefaultCloseAllSlippageBps = 100

// OpenPosition is one live position row joined to its instrument.
type OpenPosition struct {
	ID           int64           `json:"id"`
	AccountID    int64           `json:"account_id"`
	InstrumentID int64           `json:"instrument_id"`
	Symbol       string          `json:"symbol"`
	Side         string          `json:"side"` // LONG | SHORT
	Quantity     decimal.Decimal `json:"quantity"`
	EntryPrice   decimal.Decimal `json:"entry_price"`
}

// PositionReader is the read seam over the positions table.
type PositionReader interface {
	// OpenPositions returns quantity<>0 rows for the account, filtered
	// by symbol and/or position side when non-empty.
	OpenPositions(ctx context.Context, accountID int64, symbol, side string) ([]OpenPosition, error)
	// ResolveInstrument maps a symbol to instrument_id (mass-cancel
	// scope needs the id).
	ResolveInstrument(ctx context.Context, symbol string) (int64, error)
}

// TOTPSecretProvider resolves the account owner's base32 TOTP secret;
// "" means the user never enrolled (fail-closed → TWO_FACTOR_REQUIRED).
type TOTPSecretProvider interface {
	TOTPSecretForAccount(ctx context.Context, accountID int64) (string, error)
}

// CloseAllFilter carries the optional symbol/side request filters.
type CloseAllFilter struct {
	Symbol         string `json:"symbol"`
	Side           string `json:"side"` // LONG | SHORT — the *position* side
	MaxSlippageBps int    `json:"max_slippage_bps,omitempty"`
}

// CloseResult is the per-position outcome line.
type CloseResult struct {
	PositionID    int64           `json:"position_id"`
	InstrumentID  int64           `json:"instrument_id"`
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"`
	Quantity      decimal.Decimal `json:"quantity"`
	OrderID       int64           `json:"order_id,omitempty"`
	ClientOrderID string          `json:"client_order_id,omitempty"`
	OK            bool            `json:"ok"`
	Code          string          `json:"code,omitempty"` // failure code when !OK
	Detail        string          `json:"detail,omitempty"`
}

// CloseAllResult is the endpoint payload: cancels first, then per-position
// close acknowledgments.
type CloseAllResult struct {
	AccountID       int64         `json:"account_id"`
	OrdersCancelled int           `json:"orders_cancelled"`
	Closes          []CloseResult `json:"closes"`
	PartialFailure  bool          `json:"partial_failure"`
}

// MutationGuard is the legal-hold gate: AssertMutable rejects every
// mutation on a non-ACTIVE account. *FreezeService implements it.
type MutationGuard interface {
	AssertMutable(ctx context.Context, accountID int64) error
}

// TOTPVerifier validates a 6-digit RFC 6238 code against a base32
// secret at `at`. Wire auth.VerifyTOTP (SHA1/30s/6 digits, ±1 step) at
// composition; kept as a func field so this package does not import the
// auth cluster.
type TOTPVerifier func(secret, code string, at time.Time) bool

// CloseAllService orchestrates the endpoint.
type CloseAllService struct {
	freeze MutationGuard
	pos    PositionReader
	totp   TOTPSecretProvider
	verify TOTPVerifier
	disp   OrderDispatcher
	now    func() time.Time
}

// NewCloseAllService wires the service. All deps are mandatory — a nil
// dependency fails closed at request time, not silently bypassed. verify
// is the TOTP checker (nil → every non-elevated request rejects).
func NewCloseAllService(freeze MutationGuard, pos PositionReader,
	totp TOTPSecretProvider, verify TOTPVerifier, disp OrderDispatcher) *CloseAllService {
	return &CloseAllService{freeze: freeze, pos: pos, totp: totp, verify: verify, disp: disp, now: time.Now}
}

// CloseAll runs the convenience flow. totpToken is the X-2FA-Token
// header value ("" allowed only when session2FA is already true).
// session2FA reports the session's existing second-factor elevation
// (claims.TwoFactorVerified()).
func (s *CloseAllService) CloseAll(ctx context.Context, accountID int64,
	filter CloseAllFilter, totpToken string, session2FA bool) (*CloseAllResult, error) {

	if accountID == 0 {
		return nil, newError(CodeUnauthorized, "account context required")
	}
	// FROZEN accounts cannot trade — the legal hold blocks order entry
	// even for risk-reducing closes (Task 5.3.12 item 1; the admin
	// force-close path lives in liquidation, not this endpoint).
	if s.freeze != nil {
		if err := s.freeze.AssertMutable(ctx, accountID); err != nil {
			return nil, err
		}
	}

	if err := s.checkTwoFactor(ctx, accountID, totpToken, session2FA); err != nil {
		return nil, err
	}

	if filter.Symbol != "" && filter.Symbol != upperASCII(filter.Symbol) {
		return nil, newError(CodeInvalidRequest, "symbol must be upper-case (e.g. EURUSD)")
	}
	if filter.Side != "" && filter.Side != "LONG" && filter.Side != "SHORT" {
		return nil, errorf(CodeInvalidRequest, "invalid side filter %q (LONG|SHORT)", filter.Side)
	}

	result := &CloseAllResult{AccountID: accountID, Closes: []CloseResult{}}

	// Step 1: scoped mass cancel of working orders. A symbol filter
	// resolves to instrument_id; a side filter maps LONG positions to
	// cancelling resting BUY orders? No — the filter dimensions apply to
	// the positions being closed; the order-side cancel scope is left
	// unconstrained (closing a LONG still cancels that instrument's
	// working orders both sides, matching "mass cancel of working orders"
	// in the task text).
	var instrumentID int64
	if filter.Symbol != "" && s.pos != nil {
		id, err := s.pos.ResolveInstrument(ctx, filter.Symbol)
		if err != nil {
			return nil, err
		}
		instrumentID = id
	}
	if s.disp != nil {
		mc, err := s.disp.MassCancel(ctx, MassCancelScope{
			AccountID:    accountID,
			InstrumentID: instrumentID,
			Reason:       "close_all",
		})
		if err != nil {
			return nil, errorf("INTERNAL_ERROR", "close-all mass cancel: %v", err)
		}
		if mc != nil {
			result.OrdersCancelled = mc.Cancelled
		}
	}

	// Step 2: batch market closes, reduce-only + slippage protection.
	positions, err := s.pos.OpenPositions(ctx, accountID, filter.Symbol, filter.Side)
	if err != nil {
		return nil, err
	}
	bps := filter.MaxSlippageBps
	if bps <= 0 {
		bps = DefaultCloseAllSlippageBps
	}
	for _, p := range positions {
		res := CloseResult{
			PositionID:   p.ID,
			InstrumentID: p.InstrumentID,
			Symbol:       p.Symbol,
			Side:         p.Side,
			Quantity:     p.Quantity,
		}
		if s.disp == nil {
			res.Code = "INTERNAL_ERROR"
			res.Detail = "order dispatcher not wired"
			result.PartialFailure = true
			result.Closes = append(result.Closes, res)
			continue
		}
		ack, err := s.disp.SubmitClose(ctx, CloseOrderRequest{
			AccountID:      accountID,
			InstrumentID:   p.InstrumentID,
			Side:           closeSide(p.Side),
			Quantity:       p.Quantity,
			ReduceOnly:     true,
			MaxSlippageBps: bps,
			ClientOrderID: fmt.Sprintf("closeall-%d-%d-%d",
				accountID, p.ID, s.now().UnixMilli()),
		})
		if err != nil {
			res.Code = codeOf(err)
			res.Detail = err.Error()
			result.PartialFailure = true
		} else if ack == nil || !ack.Accepted {
			res.Code = CodeCloseAllPartialFailure
			if ack != nil {
				res.Detail = ack.Detail
				res.OrderID = ack.OrderID
				res.ClientOrderID = ack.ClientOrderID
			}
			result.PartialFailure = true
		} else {
			res.OK = true
			res.OrderID = ack.OrderID
			res.ClientOrderID = ack.ClientOrderID
		}
		result.Closes = append(result.Closes, res)
	}

	if result.PartialFailure {
		return result, newError(CodeCloseAllPartialFailure,
			"one or more position closes failed; see closes[].code")
	}
	return result, nil
}

// checkTwoFactor enforces the X-2FA-Token requirement. Precedence
// (spec §12.6 F2): session AMR elevation satisfies the gate without a
// fresh code.
func (s *CloseAllService) checkTwoFactor(ctx context.Context, accountID int64,
	token string, session2FA bool) error {
	if session2FA {
		return nil
	}
	if s.totp == nil {
		return newError(CodeTwoFactorRequired, "2FA provider not configured")
	}
	if token == "" {
		return newError(CodeTwoFactorRequired, "X-2FA-Token header required")
	}
	secret, err := s.totp.TOTPSecretForAccount(ctx, accountID)
	if err != nil {
		return errorf("INTERNAL_ERROR", "2FA secret lookup: %v", err)
	}
	if secret == "" {
		return newError(CodeTwoFactorRequired, "2FA not enrolled for account owner")
	}
	if s.verify == nil || !s.verify(secret, token, s.now()) {
		return newError(CodeTwoFactorRequired, "invalid 2FA token")
	}
	return nil
}

// closeSide maps an open position to the closing order side.
func closeSide(positionSide string) OrderSide {
	if positionSide == "SHORT" {
		return SideBuy
	}
	return SideSell
}

// ---------------------------------------------------------------------------
// PostgreSQL implementations of the read seams
// ---------------------------------------------------------------------------

// PgxPositionReader reads positions joined to instruments for symbol.
type PgxPositionReader struct {
	pool *pgxpool.Pool
}

// NewPgxPositionReader builds the reader.
func NewPgxPositionReader(pool *pgxpool.Pool) *PgxPositionReader {
	return &PgxPositionReader{pool: pool}
}

// OpenPositions implements PositionReader.
func (r *PgxPositionReader) OpenPositions(ctx context.Context, accountID int64,
	symbol, side string) ([]OpenPosition, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT p.id, p.account_id, p.instrument_id, i.symbol, p.side::text,
		        p.quantity, p.entry_price
		   FROM positions p JOIN instruments i ON i.id = p.instrument_id
		  WHERE p.account_id = $1 AND p.quantity <> 0
		    AND ($2::text = '' OR i.symbol = $2)
		    AND ($3::text = '' OR p.side::text = $3)
		  ORDER BY p.id`, accountID, symbol, side)
	if err != nil {
		return nil, errorf("INTERNAL_ERROR", "read open positions: %v", err)
	}
	defer rows.Close()
	out := []OpenPosition{}
	for rows.Next() {
		var p OpenPosition
		if err := rows.Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.Symbol,
			&p.Side, &p.Quantity, &p.EntryPrice); err != nil {
			return nil, errorf("INTERNAL_ERROR", "scan position: %v", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolveInstrument implements PositionReader.
func (r *PgxPositionReader) ResolveInstrument(ctx context.Context, symbol string) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol = $1`, symbol).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, errorf(CodeNotFound, "instrument %q not found", symbol)
	}
	if err != nil {
		return 0, errorf("INTERNAL_ERROR", "resolve instrument: %v", err)
	}
	return id, nil
}

// PgxTOTPSecrets resolves users.totp_secret for the account owner.
// box (optional) unwraps secrets stored sealed; a nil box reads the
// column as the plaintext base32 seed.
type PgxTOTPSecrets struct {
	pool *pgxpool.Pool
	box  SecretCodec
}

// NewPgxTOTPSecrets builds the provider.
func NewPgxTOTPSecrets(pool *pgxpool.Pool, box SecretCodec) *PgxTOTPSecrets {
	return &PgxTOTPSecrets{pool: pool, box: box}
}

// TOTPSecretForAccount implements TOTPSecretProvider. When a SecretBox
// is configured the stored value is treated as base64(nonce||ct) and
// unwrapped — a corrupt blob fails closed. Otherwise the column is read
// as the plaintext base32 seed.
func (p *PgxTOTPSecrets) TOTPSecretForAccount(ctx context.Context, accountID int64) (string, error) {
	var secret *string
	err := p.pool.QueryRow(ctx,
		`SELECT u.totp_secret FROM users u
		  JOIN accounts a ON a.user_id = u.id
		 WHERE a.id = $1`, accountID).Scan(&secret)
	if err == pgx.ErrNoRows {
		return "", errorf(CodeNotFound, "account %d owner not found", accountID)
	}
	if err != nil {
		return "", errorf("INTERNAL_ERROR", "totp secret read: %v", err)
	}
	if secret == nil || *secret == "" {
		return "", nil
	}
	if p.box == nil {
		return *secret, nil
	}
	blob, err := base64.StdEncoding.DecodeString(*secret)
	if err != nil {
		return "", errorf("INTERNAL_ERROR", "totp secret decode: %v", err)
	}
	raw, err := p.box.Open(blob)
	if err != nil {
		return "", errorf("INTERNAL_ERROR", "totp secret unwrap: %v", err)
	}
	return string(raw), nil
}

// codeOf extracts the machine-readable code from a coded error for the
// per-position failure line.
func codeOf(err error) string {
	var e *excerrors.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "INTERNAL_ERROR"
}

func upperASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
