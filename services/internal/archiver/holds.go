// Compliance (legal) holds — Phase-09 Tasks 9.3.17/9.3.22/9.3.24,
// spec §19.12 item 5 (GDPR Art. 17(3)(b) carve-outs).
//
// An ACTIVE row in data_retention_holds (released_at IS NULL, not
// expired) blocks every destructive lifecycle transition for the covered
// scope: hot→warm detach, warm→cold drop, and retention purge. Held
// partitions are never dropped and never expire — the hold is only
// lifted by an explicit release (Compliance Officer action).
//
// Fail-closed (§2.7): if the holds table cannot be consulted the mover
// aborts the transition rather than risk destroying held evidence. A
// missing table (schema_mismatch, pre-migration DBs) is the only
// tolerated miss — it predates the feature, so there are no holds.
package archiver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Hold is one row of data_retention_holds.
type Hold struct {
	HoldID        int64      `json:"hold_id"`
	ParentTable   string     `json:"parent_table"`
	PartitionName *string    `json:"partition_name,omitempty"` // NULL = whole parent
	CaseRef       string     `json:"case_ref"`
	Reason        string     `json:"reason"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
	ReleasedBy    *string    `json:"released_by,omitempty"`
}

// Active reports whether the hold currently blocks lifecycle work.
func (h Hold) Active(now time.Time) bool {
	if h.ReleasedAt != nil {
		return false
	}
	return h.ExpiresAt == nil || h.ExpiresAt.After(now)
}

// AddHold creates an active hold. partition may be "" for a parent-wide
// hold. expiresAt zero = indefinite.
func (a *Archiver) AddHold(ctx context.Context, parent, partition, caseRef,
	reason, createdBy string, expiresAt *time.Time) (int64, error) {
	if !identRe.MatchString(parent) {
		return 0, fmt.Errorf("archiver: unsafe parent %q", parent)
	}
	if partition != "" && !identRe.MatchString(partition) {
		return 0, fmt.Errorf("archiver: unsafe partition %q", partition)
	}
	if strings.TrimSpace(caseRef) == "" || strings.TrimSpace(createdBy) == "" {
		return 0, errors.New("archiver: hold requires case_ref and created_by")
	}
	var part *string
	if partition != "" {
		part = &partition
	}
	var id int64
	err := a.pool.QueryRow(ctx, `
		INSERT INTO data_retention_holds
		  (parent_table, partition_name, case_ref, reason, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING hold_id`,
		parent, part, caseRef, reason, createdBy, expiresAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("archiver: add hold: %w", err)
	}
	return id, nil
}

// ReleaseHold ends a hold; releasedBy records the releasing officer.
func (a *Archiver) ReleaseHold(ctx context.Context, holdID int64, releasedBy string) error {
	if strings.TrimSpace(releasedBy) == "" {
		return errors.New("archiver: release requires released_by")
	}
	tag, err := a.pool.Exec(ctx, `
		UPDATE data_retention_holds
		SET released_at = now(), released_by = $2
		WHERE hold_id = $1 AND released_at IS NULL`, holdID, releasedBy)
	if err != nil {
		return fmt.Errorf("archiver: release hold %d: %w", holdID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("archiver: hold %d not found or already released", holdID)
	}
	return nil
}

// ListHolds returns holds, newest first. activeOnly filters to holds that
// currently block lifecycle work.
func (a *Archiver) ListHolds(ctx context.Context, activeOnly bool) ([]Hold, error) {
	q := `SELECT hold_id, parent_table, partition_name, case_ref, reason,
	             created_by, created_at, expires_at, released_at, released_by
	      FROM data_retention_holds`
	if activeOnly {
		q += ` WHERE released_at IS NULL
		       AND (expires_at IS NULL OR expires_at > now())`
	}
	q += ` ORDER BY hold_id DESC`
	rows, err := a.pool.Query(ctx, q)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("archiver: list holds: %w", err)
	}
	defer rows.Close()
	var out []Hold
	for rows.Next() {
		var h Hold
		if err := rows.Scan(&h.HoldID, &h.ParentTable, &h.PartitionName,
			&h.CaseRef, &h.Reason, &h.CreatedBy, &h.CreatedAt,
			&h.ExpiresAt, &h.ReleasedAt, &h.ReleasedBy); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// partitionHeld reports whether an active hold covers this partition —
// either a partition-scoped row or a parent-wide row (partition_name NULL).
func (a *Archiver) partitionHeld(ctx context.Context, parent, partition string) (bool, []Hold, error) {
	rows, err := a.pool.Query(ctx, `
		SELECT hold_id, parent_table, partition_name, case_ref, reason,
		       created_by, created_at, expires_at, released_at, released_by
		FROM data_retention_holds
		WHERE released_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
		  AND parent_table = $1
		  AND (partition_name IS NULL OR partition_name = $2)`,
		parent, partition)
	if err != nil {
		if isUndefinedTable(err) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("archiver: hold check %s.%s: %w",
			parent, partition, err)
	}
	defer rows.Close()
	var holds []Hold
	for rows.Next() {
		var h Hold
		if err := rows.Scan(&h.HoldID, &h.ParentTable, &h.PartitionName,
			&h.CaseRef, &h.Reason, &h.CreatedBy, &h.CreatedAt,
			&h.ExpiresAt, &h.ReleasedAt, &h.ReleasedBy); err != nil {
			return false, nil, err
		}
		holds = append(holds, h)
	}
	return len(holds) > 0, holds, rows.Err()
}

// isUndefinedTable reports SQLSTATE 42P01 (table not yet migrated).
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P01"
	}
	return false
}
