package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logBuf collects log output so tests can count what was said.
type logBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.Write(p)
}

func (l *logBuf) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.sb.String(), sub)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.String()
}

type rig struct {
	t    *testing.T
	st   *store.SQLite
	dock *docker.Fake
	m    *workspace.Manager
	clk  *clock
	r    *Reaper
	logs *logBuf
}

func defaultConfig() Config {
	return Config{
		IdleTimeout:    30 * time.Minute,
		ReclaimAfter:   7 * 24 * time.Hour,
		Interval:       time.Minute,
		AttachInterval: 10 * time.Second,
		IdleCPUPercent: 5,
	}
}

func newRig(t *testing.T, mutate ...func(*Config)) *rig {
	t.Helper()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "berth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &clock{t: t0}
	dock := docker.NewFake(clk.Now)
	dock.AddContainer("berth-traefik")
	logs := &logBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m, err := workspace.NewManager(st, dock, workspace.Config{
		Image: "berth/workspace:latest", TraefikContainer: "berth-traefik", Entrypoint: "web",
		UDPPortMin: 30000, UDPPortMax: 30099, UDPPortsPerWorkspace: 1,
		Resolution: "1280x720@30", ShmSizeMB: 2048, PidsLimit: 2048,
		StopTimeout: 15 * time.Second, StartTimeout: 30 * time.Second,
	}, log, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	for _, f := range mutate {
		f(&cfg)
	}
	r, err := New(st, m, dock, cfg, log, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, st: st, dock: dock, m: m, clk: clk, r: r, logs: logs}
}

func (g *rig) user(name string) store.User {
	g.t.Helper()
	u, _, err := g.st.CreateUser(ctx, store.NewUser{
		Username: name, PasswordHash: "x", VolumeName: workspace.VolumeName(name), MemLimitMB: 2048, CPULimit: 2,
	}, t0)
	if err != nil {
		g.t.Fatal(err)
	}
	return u
}

// up powers a new user on and lets the healthcheck pass.
func (g *rig) up(name string) (store.User, string) {
	g.t.Helper()
	u := g.user(name)
	if _, err := g.m.Start(ctx, u); err != nil {
		g.t.Fatal(err)
	}
	id := g.dock.ContainerIDByName(workspace.ContainerName(name))
	g.dock.SetHealth(id, docker.HealthHealthy)
	if st, err := g.m.Status(ctx, u); err != nil || st.Workspace.State != store.StateRunning {
		g.t.Fatalf("%s is not running: %+v, %v", name, st.Workspace, err)
	}
	return u, id
}

func (g *rig) ws(u store.User) store.Workspace {
	g.t.Helper()
	w, err := g.st.WorkspaceByUserID(ctx, u.ID)
	if err != nil {
		g.t.Fatal(err)
	}
	return w
}

func (g *rig) running(id string) bool {
	g.t.Helper()
	info, err := g.dock.InspectContainer(ctx, id)
	if err != nil {
		g.t.Fatal(err)
	}
	return info.Running
}

// pass advances the clock by d and sweeps once, like one tick of the loop.
func (g *rig) pass(d time.Duration) {
	g.clk.Advance(d)
	g.r.Sweep(ctx)
}

func TestNewRejectsABadConfig(t *testing.T) {
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "b.db"))
	defer st.Close()
	bad := map[string]func(*Config){
		"no idle timeout":     func(c *Config) { c.IdleTimeout = 0 },
		"negative idle":       func(c *Config) { c.IdleTimeout = -time.Minute },
		"no reclaim age":      func(c *Config) { c.ReclaimAfter = 0 },
		"no interval":         func(c *Config) { c.Interval = 0 },
		"no attach interval":  func(c *Config) { c.AttachInterval = 0 },
		"negative cpu":        func(c *Config) { c.IdleCPUPercent = -1 },
		"negative concurrent": func(c *Config) { c.Concurrency = -1 },
		"negative op timeout": func(c *Config) { c.OpTimeout = -time.Second },
	}
	for name, mutate := range bad {
		cfg := defaultConfig()
		mutate(&cfg)
		if _, err := New(st, nil, nil, cfg, nil, nil); err == nil {
			t.Errorf("%s: New accepted an invalid config", name)
		}
	}
}

// ---- idle stop --------------------------------------------------------------

func TestAnUnusedWorkspaceIsStoppedAtTheIdleTimeoutAndNotBefore(t *testing.T) {
	g := newRig(t)
	u, id := g.up("alice")

	for i := 1; i < 30; i++ { // minutes 1..29: still within the timeout
		g.pass(time.Minute)
		if !g.running(id) {
			t.Fatalf("stopped after %d minutes, before the 30-minute timeout", i)
		}
	}
	if g.ws(u).State != store.StateRunning {
		t.Fatalf("state = %s", g.ws(u).State)
	}

	g.pass(time.Minute) // minute 30
	if g.running(id) {
		t.Fatal("still running 30 minutes after the last activity")
	}
	ws := g.ws(u)
	if ws.State != store.StateStopped || ws.ContainerID != id {
		t.Errorf("workspace = %+v, want stopped with its container kept", ws)
	}
	if ws.LastStoppedAt == nil || !ws.LastStoppedAt.Equal(g.clk.Now()) {
		t.Errorf("last_stopped_at = %v, want %v", ws.LastStoppedAt, g.clk.Now())
	}
	if !g.dock.HasVolume(workspace.VolumeName("alice")) {
		t.Error("the data volume is gone")
	}
	if g.logs.count("stopped an idle workspace") != 1 {
		t.Errorf("want one log line about the stop:\n%s", g.logs)
	}
	// Idempotent: later passes leave the stopped workspace alone.
	g.pass(time.Minute)
	if g.dock.CallCount("StopContainer "+id) != 1 {
		t.Errorf("StopContainer called %d times", g.dock.CallCount("StopContainer "+id))
	}
}

func TestHeartbeatsKeepAWorkspaceAlive(t *testing.T) {
	g := newRig(t)
	u, id := g.up("alice")

	for i := 0; i < 24; i++ { // four hours of sweeps; a heartbeat every 10 minutes
		for j := 0; j < 10; j++ {
			g.pass(time.Minute)
		}
		if err := g.m.Heartbeat(ctx, u); err != nil {
			t.Fatal(err)
		}
		if !g.running(id) {
			t.Fatalf("stopped after %d heartbeats", i)
		}
	}
	// The tab closes: stopped 30 minutes after the last heartbeat.
	for i := 1; i < 30; i++ {
		g.pass(time.Minute)
		if !g.running(id) {
			t.Fatalf("stopped %d minutes after the last heartbeat", i)
		}
	}
	g.pass(time.Minute)
	if g.running(id) {
		t.Fatal("still running 30 minutes after the last heartbeat")
	}
}

