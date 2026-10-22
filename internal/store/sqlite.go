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

func scanUser(r rowScanner) (User, error) {
	var u User
	if err := r.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt); err != nil {
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
	last_active_at, last_stopped_at, updated_at FROM workspaces`

func scanWorkspace(r rowScanner) (Workspace, error) {
	var (
		w                       Workspace
		containerID             sql.NullString
		lastActive, lastStopped sql.NullTime
	)
	err := r.Scan(&w.UserID, &containerID, &w.State, &w.VolumeName, &w.MemLimitMB, &w.CPULimit,
		&lastActive, &lastStopped, &w.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Workspace{}, ErrNotFound
		}
		return Workspace{}, fmt.Errorf("store: scan workspace: %w", err)
	}
	w.ContainerID = containerID.String
	w.LastActiveAt = nullTime(lastActive)
	w.LastStoppedAt = nullTime(lastStopped)
	w.UpdatedAt = w.UpdatedAt.UTC()
	return w, nil
}

func (s *SQLite) WorkspaceByUserID(ctx context.Context, userID int64) (Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx, workspaceSelect+` WHERE user_id = ?`, userID))
}

func (s *SQLite) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		sess.TokenHash, sess.UserID, fmtTime(sess.CreatedAt), fmtTime(sess.ExpiresAt))
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
