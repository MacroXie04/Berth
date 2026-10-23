package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFirstFreeRange(t *testing.T) {
	cases := []struct {
		name      string
		taken     []portRange
		min, max  int
		size      int
		wantStart int
		wantOK    bool
	}{
		{"empty", nil, 1000, 1099, 8, 1000, true},
		{"after the first block", []portRange{{1000, 1007}}, 1000, 1099, 8, 1008, true},
		{"gap in the middle", []portRange{{1000, 1007}, {1016, 1023}}, 1000, 1099, 8, 1008, true},
		{"gap too small", []portRange{{1000, 1007}, {1012, 1023}}, 1000, 1099, 8, 1024, true},
		{"leading gap", []portRange{{1010, 1017}}, 1000, 1099, 8, 1000, true},
		{"leading gap one short", []portRange{{1007, 1014}}, 1000, 1099, 8, 1015, true},
		{"exact fit at the end", []portRange{{1000, 1091}}, 1000, 1099, 8, 1092, true},
		{"one port short at the end", []portRange{{1000, 1092}}, 1000, 1099, 8, 0, false},
		{"full", []portRange{{1000, 1099}}, 1000, 1099, 8, 0, false},
		{"size larger than the whole range", nil, 1000, 1005, 8, 0, false},
		{"size equals the whole range", nil, 1000, 1007, 8, 1000, true},
		{"ranges outside the window are ignored", []portRange{{500, 600}}, 1000, 1099, 8, 1000, true},
		{"range straddling the window start", []portRange{{990, 1003}}, 1000, 1099, 8, 1004, true},
		{"size 1", []portRange{{1000, 1000}, {1001, 1001}}, 1000, 1002, 1, 1002, true},
		{"invalid size", nil, 1000, 1099, 0, 0, false},
		{"inverted bounds", nil, 1099, 1000, 8, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := firstFreeRange(tc.taken, tc.min, tc.max, tc.size)
			if ok != tc.wantOK || (ok && got != tc.wantStart) {
				t.Fatalf("firstFreeRange = %d, %v; want %d, %v", got, ok, tc.wantStart, tc.wantOK)
			}
		})
	}
}

func mustUser(t *testing.T, s *SQLite, name string) User {
	t.Helper()
	u, _, err := s.CreateUser(ctx, newUser(name), t0)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAllocateUDPRange(t *testing.T) {
	s := openTest(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")

	a0, a1, err := s.AllocateUDPRange(ctx, alice.ID, 30000, 30023, 8, t0)
	if err != nil || a0 != 30000 || a1 != 30007 {
		t.Fatalf("alice: %d-%d, %v", a0, a1, err)
	}
	// Idempotent: asking again returns the same block, not a new one.
	if b0, b1, err := s.AllocateUDPRange(ctx, alice.ID, 30000, 30023, 8, t0); err != nil || b0 != a0 || b1 != a1 {
		t.Fatalf("alice again: %d-%d, %v", b0, b1, err)
	}
	b0, b1, err := s.AllocateUDPRange(ctx, bob.ID, 30000, 30023, 8, t0)
	if err != nil || b0 != 30008 || b1 != 30015 {
		t.Fatalf("bob: %d-%d, %v", b0, b1, err)
	}
	c0, _, err := s.AllocateUDPRange(ctx, carol.ID, 30000, 30023, 8, t0)
	if err != nil || c0 != 30016 {
		t.Fatalf("carol: %d, %v", c0, err)
	}

	// The range is read back with the workspace.
	if w, _ := s.WorkspaceByUserID(ctx, bob.ID); w.UDPPortStart != 30008 || w.UDPPortEnd != 30015 {
		t.Fatalf("workspace range = %d-%d", w.UDPPortStart, w.UDPPortEnd)
	}

	// Releasing a block makes it the next one handed out.
	if _, err := s.TransitionWorkspace(ctx, bob.ID, []State{StateOffline}, StateOffline, t0, ReleaseUDPRange()); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.WorkspaceByUserID(ctx, bob.ID); w.UDPPortStart != 0 || w.UDPPortEnd != 0 {
		t.Fatalf("range not released: %d-%d", w.UDPPortStart, w.UDPPortEnd)
	}
	dave := mustUser(t, s, "dave")
	if d0, _, err := s.AllocateUDPRange(ctx, dave.ID, 30000, 30023, 8, t0); err != nil || d0 != 30008 {
		t.Fatalf("dave reuses bob's block: %d, %v", d0, err)
	}

	// Exhaustion is an error, not a silent overlap.
	erin := mustUser(t, s, "erin")
	if _, _, err := s.AllocateUDPRange(ctx, erin.ID, 30000, 30023, 8, t0); !errors.Is(err, ErrNoPortsFree) {
		t.Fatalf("err = %v, want ErrNoPortsFree", err)
	}
	if _, _, err := s.AllocateUDPRange(ctx, 4242, 30000, 30023, 8, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user err = %v, want ErrNotFound", err)
	}
}

func TestAllocateUDPRangeConcurrentNeverOverlaps(t *testing.T) {
	s := openTest(t)
	const n = 24
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = mustUser(t, s, fmt.Sprintf("user%02d", i)).ID
	}
	type res struct {
		start, end int
		err        error
	}
	out := make(chan res, n)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			a, b, err := s.AllocateUDPRange(ctx, id, 40000, 40000+n*8-1, 8, t0)
			out <- res{a, b, err}
		}(id)
	}
	wg.Wait()
	close(out)
	seen := map[int]bool{}
	for r := range out {
		if r.err != nil {
			t.Fatalf("allocation failed: %v", r.err)
		}
		for p := r.start; p <= r.end; p++ {
			if seen[p] {
				t.Fatalf("port %d handed out twice", p)
			}
			seen[p] = true
		}
	}
	if len(seen) != n*8 {
		t.Fatalf("allocated %d ports, want %d", len(seen), n*8)
	}
}