func TestABusyWorkspaceIsNotStoppedAndAnIdleOneFollowsOnce(t *testing.T) {
	g := newRig(t)
	u, id := g.up("alice")
	started := g.ws(u).LastActiveAt
	g.dock.SetCPUCores(id, 0.5) // 50% of a core, no heartbeats at all

	for i := 0; i < 180; i++ { // three hours
		g.pass(time.Minute)
	}
	if !g.running(id) {
		t.Fatal("a busy workspace was stopped")
	}
	// The verdict lives in the reaper's memory: last_active_at keeps meaning
	// "the user was here", and a busy workspace costs no database write per pass.
	if now := g.ws(u).LastActiveAt; now == nil || !now.Equal(*started) {
		t.Errorf("last_active_at = %v, want it untouched (%v): CPU load is not user activity", now, started)
	}
	if n := g.logs.count("workspace is busy"); n != 1 {
		t.Errorf("the busy verdict was logged %d times in 3 hours, want once (a change):\n%s", n, g.logs)
	}

	g.dock.SetCPUCores(id, 0.02) // the job finishes; 2% is background noise
	start := g.clk.Now()
	for g.running(id) {
		g.pass(time.Minute)
		if g.clk.Now().Sub(start) > 40*time.Minute {
			t.Fatal("an idle workspace was never stopped")
		}
	}
	if d := g.clk.Now().Sub(start); d < 29*time.Minute || d > 32*time.Minute {
		t.Errorf("stopped %v after the load ended, want about the 30-minute timeout", d)
	}
	if g.logs.count("no longer busy") != 1 {
		t.Errorf("want one line when the load ended:\n%s", g.logs)
	}
}

// The reading is an average over the whole interval between two looks, not a
// one-second sample: a short burst in a quiet minute stays under the threshold,
// a longer one does not.
func TestCPUIsAveragedOverTheIntervalAndTheThresholdIsInclusive(t *testing.T) {
	cases := []struct {
		name  string
		burst time.Duration // one core fully busy for this long in a 60 s interval
		busy  bool
	}{
		{"2 s of 60 is 3.3 percent", 2 * time.Second, false},
		{"just under 5 percent", 2900 * time.Millisecond, false},
		{"exactly 5 percent", 3 * time.Second, true},
		{"4 s of 60 is 6.7 percent", 4 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.up("alice")
			g.pass(time.Minute) // the baseline reading
			id := g.dock.ContainerIDByName(workspace.ContainerName("alice"))

			g.dock.SetCPUCores(id, 1)
			g.clk.Advance(tc.burst)
			g.dock.SetCPUCores(id, 0)
			g.pass(time.Minute - tc.burst)

			if got := g.logs.count("workspace is busy") == 1; got != tc.busy {
				t.Errorf("burst %v: busy = %v, want %v\n%s", tc.burst, got, tc.busy, g.logs)
			}
		})
	}
}

func TestTheCPUCheckCanBeSwitchedOff(t *testing.T) {
	g := newRig(t, func(c *Config) { c.IdleCPUPercent = 0 })
	_, id := g.up("alice")
	g.dock.SetCPUCores(id, 1.5)
	for i := 0; i < 30; i++ {
		g.pass(time.Minute)
	}
	if g.running(id) {
		t.Error("with the CPU check off, a busy but unattended workspace is stopped at the timeout")
	}
	if n := g.dock.CallCount("CPUUsage"); n != 0 {
		t.Errorf("CPUUsage called %d times with the check off", n)
	}
}

// With no answer about the load, nothing is stopped on a guess.
func TestAFailedCPUReadingStopsNobodyThatPass(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	g.dock.FailNext("CPUUsage", errors.New("daemon hiccup"))
	g.pass(time.Minute) // minute 30: idle, but the load is unknown
	if !g.running(id) {
		t.Fatal("stopped without knowing the load")
	}
	g.pass(time.Minute)
	if g.running(id) {
		t.Fatal("not stopped once the load was known to be low")
	}
}

// The first look at a container has nothing to compare with.
func TestTheFirstLookAtAContainerNeverStopsIt(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	g.clk.Advance(2 * time.Hour) // way past the timeout, and the reaper has not seen it yet
	g.r.floor = t0               // as if this process had been up the whole time
	g.r.Sweep(ctx)
	if !g.running(id) {
		t.Fatal("stopped on a first reading")
	}
	g.pass(time.Minute)
	if g.running(id) {
		t.Fatal("not stopped on the second pass")
	}
}

