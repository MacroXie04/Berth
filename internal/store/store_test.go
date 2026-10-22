package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func openTest(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "data", "berth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newUser(name string) NewUser {
	return NewUser{
		Username:     name,
		PasswordHash: "hash-" + name,
		VolumeName:   "berth-data-" + name,
		MemLimitMB:   2048,
		CPULimit:     2.0,
	}
}

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "berth.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	migs, err := loadMigrations(migrationsFS)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(migs) {
		t.Fatalf("user_version = %d, want %d", v, len(migs))
	}
	if _, _, err := s.CreateUser(ctx, newUser("alice"), t0); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Reopening must not re-run migrations (which would fail on CREATE TABLE)
	// nor lose data.
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.UserByUsername(ctx, "alice"); err != nil {
		t.Fatalf("data lost across reopen: %v", err)
	}
}

func TestDatabaseFilesArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "berth.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.CreateUser(ctx, newUser("alice"), t0); err != nil { // forces WAL/SHM into existence
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %v is accessible to group/other", filepath.Base(p), fi.Mode().Perm())
		}
	}
}

func TestOpenPathWithURISpecialCharacters(t *testing.T) {
	// '%', '#' and '?' are special in SQLite URIs. Unescaped, SQLite would
	// open a different file (e.g. "we%ird") and leave ours empty.
	root := t.TempDir()
	path := filepath.Join(root, "we%ird#na?me", "berth.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := s.CreateUser(ctx, newUser("alice"), t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // checkpoints the WAL into the main file
		t.Fatal(err)
	}

	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("data did not land in the database at the literal path (stat err %v)", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "we%ird#na?me" {
		t.Fatalf("stray files next to the database directory: %v (err %v)", entries, err)
	}

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.UserByUsername(ctx, "alice"); err != nil {
		t.Fatalf("reopened database lost the data: %v", err)
	}
}

// Transactions must take the write lock when they begin (_txlock=immediate).
// Without it, a read-then-write transaction can lose a race with another
// writer and fail with a non-retryable SQLITE_BUSY_SNAPSHOT, which M2's state
// transitions would hit. A second handle with no busy timeout makes the
// difference deterministic: BEGIN IMMEDIATE fails at once while the lock is
// held; a deferred BEGIN would succeed.
func TestTransactionsTakeWriteLockUpFront(t *testing.T) {
	path := filepath.Join(t.TempDir(), "berth.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	other, err := sql.Open("sqlite", dsnWithBusyTimeout(path, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	holder, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()

	tx, err := other.BeginTx(ctx, nil)
	if err == nil {
		tx.Rollback()
		t.Fatal("BeginTx succeeded while another transaction holds the write lock: _txlock=immediate is not in effect")
	}
}

func TestMigrationFailureRollsBackCompletely(t *testing.T) {
	s := openTest(t)
	bad := migration{version: 99, name: "0099_bad.sql", sql: `CREATE TABLE half (x); CREATE TABLE half (x);`}
	if err := applyMigration(ctx, s.db, bad); err == nil {
		t.Fatal("expected migration error")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration left a table behind (n=%d, err=%v)", n, err)
	}
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v == 99 {
		t.Fatal("user_version advanced despite failure")
	}

	// A failed migration must also release its write lock. Other connections
	// cannot see uncommitted work under WAL, so the checks above would pass
	// even if the transaction were leaked open; a later write would not.
	done := make(chan error, 1)
	go func() {
		_, _, err := s.CreateUser(ctx, newUser("alice"), t0)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write after failed migration: %v (write lock leaked?)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write after failed migration blocked: the failed transaction was not rolled back")
	}
}

func TestCreateUserFirstIsAdminAndWorkspaceDefaults(t *testing.T) {
	s := openTest(t)

	u1, w1, err := s.CreateUser(ctx, newUser("alice"), t0)
	if err != nil {
		t.Fatal(err)
	}
	u2, _, err := s.CreateUser(ctx, newUser("bob"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if !u1.IsAdmin || u2.IsAdmin {
		t.Fatalf("admin flags: alice=%v bob=%v, want true/false", u1.IsAdmin, u2.IsAdmin)
	}
	if u1.ID == 0 || u1.ID == u2.ID || !u1.CreatedAt.Equal(t0) {
		t.Fatalf("bad user row: %+v", u1)
	}

	if w1.UserID != u1.ID || w1.State != StateOffline || w1.ContainerID != "" ||
		w1.VolumeName != "berth-data-alice" || w1.MemLimitMB != 2048 || w1.CPULimit != 2.0 ||
		w1.LastActiveAt != nil || w1.LastStoppedAt != nil || !w1.UpdatedAt.Equal(t0) {
		t.Fatalf("unexpected workspace: %+v", w1)
	}
	got, err := s.WorkspaceByUserID(ctx, u1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The row read back must equal what CreateUser returned (times have nil
	// pointers here, so a field-wise comparison is exact).
	if got.UserID != w1.UserID || got.State != w1.State || got.ContainerID != w1.ContainerID ||
		got.VolumeName != w1.VolumeName || got.MemLimitMB != w1.MemLimitMB || got.CPULimit != w1.CPULimit ||
		got.LastActiveAt != nil || got.LastStoppedAt != nil || !got.UpdatedAt.Equal(w1.UpdatedAt) {
		t.Fatalf("WorkspaceByUserID = %+v, want %+v", got, w1)
	}
}

func TestCreateUserDuplicateLeavesNoOrphans(t *testing.T) {
	s := openTest(t)
	if _, _, err := s.CreateUser(ctx, newUser("alice"), t0); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreateUser(ctx, newUser("alice"), t0)
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
	var users, workspaces int
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	s.db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&workspaces)
	if users != 1 || workspaces != 1 {
		t.Fatalf("users=%d workspaces=%d, want 1/1", users, workspaces)
	}
}

func TestCreateUserConcurrentExactlyOneAdmin(t *testing.T) {
	s := openTest(t)
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.CreateUser(ctx, newUser(fmt.Sprintf("user%02d", i)), t0)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent CreateUser: %v", err)
		}
	}
	var admins, total int
	s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&admins)
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&total)
	if admins != 1 || total != n {
		t.Fatalf("admins=%d total=%d, want 1/%d", admins, total, n)
	}
}

func TestCreateUserConcurrentSameUsername(t *testing.T) {
	s := openTest(t)
	const n = 12
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.CreateUser(ctx, newUser("alice"), t0)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var ok, taken int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrUsernameTaken):
			taken++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || taken != n-1 {
		t.Fatalf("ok=%d taken=%d, want 1/%d", ok, taken, n-1)
	}
}

