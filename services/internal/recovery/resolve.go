package recovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RowQuerier is the narrow DB seam for symbol resolution — *pgxpool.Pool
// satisfies it; tests substitute a fake.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ResolveInstrumentID maps a symbol like "EUR/USD" to its instrument id
// via the instruments table (spec §5.1). Returns also a {id: symbol} label
// map for report decoration.
func ResolveInstrumentID(ctx context.Context, q RowQuerier, symbol string) (uint32, map[uint32]string, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	var id int64
	if err := q.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol = $1`, symbol).Scan(&id); err != nil {
		return 0, nil, fmt.Errorf("resolve symbol %q: %w", symbol, err)
	}
	return uint32(id), map[uint32]string{uint32(id): symbol}, nil
}