// A restarted backend has missed every heartbeat of its downtime.
func TestARestartedBackendGivesEveryoneAFullTimeoutToHeartbeatAgain(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	g.clk.Advance(3 * time.Hour) // the backend was down; the heartbeats failed
	r, err := New(g.st, g.m, g.dock, defaultConfig(), nil, g.clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	g.r = r

	for i := 1; i < 30; i++ {
		g.pass(time.Minute)
		if !g.running(id) {
			t.Fatalf("stopped %d minutes after the restart, though the user may be heartbeating again", i)
		}
	}
	g.pass(time.Minute)
	if g.running(id) {
		t.Fatal("never stopped after a full timeout without a heartbeat")
	}
}

// A heartbeat that arrives after the sweep read the list but before the stop must win.
func TestAHeartbeatBetweenTheDecisionAndTheStopWins(t *testing.T) {
	g := newRig(t)
	u, id := g.up("alice")
	for i := 0; i < 30; i++ { // idle by the policy, one pass before the stop would happen
		g.clk.Advance(time.Minute)
		if i < 29 {
			g.r.Sweep(ctx)
		}
	}
	// The sweep lists the workspaces (it thinks alice is idle)... then her page beats.
	g.dock.SetHook("InspectContainer", func(context.Context) {
		g.dock.SetHook("InspectContainer", nil)
		if err := g.m.Heartbeat(ctx, u); err != nil {
			t.Error(err)
		}
	})
	g.r.Sweep(ctx)
	if !g.running(id) || g.ws(u).State != store.StateRunning {
		t.Fatalf("a user with a fresh heartbeat was stopped: %+v", g.ws(u))
	}
}

// A container that never turns healthy leaves the row at `starting`.
func TestAStartThatNeverBecameHealthyIsReapedWhenNobodyUsesIt(t *testing.T) {
	g := newRig(t)
	u := g.user("alice")
	if _, err := g.m.Start(ctx, u); err != nil {
		t.Fatal(err)
	}
	id := g.dock.ContainerIDByName(workspace.ContainerName("alice"))
	for i := 1; i < 30; i++ {
		g.pass(time.Minute)
		if !g.running(id) {
			t.Fatalf("a start in progress was stopped after %d minutes", i)
		}
	}
	g.pass(time.Minute)
	if g.running(id) || g.ws(u).State != store.StateStopped {
		t.Fatalf("workspace = %+v, want the stuck start stopped", g.ws(u))
	}
}

func TestOnlyTheIdleWorkspaceOfSeveralIsStopped(t *testing.T) {
	g := newRig(t)
	alice, aliceID := g.up("alice")
	_, bobID := g.up("bob")
	carol, carolID := g.up("carol")
	g.dock.SetCPUCores(carolID, 1) // busy

	for i := 0; i < 40; i++ {
		g.pass(time.Minute)
		if i%10 == 9 {
			if err := g.m.Heartbeat(ctx, alice); err != nil { // alice's tab stays open
				t.Fatal(err)
			}
		}
	}
	if !g.running(aliceID) {
		t.Error("alice has a heartbeat; she must still be running")
	}
	if g.running(bobID) {
		t.Error("bob is idle; he must be stopped")
	}
	if !g.running(carolID) {
		t.Error("carol is busy; she must still be running")
	}
	_ = carol
}

// ---- reclaim ------------------------------------------------------------------

func TestAWorkspaceStoppedForTheReclaimAgeLosesItsContainerButNotItsData(t *testing.T) {
	g := newRig(t, func(c *Config) { c.IdleTimeout = time.Hour })
	u, id := g.up("alice")
	if _, err := g.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	stoppedAt := g.clk.Now()
	pw := g.ws(u).NekoPassword

	g.pass(7*24*time.Hour - time.Minute)
	if _, err := g.dock.InspectContainer(ctx, id); err != nil {
		t.Fatalf("removed a minute before the age: %v", err)
	}
	if g.ws(u).State != store.StateStopped {
		t.Fatalf("state = %s", g.ws(u).State)
	}

	g.pass(time.Minute) // exactly 7 days since the stop
	if _, err := g.dock.InspectContainer(ctx, id); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("container after the reclaim age: %v", err)
	}
	ws := g.ws(u)
	if ws.State != store.StateOffline || ws.ContainerID != "" || ws.UDPPortStart != 0 {
		t.Errorf("workspace = %+v, want offline, no container, no port block", ws)
	}
	if !g.dock.HasVolume(workspace.VolumeName("alice")) || ws.VolumeName != workspace.VolumeName("alice") {
		t.Error("the data volume must survive")
	}
	if ws.NekoPassword != pw || ws.LastStoppedAt == nil || !ws.LastStoppedAt.Equal(stoppedAt) {
		t.Errorf("password and stop time are kept: %+v", ws)
	}

	// The user comes back: a new container on the same volume.
	if _, err := g.m.Start(ctx, u); err != nil {
		t.Fatal(err)
	}
	spec, ok := g.dock.Spec(g.dock.ContainerIDByName(workspace.ContainerName("alice")))
	if !ok || len(spec.Volumes) != 1 || spec.Volumes[0].Volume != workspace.VolumeName("alice") {
		t.Errorf("power-on after a reclaim: %+v", spec.Volumes)
	}
}

func TestReclaimNeverTouchesRunningOrRecentlyStoppedWorkspaces(t *testing.T) {
	g := newRig(t, func(c *Config) { c.IdleTimeout = 1000 * time.Hour })
	_, runID := g.up("runner")
	stopped, stopID := g.up("recent")
	if _, err := g.m.Stop(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		g.pass(24 * time.Hour)
	}
	if !g.running(runID) {
		t.Error("a running workspace lost its container")
	}
	if _, err := g.dock.InspectContainer(ctx, stopID); err != nil {
		t.Errorf("a workspace stopped 5 days ago lost its container: %v", err)
	}
	if g.dock.RemovedWhileRunning() != 0 {
		t.Error("a running container was removed")
	}
}

func TestAWorkspaceStoppedByTheIdleRuleIsReclaimedAWeekLater(t *testing.T) {
	g := newRig(t)
	u, id := g.up("alice")
	for g.running(id) {
		g.pass(time.Minute)
		if g.clk.Now().Sub(t0) > 2*time.Hour {
			t.Fatal("never stopped")
		}
	}
	stopped := *g.ws(u).LastStoppedAt
	for g.clk.Now().Sub(stopped) < 7*24*time.Hour-time.Hour {
		g.pass(time.Hour)
	}
	if _, err := g.dock.InspectContainer(ctx, id); err != nil {
		t.Fatalf("removed too early: %v", err)
	}
	g.pass(time.Hour)
	if _, err := g.dock.InspectContainer(ctx, id); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("not removed a week after the stop: %v", err)
	}
	if g.ws(u).State != store.StateOffline {
		t.Errorf("state = %s", g.ws(u).State)
	}
}

// ---- reconcile ----------------------------------------------------------------

func TestEveryPassBringsTheDatabaseInLineWithDocker(t *testing.T) {
	g := newRig(t)
	alice, aliceID := g.up("alice")
	bob, bobID := g.up("bob")
	g.up("carol") // one that stays running through the pass: it must not be sampled either
	g.dock.Crash(aliceID, 137)
	g.dock.Vanish(bobID)

	before := g.dock.CallCount("ContainerStats")
	g.pass(time.Minute)

	if ws := g.ws(alice); ws.State != store.StateStopped {
		t.Errorf("a crashed container: state %s, want stopped", ws.State)
	}
	if ws := g.ws(bob); ws.State != store.StateOffline || ws.ContainerID != "" || ws.UDPPortStart != 0 {
		t.Errorf("a container removed by hand: %+v, want offline with its port block released", ws)
	}
	if n := g.dock.CallCount("ContainerStats") - before; n != 0 {
		t.Errorf("a pass took %d slow resource samples; it must only read cumulative CPU", n)
	}
}