func TestTransitionWorkspace(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice")
	later := t0.Add(time.Minute)

	// offline -> starting, setting fields in the same statement
	w, err := s.TransitionWorkspace(ctx, u.ID, []State{StateOffline, StateStopped}, StateStarting, later,
		SetActiveAt(later), SetNekoPassword("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if w.State != StateStarting || w.NekoPassword != "secret" || w.LastActiveAt == nil || !w.LastActiveAt.Equal(later) ||
		!w.UpdatedAt.Equal(later) || w.ContainerID != "" {
		t.Fatalf("after claim: %+v", w)
	}

	// The same claim again loses: the workspace is no longer offline/stopped.
	if _, err := s.TransitionWorkspace(ctx, u.ID, []State{StateOffline, StateStopped}, StateStarting, later); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("second claim err = %v, want ErrStateConflict", err)
	}

	// Same-state update (record the container) keeps the state.
	w, err = s.TransitionWorkspace(ctx, u.ID, []State{StateStarting}, StateStarting, later, SetContainerID("abc123"))
	if err != nil || w.State != StateStarting || w.ContainerID != "abc123" {
		t.Fatalf("set container: %+v, %v", w, err)
	}

	// starting -> running -> stopped with a stop time
	if _, err := s.TransitionWorkspace(ctx, u.ID, []State{StateStarting}, StateRunning, later); err != nil {
		t.Fatal(err)
	}
	stopAt := later.Add(time.Hour)
	w, err = s.TransitionWorkspace(ctx, u.ID, []State{StateRunning, StateStarting}, StateStopped, stopAt, SetStoppedAt(stopAt))
	if err != nil || w.State != StateStopped || w.LastStoppedAt == nil || !w.LastStoppedAt.Equal(stopAt) || w.ContainerID != "abc123" {
		t.Fatalf("stop: %+v, %v", w, err)
	}

	// container gone: back to offline, container id cleared, ports released
	if _, _, err := s.AllocateUDPRange(ctx, u.ID, 30000, 30099, 8, later); err != nil {
		t.Fatal(err)
	}
	w, err = s.TransitionWorkspace(ctx, u.ID, []State{StateStopped}, StateOffline, stopAt, ClearContainer(), ReleaseUDPRange())
	if err != nil || w.State != StateOffline || w.ContainerID != "" || w.UDPPortStart != 0 {
		t.Fatalf("offline: %+v, %v", w, err)
	}
	if w.NekoPassword != "secret" {
		t.Fatalf("the password must survive container removal, got %q", w.NekoPassword)
	}

	if _, err := s.TransitionWorkspace(ctx, 9999, []State{StateOffline}, StateStarting, later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user err = %v, want ErrNotFound", err)
	}
	if _, err := s.TransitionWorkspace(ctx, u.ID, nil, StateStarting, later); err == nil {
		t.Fatal("empty source set accepted")
	}
}

// The compare-and-swap is what keeps a double-clicked "Power on" from creating
// two containers: exactly one concurrent claim may win.
func TestTransitionWorkspaceConcurrentClaimHasOneWinner(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice")
	const n = 16
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.TransitionWorkspace(ctx, u.ID, []State{StateOffline, StateStopped}, StateStarting, t0)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var won, lost int
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrStateConflict):
			lost++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || lost != n-1 {
		t.Fatalf("won=%d lost=%d, want 1/%d", won, lost, n-1)
	}
}

func TestTransitionRejectsInvalidState(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice")
	if _, err := s.TransitionWorkspace(ctx, u.ID, []State{StateOffline}, State("bogus"), t0); err == nil {
		t.Fatal("an invalid target state was accepted")
	}
	if w, _ := s.WorkspaceByUserID(ctx, u.ID); w.State != StateOffline {
		t.Fatalf("state changed to %q despite the error", w.State)
	}
}

