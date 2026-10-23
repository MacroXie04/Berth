package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TransitionOption adds a column update to a TransitionWorkspace statement.
type TransitionOption func(*transition)

type transition struct {
	sets []string
	args []any
}

func (t *transition) set(col string, v any) {
	t.sets = append(t.sets, col+" = ?")
	t.args = append(t.args, v)
}

// SetContainerID records the workspace's container.
func SetContainerID(id string) TransitionOption {
	return func(t *transition) { t.set("container_id", id) }
}

// ClearContainer records that the container no longer exists.
func ClearContainer() TransitionOption {
	return func(t *transition) { t.sets = append(t.sets, "container_id = NULL") }
}

// SetStoppedAt records when the container stopped.
func SetStoppedAt(ts time.Time) TransitionOption {
	return func(t *transition) { t.set("last_stopped_at", fmtTime(ts)) }
}

// SetActiveAt records the last activity.
func SetActiveAt(ts time.Time) TransitionOption {
	return func(t *transition) { t.set("last_active_at", fmtTime(ts)) }
}

// SetNekoPassword records the neko web UI password.
func SetNekoPassword(pw string) TransitionOption {
	return func(t *transition) { t.set("neko_password", pw) }
}

// ReleaseUDPRange frees the workspace's port block (its container is gone).
func ReleaseUDPRange() TransitionOption {
	return func(t *transition) {
		t.sets = append(t.sets, "udp_port_start = NULL", "udp_port_end = NULL")
	}
}

func (s *SQLite) TransitionWorkspace(ctx context.Context, userID int64, from []State, to State,
	now time.Time, opts ...TransitionOption) (Workspace, error) {
	if len(from) == 0 {
		return Workspace{}, errors.New("store: TransitionWorkspace needs at least one source state")
	}
	var t transition
	for _, o := range opts {
		o(&t)
	}
	sets := append([]string{"state = ?", "updated_at = ?"}, t.sets...)
	args := append([]any{string(to), fmtTime(now)}, t.args...)
	args = append(args, userID)
	for _, f := range from {
		args = append(args, string(f))
	}
	q := fmt.Sprintf(`UPDATE workspaces SET %s WHERE user_id = ? AND state IN (%s)`,
		strings.Join(sets, ", "), strings.TrimSuffix(strings.Repeat("?,", len(from)), ","))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return Workspace{}, fmt.Errorf("store: transition workspace: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE user_id = ?`, userID).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return Workspace{}, ErrNotFound
		case err != nil:
			return Workspace{}, fmt.Errorf("store: transition workspace: %w", err)
		}
		return Workspace{}, ErrStateConflict
	}
	ws, err := scanWorkspace(tx.QueryRowContext(ctx, workspaceSelect+` WHERE user_id = ?`, userID))
	if err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, fmt.Errorf("store: commit: %w", err)
	}
	return ws, nil
}

func (s *SQLite) TouchLastActive(ctx context.Context, userID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE workspaces SET last_active_at = ?, updated_at = ?
		  WHERE user_id = ? AND state IN ('starting', 'running')`,
		fmtTime(now), fmtTime(now), userID)
	if err != nil {
		return fmt.Errorf("store: touch last active: %w", err)
	}
	return nil
}

type portRange struct{ start, end int }

// firstFreeRange returns the lowest start in [min, max] such that
// [start, start+size-1] fits inside the bounds and overlaps none of taken
// (which must be sorted by start). ok is false when nothing fits.
func firstFreeRange(taken []portRange, min, max, size int) (start int, ok bool) {
	if size < 1 || min < 1 || max < min {
		return 0, false
	}
	cand := min
	for _, r := range taken {
		if cand+size-1 < r.start {
			break // the gap before r is big enough
		}
		if r.end+1 > cand {
			cand = r.end + 1
		}
	}
	if cand+size-1 > max {
		return 0, false
	}
	return cand, true
}

func (s *SQLite) AllocateUDPRange(ctx context.Context, userID int64, min, max, size int,
	now time.Time) (int, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	var cur, curEnd sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT udp_port_start, udp_port_end FROM workspaces WHERE user_id = ?`,
		userID).Scan(&cur, &curEnd)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, 0, ErrNotFound
	case err != nil:
		return 0, 0, fmt.Errorf("store: read udp range: %w", err)
	}
	if cur.Valid && curEnd.Valid {
		return int(cur.Int64), int(curEnd.Int64), nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT udp_port_start, udp_port_end FROM workspaces
		WHERE udp_port_start IS NOT NULL ORDER BY udp_port_start`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: list udp ranges: %w", err)
	}
	var taken []portRange
	for rows.Next() {
		var r portRange
		if err := rows.Scan(&r.start, &r.end); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scan udp range: %w", err)
		}
		taken = append(taken, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("store: list udp ranges: %w", err)
	}
	rows.Close()

	start, ok := firstFreeRange(taken, min, max, size)
	if !ok {
		return 0, 0, ErrNoPortsFree
	}
	end := start + size - 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET udp_port_start = ?, udp_port_end = ?, updated_at = ? WHERE user_id = ?`,
		start, end, fmtTime(now), userID); err != nil {
		return 0, 0, fmt.Errorf("store: assign udp range: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: commit: %w", err)
	}
	return start, end, nil
}
