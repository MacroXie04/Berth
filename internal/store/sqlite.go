package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQLite implements Store on a SQLite database file (pure-Go driver, no cgo).
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if needed) the database at path and applies any
// pending migrations.
func Open(ctx context.Context, path string) (*SQLite, error) {
	// The directory holds password hashes: keep it private to the service user.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create db directory: %w", err)
	}
	// Create the file ourselves, owner-only: SQLite gives the -wal and -shm
	// files the same mode as the main database file, and the whole set holds
	// password hashes. An empty file is a valid empty database.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create db file: %w", err)
	}
	f.Close()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// busyTimeout is how long a connection waits for another writer's lock.
const busyTimeout = 5 * time.Second

// dsn builds a SQLite URI. WAL + busy_timeout let readers (every ForwardAuth
// request) proceed alongside writers; _txlock=immediate makes every
// transaction take the write lock up front, so read-then-write sequences such
// as "first user becomes admin" cannot interleave.
func dsn(path string) string { return dsnWithBusyTimeout(path, busyTimeout) }

func dsnWithBusyTimeout(path string, busy time.Duration) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	return "file:" + escaped + "?" + q.Encode()
}

func (s *SQLite) Close() error { return s.db.Close() }

// Timestamps are stored as fixed-width UTC text. Never bind a time.Time
// directly: the driver would store its String() form, including the monotonic
// clock reading ("m=+0.01"), which neither parses nor sorts.
const timeLayout = "2006-01-02 15:04:05"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func (s *SQLite) CreateUser(ctx context.Context, nu NewUser, now time.Time) (User, Workspace, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, Workspace{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	ts := fmtTime(now)
	// is_admin is computed inside the INSERT so "first user" is decided under
	// the write lock held by this immediate transaction.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin, created_at)
		 VALUES (?, ?, (SELECT COUNT(*) = 0 FROM users), ?)`,
		nu.Username, nu.PasswordHash, ts)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, Workspace{}, ErrUsernameTaken
		}
		return User{}, Workspace{}, fmt.Errorf("store: insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, Workspace{}, fmt.Errorf("store: user id: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO workspaces (user_id, state, volume_name, mem_limit_mb, cpu_limit, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, StateOffline, nu.VolumeName, nu.MemLimitMB, nu.CPULimit, ts)
	if err != nil {
		if isUniqueViolation(err) { // volume_name collides only if the username does
			return User{}, Workspace{}, ErrUsernameTaken
		}
		return User{}, Workspace{}, fmt.Errorf("store: insert workspace: %w", err)
	}
	user, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE id = ?`, id))
	if err != nil {
		return User{}, Workspace{}, err
	}
	ws, err := scanWorkspace(tx.QueryRowContext(ctx, workspaceSelect+` WHERE user_id = ?`, id))
	if err != nil {
		return User{}, Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, Workspace{}, fmt.Errorf("store: commit: %w", err)
	}
	return user, ws, nil
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

const userSelect = `SELECT id, username, password_hash, is_admin, created_at FROM users`

// userDest lists the destinations of userSelect's columns, in order.
func userDest(u *User) []any {
	return []any{&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt}
}

func scanUser(r rowScanner) (User, error) {
	var u User
	if err := r.Scan(userDest(&u)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("store: scan user: %w", err)
	}
	u.CreatedAt = u.CreatedAt.UTC()
	return u, nil
}

func (s *SQLite) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE username = ?`, username))
}

func (s *SQLite) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE id = ?`, id))
}

const workspaceSelect = `SELECT user_id, container_id, state, volume_name, mem_limit_mb, cpu_limit,
	last_active_at, last_stopped_at, updated_at, udp_port_start, udp_port_end, neko_password FROM workspaces`

// workspaceRow holds a workspace's columns as the driver returns them (NULLable
// ones as sql.Null*), in the order of workspaceSelect.
type workspaceRow struct {
	w                       Workspace
	containerID, nekoPW     sql.NullString
	lastActive, lastStopped sql.NullTime
	udpStart, udpEnd        sql.NullInt64
}

func (r *workspaceRow) dest() []any {
	return []any{&r.w.UserID, &r.containerID, &r.w.State, &r.w.VolumeName, &r.w.MemLimitMB, &r.w.CPULimit,
		&r.lastActive, &r.lastStopped, &r.w.UpdatedAt, &r.udpStart, &r.udpEnd, &r.nekoPW}
}

func (r *workspaceRow) workspace() Workspace {
	w := r.w
	w.ContainerID = r.containerID.String
	w.NekoPassword = r.nekoPW.String
	w.UDPPortStart, w.UDPPortEnd = int(r.udpStart.Int64), int(r.udpEnd.Int64) // NULL scans as 0
	w.LastActiveAt = nullTime(r.lastActive)
	w.LastStoppedAt = nullTime(r.lastStopped)
	w.UpdatedAt = w.UpdatedAt.UTC()
	return w
}

func scanWorkspace(r rowScanner) (Workspace, error) {
	var row workspaceRow
	if err := r.Scan(row.dest()...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Workspace{}, ErrNotFound
		}
		return Workspace{}, fmt.Errorf("store: scan workspace: %w", err)
	}
	return row.workspace(), nil
}

func scanUserWorkspace(r rowScanner, uw *UserWorkspace) error {
	var row workspaceRow
	if err := r.Scan(append(userDest(&uw.User), row.dest()...)...); err != nil {
		return fmt.Errorf("store: scan workspace: %w", err)
	}
	uw.User.CreatedAt = uw.User.CreatedAt.UTC()
	uw.Workspace = row.workspace()
	return nil
}

func (s *SQLite) WorkspaceByUserID(ctx context.Context, userID int64) (Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx, workspaceSelect+` WHERE user_id = ?`, userID))
}

func (s *SQLite) ListWorkspaces(ctx context.Context) ([]UserWorkspace, error) {
	// One statement, so the user and workspace of a row are read together.
	// The password hash and the neko password are not selected (their fields come
	// back empty): a listing of everyone has no use for them, and a secret that is
	// never loaded cannot end up in a log line or a response.
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.username, '', u.is_admin, u.created_at,
		w.user_id, w.container_id, w.state, w.volume_name, w.mem_limit_mb, w.cpu_limit,
		w.last_active_at, w.last_stopped_at, w.updated_at, w.udp_port_start, w.udp_port_end, NULL
		FROM users u JOIN workspaces w ON w.user_id = u.id ORDER BY u.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	defer rows.Close()
	var out []UserWorkspace
	for rows.Next() {
		var uw UserWorkspace
		if err := scanUserWorkspace(rows, &uw); err != nil {
			return nil, err
		}
		out = append(out, uw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	return out, nil
}

func (s *SQLite) CreateSession(ctx context.Context, sess Session) error {
	var wsHash any // NULL, not "", so the partial unique index ignores it
	if sess.WSTokenHash != "" {
		wsHash = sess.WSTokenHash
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, ws_token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		sess.TokenHash, wsHash, sess.UserID, fmtTime(sess.CreatedAt), fmtTime(sess.ExpiresAt))
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

func (s *SQLite) SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.password_hash, u.is_admin, u.created_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ? AND s.expires_at > ?`,
		tokenHash, fmtTime(now)))
}

func (s *SQLite) WorkspaceSessionUser(ctx context.Context, wsTokenHash string, now time.Time) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.password_hash, u.is_admin, u.created_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.ws_token_hash = ? AND s.expires_at > ?`,
		wsTokenHash, fmtTime(now)))
}

func (s *SQLite) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

func (s *SQLite) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, fmtTime(now))
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