func TestTouchLastActiveOnlyForLiveWorkspaces(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice")
	at := t0.Add(time.Minute)

	if err := s.TouchLastActive(ctx, u.ID, at); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.WorkspaceByUserID(ctx, u.ID); w.LastActiveAt != nil {
		t.Fatalf("an offline workspace recorded activity: %v", w.LastActiveAt)
	}
	for _, st := range []State{StateStarting, StateRunning} {
		s.db.Exec(`UPDATE workspaces SET state = ?, last_active_at = NULL WHERE user_id = ?`, st, u.ID)
		if err := s.TouchLastActive(ctx, u.ID, at); err != nil {
			t.Fatal(err)
		}
		if w, _ := s.WorkspaceByUserID(ctx, u.ID); w.LastActiveAt == nil || !w.LastActiveAt.Equal(at) {
			t.Fatalf("%s: LastActiveAt = %v, want %v", st, w.LastActiveAt, at)
		}
	}
	s.db.Exec(`UPDATE workspaces SET state = 'stopped', last_active_at = NULL WHERE user_id = ?`, u.ID)
	s.TouchLastActive(ctx, u.ID, at)
	if w, _ := s.WorkspaceByUserID(ctx, u.ID); w.LastActiveAt != nil {
		t.Fatalf("a stopped workspace recorded activity: %v", w.LastActiveAt)
	}
}

func TestWorkspaceSessions(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice")
	expires := t0.Add(time.Hour)
	if err := s.CreateSession(ctx, Session{TokenHash: "api1", WSTokenHash: "ws1", UserID: u.ID, CreatedAt: t0, ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}

	if got, err := s.WorkspaceSessionUser(ctx, "ws1", t0); err != nil || got.ID != u.ID {
		t.Fatalf("WorkspaceSessionUser = %+v, %v", got, err)
	}
	// The two tokens are not interchangeable.
	if _, err := s.WorkspaceSessionUser(ctx, "api1", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("API token accepted as workspace token: %v", err)
	}
	if _, err := s.SessionUser(ctx, "ws1", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("workspace token accepted as API token: %v", err)
	}
	if _, err := s.WorkspaceSessionUser(ctx, "ws1", expires); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired workspace token accepted: %v", err)
	}
	// Revoking by the API token kills the workspace token too.
	if err := s.DeleteSession(ctx, "api1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkspaceSessionUser(ctx, "ws1", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("workspace token survived logout: %v", err)
	}
}

func TestSessionsWithoutWorkspaceTokenCoexistAndNeverMatch(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice")
	for _, h := range []string{"a", "b"} { // two NULL ws hashes must not collide in the unique index
		if err := s.CreateSession(ctx, Session{TokenHash: h, UserID: u.ID, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}); err != nil {
			t.Fatalf("session %s: %v", h, err)
		}
	}
	// An empty token must never match a row whose workspace token is NULL.
	if _, err := s.WorkspaceSessionUser(ctx, "", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty workspace token matched: %v", err)
	}
	// A duplicate workspace token is refused.
	if err := s.CreateSession(ctx, Session{TokenHash: "c", WSTokenHash: "dup", UserID: u.ID, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, Session{TokenHash: "d", WSTokenHash: "dup", UserID: u.ID, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}); err == nil {
		t.Fatal("duplicate workspace token accepted")
	}
}

// A database written by the Milestone 1 binary (schema version 1) must upgrade
// in place without losing accounts, sessions or workspaces.
func TestUpgradeFromVersion1KeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "berth.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild a v1 database: drop what migration 2 added and rewind the version.
	for _, stmt := range []string{
		`DROP INDEX sessions_ws_token_hash`,
		`DROP INDEX workspaces_udp_port_start`,
		`ALTER TABLE sessions DROP COLUMN ws_token_hash`,
		`ALTER TABLE workspaces DROP COLUMN udp_port_start`,
		`ALTER TABLE workspaces DROP COLUMN udp_port_end`,
		`ALTER TABLE workspaces DROP COLUMN neko_password`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// Rows exactly as the v1 binary wrote them (it knew none of the new columns).
	for _, stmt := range []string{
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (1, 'alice', 'hash', 1, '2026-10-22 12:00:00')`,
		`INSERT INTO workspaces (user_id, state, volume_name, mem_limit_mb, cpu_limit, updated_at)
		   VALUES (1, 'offline', 'berth-data-alice', 2048, 2.0, '2026-10-22 12:00:00')`,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ('old', 1, '2026-10-22 12:00:00', '2026-10-22 13:00:00')`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	s.Close()

	s2, err := Open(ctx, path) // runs migration 2
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s2.Close()
	if u, err := s2.UserByUsername(ctx, "alice"); err != nil || !u.IsAdmin {
		t.Fatalf("user lost or changed: %+v, %v", u, err)
	}
	if u, err := s2.SessionUser(ctx, "old", t0); err != nil || u.Username != "alice" {
		t.Fatalf("v1 session lost: %+v, %v", u, err)
	}
	w, err := s2.WorkspaceByUserID(ctx, 1)
	if err != nil || w.State != StateOffline || w.VolumeName != "berth-data-alice" || w.UDPPortStart != 0 || w.NekoPassword != "" {
		t.Fatalf("workspace after upgrade: %+v, %v", w, err)
	}
	// The v1 session has no workspace token, so it can reach the API but never a workspace.
	if _, err := s2.WorkspaceSessionUser(ctx, "old", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("v1 session works as a workspace token: %v", err)
	}
	var v int
	s2.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if migs, _ := loadMigrations(migrationsFS); v != len(migs) {
		t.Fatalf("user_version = %d, want %d", v, len(migs))
	}
}