func TestAContainerStartedByHandGetsAFreshIdleClock(t *testing.T) {
	g := newRig(t)
	u, id := g.up("alice")
	for g.running(id) {
		g.pass(time.Minute)
		if g.clk.Now().Sub(t0) > 2*time.Hour {
			t.Fatal("never stopped")
		}
	}
	g.clk.Advance(48 * time.Hour)
	if err := g.dock.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	g.dock.SetHealth(id, docker.HealthHealthy)
	g.pass(time.Minute) // noticed: resumed
	g.pass(time.Minute) // promoted to running
	if !g.running(id) {
		t.Fatalf("a container someone just started was stopped at once: %+v", g.ws(u))
	}
}

// ---- containers the database does not know ---------------------------------------

func TestUnaccountedContainersAreReportedOnceAndNeverRemoved(t *testing.T) {
	g := newRig(t)
	g.user("alice")
	ghost, err := g.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name: "berth-ws-ghost", Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: "ghost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	leftover, err := g.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name: workspace.ContainerName("alice"), Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dock.StartContainer(ctx, ghost); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		g.pass(time.Minute)
	}
	if g.logs.count("belongs to a user the database does not know") != 1 {
		t.Errorf("want the unknown-user container reported once:\n%s", g.logs)
	}
	if g.logs.count("is not recorded in the database") != 1 {
		t.Errorf("want the unrecorded container reported once:\n%s", g.logs)
	}
	for _, id := range []string{ghost, leftover} {
		if _, err := g.dock.InspectContainer(ctx, id); err != nil {
			t.Errorf("container %s was touched: %v", id, err)
		}
	}
	if g.dock.CallCount("RemoveContainer") != 0 {
		t.Error("the reaper removed a container it only had to report")
	}
	// Once the container is gone, a new one with the same ID is reported again
	// (the report set follows what exists).
	g.dock.Vanish(ghost)
	g.pass(time.Minute)
	if len(g.r.warned) != 1 {
		t.Errorf("the report set holds %d entries, want it pruned to the 1 container that still exists", len(g.r.warned))
	}
}

// ---- failures --------------------------------------------------------------------

func TestOneUsersFailureDoesNotStopTheRestAndIsLoggedOncePerChange(t *testing.T) {
	g := newRig(t)
	_, aID := g.up("alice")
	_, bID := g.up("bob")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	// Alice's stop fails for three passes; bob's works.
	var aliceFails atomic.Bool
	aliceFails.Store(true)
	g.r.ws = &failingStops{Workspaces: g.r.ws, fail: func(user string) bool { return user == "alice" && aliceFails.Load() }}

	for i := 0; i < 3; i++ {
		g.pass(time.Minute)
	}
	if g.running(bID) {
		t.Error("bob was not stopped because alice's stop failed")
	}
	if !g.running(aID) {
		t.Error("alice's failed stop must leave her running")
	}
	if n := g.logs.count("could not look after every workspace"); n != 1 {
		t.Errorf("the same failure on 3 passes was logged %d times, want once:\n%s", n, g.logs)
	}

	aliceFails.Store(false)
	g.pass(time.Minute)
	if g.running(aID) {
		t.Error("alice was not stopped once the failure cleared")
	}
	if g.logs.count("running normally again") != 1 {
		t.Errorf("want a recovery line:\n%s", g.logs)
	}
}

// failingStops wraps the manager: StopIfIdle fails for the users fail names.
type failingStops struct {
	Workspaces
	fail func(username string) bool
}

func (f *failingStops) StopIfIdle(ctx context.Context, u store.User, idle func(store.Workspace) bool) (bool, error) {
	if f.fail(u.Username) {
		return false, errors.New("docker stop failed")
	}
	return f.Workspaces.StopIfIdle(ctx, u, idle)
}

func TestThePassSurvivesAPanicForOneUser(t *testing.T) {
	g := newRig(t)
	_, aID := g.up("alice")
	_, bID := g.up("bob")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	g.r.ws = &panicking{Workspaces: g.r.ws, user: "alice"}
	g.pass(time.Minute) // must not panic, must not take bob down with it
	if g.running(bID) {
		t.Error("bob was not looked after because alice's handling panicked")
	}
	if !g.running(aID) {
		t.Error("alice should be untouched")
	}
	if g.logs.count("panic") == 0 {
		t.Errorf("the panic should be logged:\n%s", g.logs)
	}
}

type panicking struct {
	Workspaces
	user string
}

func (p *panicking) Reconcile(ctx context.Context, u store.User) (workspace.Status, error) {
	if u.Username == p.user {
		panic("unexpected reply from Docker")
	}
	return p.Workspaces.Reconcile(ctx, u)
}

func TestAFailedContainerListIsReportedButTheWorkspacesAreStillLookedAfter(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	g.dock.FailNext("ManagedContainers", errors.New("list failed"))
	g.pass(time.Minute)
	if g.running(id) {
		t.Error("the idle workspace was not stopped because the container list failed")
	}
	if g.logs.count("could not look after every workspace") != 1 {
		t.Errorf("want the failure reported:\n%s", g.logs)
	}
}

func TestADatabaseErrorSkipsThePass(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	g.r.store = brokenLister{}
	g.pass(time.Hour)
	if !g.running(id) {
		t.Error("a pass that could not read the database must not act")
	}
	if g.logs.count("could not look after every workspace") != 1 {
		t.Errorf("want the failure reported:\n%s", g.logs)
	}
}

type brokenLister struct{}

func (brokenLister) ListWorkspaces(context.Context) ([]store.UserWorkspace, error) {
	return nil, errors.New("database is locked")
}

// ---- load ------------------------------------------------------------------------

func TestAtMostConcurrencyWorkspacesAreHandledAtOnce(t *testing.T) {
	g := newRig(t, func(c *Config) { c.Concurrency = 3 })
	for i := 0; i < 10; i++ {
		g.up(fmt.Sprintf("user%02d", i))
	}
	g.pass(time.Minute) // baselines
	var cur, peak atomic.Int32
	g.dock.SetHook("CPUUsage", func(context.Context) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
	})
	g.pass(time.Minute)
	if p := peak.Load(); p > 3 || p < 2 {
		t.Errorf("peak concurrency %d, want 2 or 3 with Concurrency=3 (and 10 workspaces)", p)
	}
	if got := g.dock.CallCount("CPUUsage"); got < 20 {
		t.Errorf("CPUUsage called %d times for 2 passes of 10 workspaces", got)
	}
}

// ---- the loops -------------------------------------------------------------------

// stub is a Workspaces that records what the loops call.
type stub struct {
	reattach  atomic.Int32
	reconcile atomic.Int32

	mu  sync.Mutex
	err error // returned by ReattachTraefik
}

