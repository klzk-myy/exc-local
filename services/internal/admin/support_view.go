// Task 7.3.7 item 4 — support-view: the read-only account dossier a
// Support Agent troubleshoots from. No impersonation, no mutation
// endpoints; every view is written to admin_audit_log so "who looked at
// which account" is provable (Phase-07 AC #18, spec §8.2).
package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// SupportView is the read-only account dossier.
type SupportView struct {
	AccountID   int64            `json:"account_id"`
	UserID      int64            `json:"user_id"`
	Status      string           `json:"status"`
	KYCTier     string           `json:"kyc_tier"`
	AccountType string           `json:"account_type"`
	CreatedAt   time.Time        `json:"created_at"`
	Balances    []SupportBalance `json:"balances"`
	Orders      []SupportOrder   `json:"recent_orders"`
	Tickets     []SupportTicket  `json:"recent_tickets"`
	KYCDocs     []SupportKYCDoc  `json:"kyc_documents"`
}

// SupportBalance is one currency row (decimal strings — exact).
type SupportBalance struct {
	Currency  string `json:"currency"`
	Available string `json:"available"`
	Locked    string `json:"locked"`
}

// SupportOrder is a truncated order row for troubleshooting.
type SupportOrder struct {
	ID             int64     `json:"id"`
	Symbol         string    `json:"symbol,omitempty"`
	Side           string    `json:"side"`
	OrderType      string    `json:"order_type"`
	Status         string    `json:"status"`
	Quantity       string    `json:"quantity"`
	FilledQuantity string    `json:"filled_quantity"`
	Price          string    `json:"price,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// SupportTicket is the ticket summary embedded in the view; the shape is
// copied here so admin does not import support (support imports admin
// for audit logging — the reverse edge would be a cycle).
type SupportTicket struct {
	ID         int64      `json:"id"`
	Type       string     `json:"type"`
	Category   string     `json:"category"`
	Status     string     `json:"status"`
	Priority   string     `json:"priority"`
	Subject    string     `json:"subject"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// SupportKYCDoc is one KYC document's review status.
type SupportKYCDoc struct {
	ID         int64      `json:"id"`
	Type       string     `json:"type"`
	Status     string     `json:"status"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// SupportViewService assembles the dossier and audit-logs each view.
type SupportViewService struct {
	pool *pgxpool.Pool
}

// NewSupportViewService wires the production service.
func NewSupportViewService(pool *pgxpool.Pool) *SupportViewService {
	return &SupportViewService{pool: pool}
}

// View returns the account dossier and writes the
// `support.account_view` admin_audit_log row (action, target, viewer
// identity — never the payload, which stays queryable, not copied). A
// failed audit write fails the view closed (spec §2.7: unlogged
// privileged reads are not served).
func (s *SupportViewService) View(ctx context.Context, adminUserID int64, accountID int64, clientIP string) (*SupportView, error) {
	var v SupportView
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, account_type::text, kyc_tier::text, status::text, created_at
		  FROM accounts WHERE id = $1`, accountID).
		Scan(&v.AccountID, &v.UserID, &v.AccountType, &v.KYCTier, &v.Status, &v.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("ACCOUNT_NOT_FOUND",
			fmt.Sprintf("account %d not found", accountID))
	}
	if err != nil {
		return nil, fmt.Errorf("support view: read account: %w", err)
	}

	v.Balances = []SupportBalance{}
	rows, err := s.pool.Query(ctx, `
		SELECT currency, available::text, locked::text
		  FROM balances WHERE account_id = $1 ORDER BY currency`, accountID)
	if err != nil {
		return nil, fmt.Errorf("support view: balances: %w", err)
	}
	for rows.Next() {
		var b SupportBalance
		if err := rows.Scan(&b.Currency, &b.Available, &b.Locked); err != nil {
			rows.Close()
			return nil, fmt.Errorf("support view: balance scan: %w", err)
		}
		v.Balances = append(v.Balances, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	v.Orders = []SupportOrder{}
	rows, err = s.pool.Query(ctx, `
		SELECT o.id, COALESCE(i.symbol,''), o.side::text, o.order_type::text,
		       o.status::text, o.quantity::text, COALESCE(o.filled_qty::text,'0'),
		       COALESCE(o.price::text,''), o.created_at
		  FROM orders o LEFT JOIN instruments i ON i.id = o.instrument_id
		 WHERE o.account_id = $1
		 ORDER BY o.created_at DESC, o.id DESC LIMIT 50`, accountID)
	if err != nil {
		return nil, fmt.Errorf("support view: orders: %w", err)
	}
	for rows.Next() {
		var o SupportOrder
		if err := rows.Scan(&o.ID, &o.Symbol, &o.Side, &o.OrderType, &o.Status,
			&o.Quantity, &o.FilledQuantity, &o.Price, &o.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("support view: order scan: %w", err)
		}
		v.Orders = append(v.Orders, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// support_tickets lands with migration 048; a missing table degrades
	// the section to empty rather than failing the whole view (the table
	// is absent on pre-048 schemas — everything else still works).
	v.Tickets = []SupportTicket{}
	rows, err = s.pool.Query(ctx, `
		SELECT id, type::text, category::text, status::text, priority::text,
		       subject, created_at, resolved_at
		  FROM support_tickets WHERE account_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT 50`, accountID)
	if err != nil {
		if !isUndefinedTable(err) {
			return nil, fmt.Errorf("support view: tickets: %w", err)
		}
	} else {
		for rows.Next() {
			var t SupportTicket
			if err := rows.Scan(&t.ID, &t.Type, &t.Category, &t.Status,
				&t.Priority, &t.Subject, &t.CreatedAt, &t.ResolvedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("support view: ticket scan: %w", err)
			}
			v.Tickets = append(v.Tickets, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	v.KYCDocs = []SupportKYCDoc{}
	rows, err = s.pool.Query(ctx, `
		SELECT id, type, status::text, verified_at, created_at
		  FROM kyc_documents WHERE account_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT 20`, accountID)
	if err != nil {
		if !isUndefinedTable(err) {
			return nil, fmt.Errorf("support view: kyc: %w", err)
		}
	} else {
		for rows.Next() {
			var k SupportKYCDoc
			if err := rows.Scan(&k.ID, &k.Type, &k.Status, &k.VerifiedAt, &k.CreatedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("support view: kyc scan: %w", err)
			}
			v.KYCDocs = append(v.KYCDocs, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Audit-logged read (Task 7.3.7 item 4): the view itself is the only
	// mutation support-view ever performs.
	if _, _, err := LogAuto(ctx, s.pool, AuditEntry{
		AdminUserID: adminUserID,
		Action:      "support.account_view",
		TargetType:  "account",
		TargetID:    &accountID,
		AfterState:  map[string]any{"sections": []string{"balances", "orders", "tickets", "kyc"}},
		IPAddress:   clientIP,
	}); err != nil {
		return nil, fmt.Errorf("support view: audit log: %w", err)
	}
	return &v, nil
}

// isUndefinedTable reports SQLSTATE 42P01 (relation does not exist) —
// the graceful-degradation check for sections whose migration has not
// been applied yet.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	for err != nil {
		if e, ok := err.(*pgconn.PgError); ok {
			pgErr = e
			break
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return pgErr != nil && pgErr.Code == "42P01"
}