func TestUserLookups(t *testing.T) {
	s := openTest(t)
	created, _, err := s.CreateUser(ctx, newUser("alice"), t0)
	if err != nil {
		t.Fatal(err)
	}
	byName, err := s.UserByUsername(ctx, "alice")
	if err != nil || byName != created {
		t.Fatalf("UserByUsername = %+v, %v; want %+v", byName, err, created)
	}
	byID, err := s.UserByID(ctx, created.ID)
	if err != nil || byID != created {
		t.Fatalf("UserByID = %+v, %v", byID, err)
	}
	for _, name := range []string{"ALICE", "alic", "nobody", ""} { // lookup is exact and case-sensitive
		if _, err := s.UserByUsername(ctx, name); !errors.Is(err, ErrNotFound) {
			t.Errorf("UserByUsername(%q) err = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := s.UserByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("UserByID(9999) err = %v, want ErrNotFound", err)
	}
	if _, err := s.WorkspaceByUserID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("WorkspaceByUserID(9999) err = %v, want ErrNotFound", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := openTest(t)
	u, _, err := s.CreateUser(ctx, newUser("alice"), t0)
	if err != nil {
		t.Fatal(err)
	}
	expires := t0.Add(time.Hour)
	if err := s.CreateSession(ctx, Session{TokenHash: "h1", UserID: u.ID, CreatedAt: t0, ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}

	got, err := s.SessionUser(ctx, "h1", t0.Add(time.Minute))
	if err != nil || got.ID != u.ID {
		t.Fatalf("SessionUser = %+v, %v", got, err)
	}
	// The expiry instant itself is already expired.
	if _, err := s.SessionUser(ctx, "h1", expires); !errors.Is(err, ErrNotFound) {
		t.Errorf("at expiry: err = %v, want ErrNotFound", err)
	}
	if _, err := s.SessionUser(ctx, "h1", expires.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Errorf("after expiry: err = %v, want ErrNotFound", err)
	}
	if _, err := s.SessionUser(ctx, "unknown", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token: err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteSession(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, "h1", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession(ctx, "h1"); err != nil { // idempotent
		t.Errorf("second delete: %v", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	s := openTest(t)
	u, _, _ := s.CreateUser(ctx, newUser("alice"), t0)
	add := func(hash string, exp time.Time) {
		if err := s.CreateSession(ctx, Session{TokenHash: hash, UserID: u.ID, CreatedAt: t0, ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	add("old1", t0.Add(-time.Hour))
	add("old2", t0) // expires exactly now: counts as expired
	add("live", t0.Add(time.Hour))

	n, err := s.DeleteExpiredSessions(ctx, t0)
	if err != nil || n != 2 {
		t.Fatalf("DeleteExpiredSessions = %d, %v; want 2", n, err)
	}
	if _, err := s.SessionUser(ctx, "live", t0); err != nil {
		t.Fatalf("live session was deleted: %v", err)
	}
}

func TestSessionRequiresExistingUser(t *testing.T) {
	s := openTest(t)
	err := s.CreateSession(ctx, Session{TokenHash: "h", UserID: 4242, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)})
	if err == nil {
		t.Fatal("session for a nonexistent user was accepted; foreign keys are not enforced")
	}
}

func TestWorkspaceStateIsConstrained(t *testing.T) {
	s := openTest(t)
	u, _, _ := s.CreateUser(ctx, newUser("alice"), t0)
	if _, err := s.db.Exec(`UPDATE workspaces SET state = 'bogus' WHERE user_id = ?`, u.ID); err == nil {
		t.Fatal("invalid state accepted")
	}
	for _, st := range []State{StateOffline, StateStarting, StateRunning, StateStopped} {
		if _, err := s.db.Exec(`UPDATE workspaces SET state = ? WHERE user_id = ?`, st, u.ID); err != nil {
			t.Errorf("state %q rejected: %v", st, err)
		}
	}
}

// Timestamps must be stored in the fixed-width form even when the caller's
// time carries a monotonic reading, a zone, or sub-second precision.
func TestTimestampsStoredFixedWidthUTC(t *testing.T) {
	s := openTest(t)
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Now().In(loc) // has monotonic reading, non-UTC zone, nanoseconds
	u, w, err := s.CreateUser(ctx, newUser("alice"), now)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT CAST(created_at AS TEXT) FROM users WHERE id = ?`, u.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`).MatchString(raw) {
		t.Fatalf("created_at stored as %q", raw)
	}
	want := now.UTC().Truncate(time.Second)
	if !u.CreatedAt.Equal(want) || u.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt = %v (%v), want %v UTC", u.CreatedAt, u.CreatedAt.Location(), want)
	}
	if !w.UpdatedAt.Equal(want) {
		t.Fatalf("UpdatedAt = %v, want %v", w.UpdatedAt, want)
	}
}

// Only a UNIQUE violation means "username taken". Any other constraint failure
// (CHECK, NOT NULL) is a server fault and must not be reported to a user as a
// name clash, and it must leave no half-created account behind.
func TestCreateUserOtherConstraintFailuresAreNotUsernameTaken(t *testing.T) {
	bad := map[string]func(*NewUser){
		"mem limit zero (CHECK)":     func(n *NewUser) { n.MemLimitMB = 0 },
		"cpu limit negative (CHECK)": func(n *NewUser) { n.CPULimit = -1 },
		"cpu limit NaN (NOT NULL)":   func(n *NewUser) { n.CPULimit = math.NaN() },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			s := openTest(t)
			nu := newUser("alice")
			mutate(&nu)
			_, _, err := s.CreateUser(ctx, nu, t0)
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, ErrUsernameTaken) {
				t.Fatalf("constraint failure misreported as ErrUsernameTaken: %v", err)
			}
			var users, workspaces int
			s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
			s.db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&workspaces)
			if users != 0 || workspaces != 0 {
				t.Fatalf("failed CreateUser left rows behind: users=%d workspaces=%d", users, workspaces)
			}
		})
	}
}

// The workspace insert can hit its own UNIQUE index (volume_name); that is also
// a name clash, and the user row inserted just before it must be rolled back.
func TestCreateUserVolumeNameCollisionRollsBackUser(t *testing.T) {
	s := openTest(t)
	if _, _, err := s.CreateUser(ctx, newUser("alice"), t0); err != nil {
		t.Fatal(err)
	}
	clash := newUser("bob")
	clash.VolumeName = "berth-data-alice"
	if _, _, err := s.CreateUser(ctx, clash, t0); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
	if _, err := s.UserByUsername(ctx, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob's user row survived the failed workspace insert: %v", err)
	}
}

// Nothing in the backend writes these columns yet (M2-M4 will), so populate
// them directly and make sure the scan path returns exact, UTC values.
func TestWorkspacePopulatedColumnsAreScanned(t *testing.T) {
	s := openTest(t)
	u, _, _ := s.CreateUser(ctx, newUser("alice"), t0)
	if _, err := s.db.Exec(`UPDATE workspaces SET container_id = 'abc123', state = 'stopped',
		last_active_at = '2026-10-22 12:00:00', last_stopped_at = '2026-10-21 08:30:00'
		WHERE user_id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkspaceByUserID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.ContainerID != "abc123" || w.State != StateStopped {
		t.Fatalf("container/state = %q/%q", w.ContainerID, w.State)
	}
	wantActive := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	wantStopped := time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC)
	if w.LastActiveAt == nil || !w.LastActiveAt.Equal(wantActive) || w.LastActiveAt.Location() != time.UTC {
		t.Fatalf("LastActiveAt = %v", w.LastActiveAt)
	}
	if w.LastStoppedAt == nil || !w.LastStoppedAt.Equal(wantStopped) || w.LastStoppedAt.Location() != time.UTC {
		t.Fatalf("LastStoppedAt = %v", w.LastStoppedAt)
	}
}

// A database written by a newer binary must be refused, not opened silently.
func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "berth.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	migs, _ := loadMigrations(migrationsFS)
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migs)+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(ctx, path)
	if err == nil {
		s2.Close()
		t.Fatal("Open accepted a database from a newer schema version")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
}

func TestDatabaseDirectoryIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh", "state")
	s, err := Open(ctx, filepath.Join(dir, "berth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database directory mode %v is accessible to group/other", fi.Mode().Perm())
	}
}