func (s *stub) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *stub) Reconcile(context.Context, store.User) (workspace.Status, error) {
	s.reconcile.Add(1)
	return workspace.Status{}, nil
}
func (s *stub) StopIfIdle(context.Context, store.User, func(store.Workspace) bool) (bool, error) {
	return false, nil
}
func (s *stub) ReclaimIf(context.Context, store.User, func(store.Workspace) bool) (bool, error) {
	return false, nil
}
func (s *stub) Heartbeat(context.Context, store.User) error { return nil }
func (s *stub) ReattachTraefik(context.Context) error {
	s.reattach.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

type emptyLister struct{ calls atomic.Int32 }

func (l *emptyLister) ListWorkspaces(context.Context) ([]store.UserWorkspace, error) {
	l.calls.Add(1)
	return nil, nil
}

type noContainers struct{}

func (noContainers) CPUUsage(context.Context, string) (docker.CPUUsage, error) {
	return docker.CPUUsage{}, nil
}
func (noContainers) ManagedContainers(context.Context) ([]docker.ContainerInfo, error) {
	return nil, nil
}

func TestRunAttachesTraefikAtOnceAndPeriodicallyAndSweepsOnItsOwnClock(t *testing.T) {
	s := &stub{}
	l := &emptyLister{}
	logs := &logBuf{}
	r, err := New(l, s, noContainers{}, Config{
		IdleTimeout: time.Hour, ReclaimAfter: time.Hour, Interval: 30 * time.Millisecond, AttachInterval: 10 * time.Millisecond,
	}, slog.New(slog.NewTextHandler(logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(cctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for (s.reattach.Load() < 5 || l.calls.Load() < 3) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.reattach.Load() < 5 {
		t.Errorf("ReattachTraefik called %d times, want it repeated", s.reattach.Load())
	}
	if l.calls.Load() < 3 {
		t.Errorf("swept %d times, want it repeated", l.calls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	n := s.reattach.Load()
	time.Sleep(50 * time.Millisecond)
	if s.reattach.Load() != n {
		t.Error("ReattachTraefik still being called after Run returned")
	}
}

func TestTheFirstSweepComesAfterOneInterval(t *testing.T) {
	l := &emptyLister{}
	r, err := New(l, &stub{}, noContainers{}, Config{
		IdleTimeout: time.Hour, ReclaimAfter: time.Hour, Interval: 400 * time.Millisecond, AttachInterval: time.Hour,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(cctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if n := l.calls.Load(); n != 0 {
		t.Errorf("swept %d times right after start, want the first sweep one interval later", n)
	}
	cancel()
	<-done
}

func TestAttachFailuresAreLoggedWhenTheyAppearAndWhenTheyClear(t *testing.T) {
	s := &stub{}
	s.setErr(errors.New("traefik is gone"))
	logs := &logBuf{}
	r, err := New(&emptyLister{}, s, noContainers{}, defaultConfig(), slog.New(slog.NewTextHandler(logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		r.attachOnce(ctx)
	}
	if n := logs.count("could not attach the proxy"); n != 1 {
		t.Errorf("a failure repeated 4 times was logged %d times, want once", n)
	}
	s.setErr(errors.New("a different failure"))
	r.attachOnce(ctx)
	if n := logs.count("could not attach the proxy"); n != 2 {
		t.Errorf("a changed failure was logged %d times in total, want 2", n)
	}
	s.setErr(nil)
	r.attachOnce(ctx)
	r.attachOnce(ctx)
	if n := logs.count("attached to all user networks again"); n != 1 {
		t.Errorf("recovery logged %d times, want once", n)
	}
}

func TestAPanicInTheAttachLoopIsContained(t *testing.T) {
	r, err := New(&emptyLister{}, panicAttach{}, noContainers{}, defaultConfig(), slog.New(slog.NewTextHandler(&logBuf{}, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.attachOnce(ctx) // must not panic
}

type panicAttach struct{ Workspaces }

func (panicAttach) ReattachTraefik(context.Context) error { panic("boom") }

// ---- the rules added by the review ----------------------------------------------

// A start that never turns healthy is judged by its heartbeat alone: its CPU is
// not a user's job (a crash-looping neko burns CPU too), so it must not keep the
// container alive for ever.
func TestAStartingWorkspaceIsJudgedByItsHeartbeatNotItsCPU(t *testing.T) {
	g := newRig(t)
	u := g.user("alice")
	if _, err := g.m.Start(ctx, u); err != nil {
		t.Fatal(err)
	}
	id := g.dock.ContainerIDByName(workspace.ContainerName("alice"))
	g.dock.SetCPUCores(id, 1.5)
	for i := 0; i < 30; i++ {
		g.pass(time.Minute)
	}
	if g.running(id) || g.ws(u).State != store.StateStopped {
		t.Fatalf("a stuck start with CPU load was kept alive: %+v", g.ws(u))
	}
	if n := g.dock.CallCount("CPUUsage"); n != 0 {
		t.Errorf("CPUUsage called %d times for a starting workspace", n)
	}
}

func TestMissingCPUReadingsAreReportedOnceAndStopNobody(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	for i := 0; i < 4; i++ {
		g.dock.FailNext("CPUUsage", errors.New("stats are broken on this host"))
	}
	for i := 0; i < 4; i++ {
		g.pass(time.Minute)
		if !g.running(id) {
			t.Fatalf("stopped without a CPU reading (pass %d)", i+1)
		}
	}
	if n := g.logs.count("no usable CPU reading"); n != 1 {
		t.Errorf("the missing reading was reported %d times, want once:\n%s", n, g.logs)
	}
	if !strings.Contains(g.logs.String(), "BERTH_IDLE_CPU_PERCENT=0") {
		t.Errorf("the warning should say how to switch the CPU check off:\n%s", g.logs)
	}
	g.pass(time.Minute) // readings work again (the one after the gap spans several minutes)
	g.pass(time.Minute)
	if g.running(id) {
		t.Error("not stopped once readings were usable again")
	}
}

func TestReadingsThatCannotBeComparedAreNotAVerdict(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	g.dock.SetCPUCores(id, 0.02) // quiet, but the counter is not zero
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	// The container restarts under the same ID: its cumulative counter starts again.
	if err := g.dock.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := g.dock.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	g.dock.SetHealth(id, docker.HealthHealthy)
	g.pass(time.Minute) // minute 30: idle by the clock, but the counter went backwards
	if !g.running(id) {
		t.Fatal("stopped on readings that cannot be compared")
	}
	g.pass(time.Minute) // comparable again, and quiet
	if g.running(id) {
		t.Fatal("not stopped once the readings could be compared")
	}
}

// unavailableDocker makes every Reconcile fail the way a missing daemon does.
type unavailableDocker struct {
	Workspaces
	calls atomic.Int32
	err   func(user string) error
}

func (u *unavailableDocker) Reconcile(ctx context.Context, user store.User) (workspace.Status, error) {
	u.calls.Add(1)
	return workspace.Status{}, u.err(user.Username)
}

func TestAnUnreachableDockerEndsThePassAndIsReportedOnceWhoeverFailsFirst(t *testing.T) {
	g := newRig(t, func(c *Config) { c.Concurrency = 2 })
	for i := 0; i < 12; i++ {
		g.up(fmt.Sprintf("user%02d", i))
	}
	var seq atomic.Int32
	down := &unavailableDocker{Workspaces: g.r.ws, err: func(user string) error {
		// Different text for every user and every call: only the class is the same.
		return fmt.Errorf("%s, call %d: %w", user, seq.Add(1), docker.ErrUnavailable)
	}}
	g.r.ws = down
	for i := 0; i < 4; i++ {
		g.pass(time.Minute)
	}
	if n := down.calls.Load(); n > 4*4 {
		t.Errorf("Reconcile called %d times in 4 passes of 12 workspaces: the pass should end once Docker is unreachable", n)
	}
	if n := g.logs.count("could not look after every workspace"); n != 1 {
		t.Errorf("a steady outage was logged %d times, want once:\n%s", n, g.logs)
	}
	g.r.ws = down.Workspaces // Docker is back
	g.pass(time.Minute)
	if g.logs.count("running normally again") != 1 {
		t.Errorf("want a recovery line:\n%s", g.logs)
	}
}

func TestATimedOutDockerCallIsAlsoAnOutageNotAFailurePerWorkspace(t *testing.T) {
	g := newRig(t, func(c *Config) { c.Concurrency = 1 })
	for i := 0; i < 8; i++ {
		g.up(fmt.Sprintf("user%02d", i))
	}
	down := &unavailableDocker{Workspaces: g.r.ws, err: func(user string) error {
		return fmt.Errorf("inspect %s: %w", user, context.DeadlineExceeded)
	}}
	g.r.ws = down
	g.pass(time.Minute)
	g.pass(time.Minute)
	if n := down.calls.Load(); n > 4 {
		t.Errorf("Reconcile called %d times in 2 passes: a timeout should end the pass", n)
	}
	if n := g.logs.count("could not look after every workspace"); n != 1 {
		t.Errorf("logged %d times, want once", n)
	}
}

// slowReconcile never answers until its context ends.
type slowReconcile struct{ Workspaces }

func (slowReconcile) Reconcile(ctx context.Context, user store.User) (workspace.Status, error) {
	<-ctx.Done()
	return workspace.Status{}, ctx.Err()
}

func TestEachWorkspacesInspectionHasItsOwnShortDeadline(t *testing.T) {
	g := newRig(t, func(c *Config) { c.OpTimeout = time.Hour })
	g.up("alice")
	g.r.reconcileTimeout = 50 * time.Millisecond
	g.r.ws = slowReconcile{g.r.ws}
	done := make(chan struct{})
	go func() { g.r.Sweep(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung Docker call held the pass for the whole operation timeout")
	}
	if g.logs.count("could not look after every workspace") != 1 {
		t.Errorf("want the timeout reported:\n%s", g.logs)
	}
}

func TestClockStepped(t *testing.T) {
	const tol = 30 * time.Second
	for _, tc := range []struct {
		name       string
		wall, mono time.Duration
		want       bool
	}{
		{"normal tick", time.Minute, time.Minute, false},
		{"a little drift", time.Minute + 5*time.Second, time.Minute, false},
		{"exactly the tolerance", time.Minute + tol, time.Minute, false},
		{"suspended for an hour: wall ran, monotonic did not", time.Hour, time.Minute, true},
		{"clock set forward", time.Minute + time.Hour, time.Minute, true},
		{"clock set back", time.Minute - time.Hour, time.Minute, true},
		{"just over the tolerance", time.Minute + tol + time.Second, time.Minute, true},
	} {
		if got := clockStepped(tc.wall, tc.mono, tol); got != tc.want {
			t.Errorf("%s: clockStepped(%v, %v) = %v, want %v", tc.name, tc.wall, tc.mono, got, tc.want)
		}
	}
}

// After the clock steps, heartbeats that were due have been missed: everyone gets
// a fresh timeout, like after a restart.
func TestAFreshFloorGivesEveryoneAFullTimeoutAgain(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	g.r.raiseFloor(g.clk.Now()) // what sweepLoop does on a clock step
	for i := 1; i < 30; i++ {
		g.pass(time.Minute)
		if !g.running(id) {
			t.Fatalf("stopped %d minutes after the floor was raised", i)
		}
	}
	g.pass(time.Minute)
	if g.running(id) {
		t.Fatal("not stopped a full timeout after the floor")
	}
	// The floor never moves back.
	before := g.r.floor
	g.r.raiseFloor(before.Add(-time.Hour))
	if !g.r.floor.Equal(before) {
		t.Errorf("floor moved back to %v", g.r.floor)
	}
}

func TestShuttingDownLeavesNoTraceAndNoWarning(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	g.clk.Advance(5 * time.Minute) // idle by now

	// Before the pass: nothing is done, nothing is reported.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	g.r.Sweep(cctx)
	if !g.running(id) || g.dock.CallCount("StopContainer") != 0 {
		t.Error("a cancelled pass acted")
	}

	// During the pass: the Docker call that was cut short fails with the
	// cancellation, which is not a failure of Docker and must not be reported.
	cctx, cancel = context.WithCancel(ctx)
	g.r.ws = cancelling{Workspaces: g.r.ws, cancel: cancel}
	g.r.Sweep(cctx)
	if g.logs.count("could not look after every workspace") != 0 {
		t.Errorf("a clean shutdown must not be reported as a failure:\n%s", g.logs)
	}
	if !g.running(id) {
		t.Error("the workspace was stopped by a pass that was shut down")
	}
}

// cancelling cancels the pass from inside the first Docker call, as a shutdown would.
type cancelling struct {
	Workspaces
	cancel context.CancelFunc
}

func (c cancelling) Reconcile(ctx context.Context, user store.User) (workspace.Status, error) {
	c.cancel()
	return workspace.Status{}, ctx.Err()
}

func TestWhatIsRememberedAboutAContainerIsForgottenWhenItStops(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	g.dock.SetCPUCores(id, 1)
	g.pass(time.Minute)
	g.pass(time.Minute)
	g.dock.FailNext("CPUUsage", errors.New("hiccup"))
	g.pass(time.Minute) // leaves a count of looks without a verdict
	if len(g.r.cpu) != 1 || len(g.r.busyAt) != 1 || len(g.r.busyNow) != 1 || len(g.r.unknown) != 1 {
		t.Fatalf("setup: cpu=%d busyAt=%d busyNow=%d unknown=%d", len(g.r.cpu), len(g.r.busyAt), len(g.r.busyNow), len(g.r.unknown))
	}
	if err := g.dock.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	g.pass(time.Minute) // Reconcile records the stop
	g.pass(time.Minute) // the workspace is no longer up in the rows this pass starts from
	if len(g.r.cpu) != 0 || len(g.r.busyAt) != 0 || len(g.r.busyNow) != 0 || len(g.r.unknown) != 0 {
		t.Errorf("stale baselines: cpu=%d busyAt=%d busyNow=%d unknown=%d", len(g.r.cpu), len(g.r.busyAt), len(g.r.busyNow), len(g.r.unknown))
	}
}

// listThenRecord lists the containers, and only then does the user's power-on
// record one of them: a container that is unrecorded at listing time but recorded
// by the time the pass reads the database is not unaccounted for.
type listThenRecord struct {
	Docker
	after func()
}

func (l listThenRecord) ManagedContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	list, err := l.Docker.ManagedContainers(ctx)
	if l.after != nil {
		l.after()
	}
	return list, err
}

func TestAContainerRecordedWhileThePassRunsIsNotReportedAsUnaccountedFor(t *testing.T) {
	g := newRig(t)
	u := g.user("alice")
	id, err := g.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name: workspace.ContainerName("alice"), Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.r.dock = listThenRecord{Docker: g.dock, after: func() {
		// What a Start does right after creating the container.
		if _, err := g.st.TransitionWorkspace(ctx, u.ID, []store.State{store.StateOffline}, store.StateStarting, g.clk.Now(),
			store.SetContainerID(id)); err != nil {
			t.Error(err)
		}
	}}
	g.pass(time.Minute)
	if g.logs.count("is not recorded in the database") != 0 {
		t.Errorf("a container recorded during the pass was reported as unrecorded:\n%s", g.logs)
	}
}

// Removing a container is the reaper's own doing: it must not report it as
// a container the database does not know.
func TestAReclaimDuringThePassIsNotReportedAsAnUnrecordedContainer(t *testing.T) {
	g := newRig(t, func(c *Config) { c.IdleTimeout = 1000 * time.Hour })
	u, id := g.up("alice")
	if _, err := g.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	g.pass(7 * 24 * time.Hour) // the reclaim happens in this pass
	if _, err := g.dock.InspectContainer(ctx, id); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("setup: the container should be gone: %v", err)
	}
	if n := g.logs.count("is not recorded in the database"); n != 0 {
		t.Errorf("the reaper reported its own removal as an unrecorded container:\n%s", g.logs)
	}
}

// A pass that could not get to a workspace (Docker unreachable, an inspection that
// failed) must not make it forget what it knows: a busy container's CPU baseline
// and the moment it was last busy decide the next pass.
func TestAPassThatDidNotReachAWorkspaceKeepsItsMemory(t *testing.T) {
	g := newRig(t, func(c *Config) { c.Concurrency = 1 })
	_, aID := g.up("alice")
	g.up("bob")
	g.dock.SetCPUCores(aID, 1)
	g.pass(time.Minute)
	g.pass(time.Minute)
	if _, ok := g.r.busyAt[aID]; !ok || len(g.r.cpu) != 2 {
		t.Fatalf("setup: busyAt=%v cpu=%d", g.r.busyAt, len(g.r.cpu))
	}
	// Docker goes away at the first workspace: the pass ends before it reaches the rest.
	g.r.ws = &unavailableDocker{Workspaces: g.r.ws, err: func(string) error { return docker.ErrUnavailable }}
	g.pass(time.Minute)
	if _, ok := g.r.busyAt[aID]; !ok || len(g.r.cpu) != 2 {
		t.Errorf("a pass that did not run forgot what it knew: busyAt=%v cpu=%d", g.r.busyAt, len(g.r.cpu))
	}
}

// ---- gaps found by the test review ------------------------------------------------------

// The loop's own beat: a wall clock that moved differently from the monotonic one
// (a resumed laptop, a paused VM) gives everyone a fresh timeout, and nothing in
// the pass can end the backend.
func TestATickWithASteppedClockGivesEveryoneAFreshTimeout(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	g.clk.Advance(time.Minute)              // minute 30: idle by the clock...
	g.r.tick(ctx, 3*time.Hour, time.Minute) // ...but the wall clock ran 3 hours while the monotonic one ran a minute
	if !g.running(id) {
		t.Fatal("stopped on the first pass after the machine slept")
	}
	if g.logs.count("the clock stepped") != 1 {
		t.Errorf("want the step logged:\n%s", g.logs)
	}
	for i := 1; i < 30; i++ {
		g.clk.Advance(time.Minute)
		g.r.tick(ctx, time.Minute, time.Minute)
		if !g.running(id) {
			t.Fatalf("stopped %d minutes after the step", i)
		}
	}
	g.clk.Advance(time.Minute)
	g.r.tick(ctx, time.Minute, time.Minute)
	if g.running(id) {
		t.Fatal("not stopped a full timeout after the step")
	}
}

func TestATickWithAnAgreeingClockLeavesTheFloorAlone(t *testing.T) {
	g := newRig(t)
	_, id := g.up("alice")
	for i := 0; i < 29; i++ {
		g.pass(time.Minute)
	}
	g.clk.Advance(time.Minute)
	g.r.tick(ctx, time.Minute+5*time.Second, time.Minute) // ordinary drift
	if g.running(id) {
		t.Fatal("a workspace idle for the full timeout was kept by a tick that saw no clock step")
	}
	if g.logs.count("the clock stepped") != 0 {
		t.Errorf("an ordinary tick was reported as a clock step:\n%s", g.logs)
	}
}

type panickingLister struct{ calls atomic.Int32 }

func (p *panickingLister) ListWorkspaces(context.Context) ([]store.UserWorkspace, error) {
	if p.calls.Add(1) == 1 {
		panic("the store misbehaved")
	}
	return nil, nil
}

// A panic anywhere in the pass, not only inside a per-workspace worker, must not
// end the backend: the loop logs it and carries on.
func TestAPanicInThePassCostsOnePassNotTheLoop(t *testing.T) {
	l := &panickingLister{}
	logs := &logBuf{}
	r, err := New(l, &stub{}, noContainers{}, Config{
		IdleTimeout: time.Hour, ReclaimAfter: time.Hour, Interval: 10 * time.Millisecond, AttachInterval: time.Hour,
	}, slog.New(slog.NewTextHandler(logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(cctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for l.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if l.calls.Load() < 3 {
		t.Fatalf("the loop stopped after the panic: %d passes", l.calls.Load())
	}
	if logs.count("reaper: panic") != 1 {
		t.Errorf("want the panic logged once:\n%s", logs)
	}
}

// Traefik is re-attached at once when the reaper starts, not one interval later:
// a deploy that recreates Traefik and the backend together must not leave every
// workspace answering 504 for an extra interval.
func TestTheFirstAttachIsAtStartNotOneIntervalLater(t *testing.T) {
	s := &stub{}
	r, err := New(&emptyLister{}, s, noContainers{}, Config{
		IdleTimeout: time.Hour, ReclaimAfter: time.Hour, Interval: time.Hour, AttachInterval: time.Hour,
	}, slog.New(slog.NewTextHandler(&logBuf{}, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(cctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.reattach.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	if s.reattach.Load() != 1 {
		t.Errorf("ReattachTraefik called %d times, want exactly one at start", s.reattach.Load())
	}
}

func TestAShutdownDuringAnAttachIsNotReportedAsAProxyFailure(t *testing.T) {
	s := &stub{}
	s.setErr(context.Canceled)
	logs := &logBuf{}
	r, err := New(&emptyLister{}, s, noContainers{}, defaultConfig(), slog.New(slog.NewTextHandler(logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	r.attachOnce(cctx)
	if logs.count("could not attach the proxy") != 0 {
		t.Errorf("a clean shutdown was reported as a proxy failure:\n%s", logs)
	}
}

func TestDefaultsOfTheReaper(t *testing.T) {
	r, err := New(&emptyLister{}, &stub{}, noContainers{}, defaultConfig(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.Concurrency != 4 || r.cfg.OpTimeout != 5*time.Minute {
		t.Errorf("defaults: concurrency %d, op timeout %v, want 4 and 5m", r.cfg.Concurrency, r.cfg.OpTimeout)
	}
	if r.reconcileTimeout != 30*time.Second {
		t.Errorf("reconcile timeout %v, want 30s", r.reconcileTimeout)
	}
}

// Isolated hiccups are not worth a warning; three looks in a row without a usable
// CPU reading are, once.
func TestOnlyARunOfMissingCPUReadingsIsReported(t *testing.T) {
	g := newRig(t)
	g.up("alice")
	g.pass(time.Minute) // the baseline: the first look has nothing to compare with
	g.pass(time.Minute) // the first usable reading
	fail := func(n int) {
		for i := 0; i < n; i++ {
			g.dock.FailNext("CPUUsage", errors.New("hiccup"))
			g.pass(time.Minute)
		}
	}
	fail(2)
	g.pass(time.Minute) // a good reading resets the run
	fail(2)
	if n := g.logs.count("no usable CPU reading"); n != 0 {
		t.Errorf("two hiccups, a good reading and two more were reported %d times, want never:\n%s", n, g.logs)
	}
	fail(1) // the third in a row since the good reading
	if n := g.logs.count("no usable CPU reading"); n != 1 {
		t.Errorf("three in a row were reported %d times, want once:\n%s", n, g.logs)
	}
	fail(2)
	if n := g.logs.count("no usable CPU reading"); n != 1 {
		t.Errorf("a longer run was reported %d times, want still once", n)
	}
}

// The reaper repeats a reclaim decision against the row as it is under the claim:
// a user who powered on and off again while the pass was looking has just
// stopped, and the container she stopped seconds ago must not be removed.
func TestAReclaimDecisionOvertakenByTheUserIsRefused(t *testing.T) {
	g := newRig(t, func(c *Config) { c.IdleTimeout = 1000 * time.Hour })
	u, id := g.up("alice")
	if _, err := g.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(7*24*time.Hour + time.Minute) // reclaimable by the snapshot
	g.dock.SetHook("InspectContainer", func(context.Context) {
		g.dock.SetHook("InspectContainer", nil)
		// While the pass is inside Reconcile, the user comes back and leaves again.
		if _, err := g.m.Start(ctx, u); err != nil {
			t.Error(err)
		}
		g.dock.SetHealth(id, docker.HealthHealthy)
		if _, err := g.m.Status(ctx, u); err != nil {
			t.Error(err)
		}
		if _, err := g.m.Stop(ctx, u); err != nil {
			t.Error(err)
		}
	})
	g.r.Sweep(ctx)
	if _, err := g.dock.InspectContainer(ctx, id); err != nil {
		t.Fatalf("the container the user stopped moments ago was removed: %v", err)
	}
	if ws := g.ws(u); ws.State != store.StateStopped || ws.ContainerID != id {
		t.Errorf("workspace = %+v", ws)
	}
}

// A container recorded after the pass has read the workspaces (a power-on that lands
// while the pass is busy with someone else) is not "unrecorded": the rows are read
// again for the report.
func TestAContainerRecordedDuringTheHandlingPhaseIsNotReported(t *testing.T) {
	g := newRig(t)
	g.up("bob") // something for the handling phase to do
	alice := g.user("alice")
	id, err := g.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name: workspace.ContainerName("alice"), Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name: "berth-ws-carol", Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: "carol"},
	}); err != nil {
		t.Fatal(err)
	}
	g.pass(time.Minute) // baselines, so the next pass reads CPU
	g.dock.SetHook("CPUUsage", func(context.Context) {
		g.dock.SetHook("CPUUsage", nil)
		// Alice's power-on records her container while the pass is handling bob.
		if _, err := g.st.TransitionWorkspace(ctx, alice.ID, []store.State{store.StateOffline}, store.StateStarting, g.clk.Now(),
			store.SetContainerID(id)); err != nil {
			t.Error(err)
		}
	})
	g.logs.sb.Reset()
	g.pass(time.Minute)
	if g.logs.count("not recorded in the database") != 0 {
		t.Errorf("alice's container was recorded during the pass and must not be reported:\n%s", g.logs)
	}
}
