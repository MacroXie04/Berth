package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
)

const traefikName = "berth-traefik"

func testConfig() Config {
	return Config{
		Image:                "berth/workspace:latest",
		TraefikContainer:     traefikName,
		Entrypoint:           "web",
		UDPPortMin:           30000,
		UDPPortMax:           30031,
		UDPPortsPerWorkspace: 8,
		Resolution:           "1280x720@30",
		ShmSizeMB:            2048,
		PidsLimit:            2048,
		StopTimeout:          15 * time.Second,
		StartTimeout:         30 * time.Second,
	}
}

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

type rig struct {
	t       *testing.T
	m       *Manager
	st      *store.SQLite
	dock    *docker.Fake
	clock   *clock
	traefik string // the Traefik container's ID in the fake
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
	cfg := testConfig()
	for _, f := range mutate {
		f(&cfg)
	}
	m, err := NewManager(st, dock, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, m: m, st: st, dock: dock, clock: clk, traefik: dock.AddContainer(traefikName)}
}

func (r *rig) user(name string) store.User {
	r.t.Helper()
	u, _, err := r.st.CreateUser(ctx, store.NewUser{
		Username: name, PasswordHash: "x", VolumeName: VolumeName(name), MemLimitMB: 2048, CPULimit: 2,
	}, t0)
	if err != nil {
		r.t.Fatal(err)
	}
	return u
}

func (r *rig) ws(u store.User) store.Workspace {
	r.t.Helper()
	w, err := r.st.WorkspaceByUserID(ctx, u.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

func (r *rig) mustStart(u store.User) Status {
	r.t.Helper()
	s, err := r.m.Start(ctx, u)
	if err != nil {
		r.t.Fatalf("Start: %v", err)
	}
	return s
}

func (r *rig) containerID(name string) string { return r.dock.ContainerIDByName(ContainerName(name)) }

func TestNewManagerRejectsIncompleteConfig(t *testing.T) {
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "b.db"))
	defer st.Close()
	bad := map[string]func(*Config){
		"no image":           func(c *Config) { c.Image = "" },
		"no entrypoint":      func(c *Config) { c.Entrypoint = "" },
		"no resolution":      func(c *Config) { c.Resolution = "" },
		"zero start timeout": func(c *Config) { c.StartTimeout = 0 },
		"zero stop timeout":  func(c *Config) { c.StopTimeout = 0 },
		"zero shm":           func(c *Config) { c.ShmSizeMB = 0 },
		"zero pids":          func(c *Config) { c.PidsLimit = 0 },
		"zero block size":    func(c *Config) { c.UDPPortsPerWorkspace = 0 },
		"range too small":    func(c *Config) { c.UDPPortMin, c.UDPPortMax = 30000, 30003 },
		"port above 65535":   func(c *Config) { c.UDPPortMax = 70000 },
	}
	for name, mutate := range bad {
		cfg := testConfig()
		mutate(&cfg)
		if _, err := NewManager(st, docker.NewFake(nil), cfg, nil, nil); err == nil {
			t.Errorf("%s: NewManager accepted an invalid config", name)
		}
	}
}

func TestStartFromOffline(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")

	s := r.mustStart(u)
	if s.Workspace.State != store.StateStarting || s.Health != docker.HealthStarting || s.Stats != nil {
		t.Fatalf("status right after start: state=%s health=%q stats=%v", s.Workspace.State, s.Health, s.Stats)
	}

	// The per-user network exists, Traefik is attached to it, and the workspace
	// is on it and on nothing else.
	if members := r.dock.NetworkMembers("berth-net-alice"); strings.Join(members, ",") != "berth-traefik,berth-ws-alice" {
		t.Errorf("network members = %v", members)
	}
	if !r.dock.HasVolume("berth-data-alice") {
		t.Error("data volume was not created")
	}
	id := r.containerID("alice")
	info, _ := r.dock.InspectContainer(ctx, id)
	if !info.Running || len(info.Networks) != 1 || info.Networks[0] != "berth-net-alice" {
		t.Errorf("container info: %+v", info)
	}

	// The database records everything a later request needs.
	ws := r.ws(u)
	if ws.ContainerID != id || ws.UDPPortStart != 30000 || ws.UDPPortEnd != 30007 || len(ws.NekoPassword) < 20 ||
		ws.LastActiveAt == nil || !ws.LastActiveAt.Equal(t0) {
		t.Errorf("workspace row: %+v", ws)
	}

	spec, _ := r.dock.Spec(id)
	checks := map[string]bool{
		"image":           spec.Image == "berth/workspace:latest",
		"name and host":   spec.Name == "berth-ws-alice" && spec.Hostname == "berth-ws-alice",
		"network":         spec.Network == "berth-net-alice",
		"volume":          len(spec.Volumes) == 1 && spec.Volumes[0] == docker.VolumeMount{Volume: "berth-data-alice", Target: "/home/user"},
		"memory":          spec.MemoryBytes == 2048<<20,
		"cpus":            spec.NanoCPUs == 2_000_000_000,
		"shm":             spec.ShmSizeBytes == 2<<30,
		"pids":            spec.PidsLimit == 2048,
		"cap drop":        len(spec.CapDrop) == 1 && spec.CapDrop[0] == "NET_RAW",
		"no-new-privs":    len(spec.SecurityOpt) == 1 && spec.SecurityOpt[0] == "no-new-privileges",
		"init":            spec.Init,
		"stop timeout":    spec.StopTimeout == 15*time.Second,
		"udp range":       spec.UDPPortStart == 30000 && spec.UDPPortEnd == 30007,
		"owner label":     spec.Labels[docker.LabelUser] == "alice" && spec.Labels[docker.LabelManaged] == "true",
		"neko user pw":    envValue(spec.Env, envNekoUserPassword) == ws.NekoPassword,
		"neko admin pw":   len(envValue(spec.Env, envNekoAdminPassword)) >= 20 && envValue(spec.Env, envNekoAdminPassword) != ws.NekoPassword,
		"neko epr":        envValue(spec.Env, envNekoEPR) == "30000-30007",
		"neko path":       envValue(spec.Env, envNekoPathPrefix) == "/ws/alice/browser",
		"neko screen":     envValue(spec.Env, envNekoScreen) == "1280x720@30",
		"no nat1to1 set":  envValue(spec.Env, envNekoNAT1TO1) == "",
		"traefik network": spec.Labels["traefik.docker.network"] == "berth-net-alice",
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("spec check failed: %s\nspec: %+v", name, spec)
		}
	}
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func TestReadinessFollowsTheHealthcheck(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	id := r.containerID("alice")

	// Still starting: stays 'starting', and repeated polling changes nothing.
	for i := 0; i < 3; i++ {
		if s, _ := r.m.Status(ctx, u); s.Workspace.State != store.StateStarting {
			t.Fatalf("state = %s while the healthcheck is still starting", s.Workspace.State)
		}
	}

	r.dock.SetHealth(id, docker.HealthHealthy)
	r.dock.SetStats(id, docker.Stats{CPUPercent: 12.5, MemUsageBytes: 100 << 20, MemLimitBytes: 2048 << 20, Pids: 42})
	s, err := r.m.Status(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workspace.State != store.StateRunning || s.Health != docker.HealthHealthy {
		t.Fatalf("state=%s health=%q, want running/healthy", s.Workspace.State, s.Health)
	}
	if s.Stats == nil || s.Stats.CPUPercent != 12.5 || s.Stats.MemUsageBytes != 100<<20 {
		t.Fatalf("stats = %+v", s.Stats)
	}

	// A healthcheck that starts failing later does not demote a running workspace.
	r.dock.SetHealth(id, docker.HealthUnhealthy)
	s, _ = r.m.Status(ctx, u)
	if s.Workspace.State != store.StateRunning || s.Health != docker.HealthUnhealthy {
		t.Fatalf("unhealthy: state=%s health=%q", s.Workspace.State, s.Health)
	}
}

func TestImageWithoutHealthcheckIsReadyOnceRunning(t *testing.T) {
	r := newRig(t)
	r.dock.NoHealthcheck = true
	u := r.user("alice")
	r.mustStart(u)
	if s, _ := r.m.Status(ctx, u); s.Workspace.State != store.StateRunning {
		t.Fatalf("state = %s, want running", s.Workspace.State)
	}
}

func TestStartIsIdempotent(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	r.mustStart(u)
	r.dock.SetHealth(r.containerID("alice"), docker.HealthHealthy)
	r.m.Status(ctx, u)
	r.mustStart(u) // while running
	if n := r.dock.CallCount("CreateContainer"); n != 1 {
		t.Fatalf("CreateContainer called %d times, want 1", n)
	}
	if s, _ := r.m.Status(ctx, u); s.Workspace.State != store.StateRunning {
		t.Fatalf("state = %s", s.Workspace.State)
	}
}

// A double-clicked "Power on" is two simultaneous requests: only one may build
// a container.
func TestConcurrentStartsCreateOneContainer(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.m.Start(ctx, u)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Start: %v", err)
		}
	}
	if c := r.dock.CallCount("CreateContainer"); c != 1 {
		t.Fatalf("CreateContainer called %d times, want 1", c)
	}
	if got := r.dock.Containers(); strings.Join(got, ",") != "berth-traefik,berth-ws-alice" {
		t.Fatalf("containers = %v", got)
	}
}

func TestStopAndRestart(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	id := r.containerID("alice")
	r.dock.SetHealth(id, docker.HealthHealthy)
	r.m.Status(ctx, u)
	before := r.ws(u)

	r.clock.Advance(time.Hour)
	s, err := r.m.Stop(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workspace.State != store.StateStopped || s.Workspace.LastStoppedAt == nil ||
		!s.Workspace.LastStoppedAt.Equal(t0.Add(time.Hour)) || s.Workspace.ContainerID != id {
		t.Fatalf("after stop: %+v", s.Workspace)
	}
	if info, _ := r.dock.InspectContainer(ctx, id); info.Running {
		t.Fatal("container still running after Stop")
	}
	if s, err := r.m.Stop(ctx, u); err != nil || s.Workspace.State != store.StateStopped { // idempotent
		t.Fatalf("second Stop: %v %v", s.Workspace.State, err)
	}

	// Returning user: the same container is started, nothing is recreated, and
	// the secrets and port block are unchanged.
	r.clock.Advance(time.Hour)
	r.mustStart(u)
	if n := r.dock.CallCount("CreateContainer"); n != 1 {
		t.Fatalf("restart created a new container (%d creates)", n)
	}
	after := r.ws(u)
	if after.State != store.StateStarting || after.ContainerID != id || after.NekoPassword != before.NekoPassword ||
		after.UDPPortStart != before.UDPPortStart {
		t.Fatalf("after restart: %+v (before %+v)", after, before)
	}
	if info, _ := r.dock.InspectContainer(ctx, id); !info.Running {
		t.Fatal("container not running after restart")
	}
}

func TestStopWhenNotUpIsHarmless(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	s, err := r.m.Stop(ctx, u)
	if err != nil || s.Workspace.State != store.StateOffline {
		t.Fatalf("Stop on an offline workspace: %v %v", s.Workspace.State, err)
	}
	if n := r.dock.CallCount("StopContainer"); n != 0 {
		t.Fatalf("StopContainer called %d times for an offline workspace", n)
	}
}

func TestStopDuringContainerCreationIsBusy(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	// A start has claimed the workspace but not yet recorded its container.
	if _, err := r.st.TransitionWorkspace(ctx, u.ID, Sources(EventStart), store.StateStarting, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Stop(ctx, u); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestStopOfAVanishedContainerGoesOffline(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	r.dock.Vanish(r.containerID("alice"))
	s, err := r.m.Stop(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workspace.State != store.StateOffline || s.Workspace.ContainerID != "" || s.Workspace.UDPPortStart != 0 {
		t.Fatalf("workspace: %+v", s.Workspace)
	}
}

func TestStartRecreatesAContainerThatVanishedWhileStopped(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	r.dock.SetHealth(r.containerID("alice"), docker.HealthHealthy)
	r.m.Status(ctx, u)
	r.m.Stop(ctx, u)
	before := r.ws(u)

	r.dock.Vanish(before.ContainerID) // someone ran `docker rm -f` by hand
	r.mustStart(u)

	after := r.ws(u)
	if after.ContainerID == "" || after.ContainerID == before.ContainerID {
		t.Fatalf("container id %q (was %q)", after.ContainerID, before.ContainerID)
	}
	if after.NekoPassword != before.NekoPassword || after.UDPPortStart != before.UDPPortStart {
		t.Fatalf("secrets/ports changed across recreation: %+v vs %+v", after, before)
	}
	if info, _ := r.dock.InspectContainer(ctx, after.ContainerID); !info.Running {
		t.Fatal("recreated container is not running")
	}
}

func TestStartFailuresRollBack(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name    string
		fail    string
		wantErr string
	}{
		{"network", "EnsureNetwork", "boom"},
		{"volume", "EnsureVolume", "boom"},
		{"create", "CreateContainer", "boom"},
		{"start", "StartContainer", "boom"},
		{"attach proxy", "ConnectToNetwork", "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			u := r.user("alice")
			r.dock.FailNext(tc.fail, boom)

			if _, err := r.m.Start(ctx, u); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			ws := r.ws(u)
			if ws.State != store.StateOffline || ws.ContainerID != "" || ws.UDPPortStart != 0 {
				t.Fatalf("workspace not rolled back: %+v", ws)
			}
			if got := r.dock.Containers(); strings.Join(got, ",") != "berth-traefik" {
				t.Fatalf("containers left behind: %v", got)
			}
			// ... and the user can simply try again.
			if _, err := r.m.Start(ctx, u); err != nil {
				t.Fatalf("retry after rollback: %v", err)
			}
			if r.ws(u).State != store.StateStarting {
				t.Fatalf("retry state = %s", r.ws(u).State)
			}
		})
	}
}

func TestFailedStartOfAKeptContainerLeavesItStopped(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	id := r.containerID("alice")
	r.dock.SetHealth(id, docker.HealthHealthy)
	r.m.Status(ctx, u)
	r.m.Stop(ctx, u)

	r.dock.FailNext("StartContainer", errors.New("no space left on device"))
	if _, err := r.m.Start(ctx, u); err == nil {
		t.Fatal("expected an error")
	}
	ws := r.ws(u)
	if ws.State != store.StateStopped || ws.ContainerID != id {
		t.Fatalf("a failed restart must not lose the kept container: %+v", ws)
	}
	if got := r.containerID("alice"); got != id {
		t.Fatalf("the kept container was removed (now %q)", got)
	}
}

func TestStartWhenProxyContainerIsMissing(t *testing.T) {
	r := newRig(t)
	r.dock.Vanish(r.traefik)
	u := r.user("alice")
	_, err := r.m.Start(ctx, u)
	if err == nil || !errors.Is(err, docker.ErrNotFound) || !strings.Contains(err.Error(), traefikName) {
		t.Fatalf("err = %v, want a not-found error naming the proxy container", err)
	}
	if r.ws(u).State != store.StateOffline {
		t.Fatalf("state = %s", r.ws(u).State)
	}
}

func TestStartWithoutProxyWhenNotConfigured(t *testing.T) {
	r := newRig(t, func(c *Config) { c.TraefikContainer = "" })
	r.dock.Vanish(r.traefik)
	u := r.user("alice")
	r.mustStart(u)
	if n := r.dock.CallCount("ConnectToNetwork"); n != 0 {
		t.Fatalf("ConnectToNetwork called %d times with no proxy configured", n)
	}
}

func TestStartAdoptsItsOwnOrphanedContainer(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	// A previous attempt created the container then died before recording it.
	orphan, err := r.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name: "berth-ws-alice", Image: "x",
		Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.mustStart(u)
	if r.ws(u).ContainerID != orphan {
		t.Fatalf("container id = %q, want the orphan %q to be adopted", r.ws(u).ContainerID, orphan)
	}
	if info, _ := r.dock.InspectContainer(ctx, orphan); !info.Running {
		t.Fatal("the adopted container was not started")
	}
}

func TestStartNeverTouchesAForeignContainerWithItsName(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	foreign, _ := r.dock.CreateContainer(ctx, docker.ContainerSpec{Name: "berth-ws-alice", Image: "other"})
	for _, labels := range []map[string]string{
		nil,
		{docker.LabelManaged: "true", docker.LabelUser: "mallory"}, // another user's
	} {
		r.dock.Vanish(foreign)
		foreign, _ = r.dock.CreateContainer(ctx, docker.ContainerSpec{Name: "berth-ws-alice", Image: "other", Labels: labels})
		if _, err := r.m.Start(ctx, u); err == nil || !strings.Contains(err.Error(), "did not create") {
			t.Fatalf("labels %v: err = %v, want a refusal", labels, err)
		}
		if _, err := r.dock.InspectContainer(ctx, foreign); err != nil {
			t.Fatalf("the foreign container was removed: %v", err)
		}
		if r.ws(u).State != store.StateOffline || r.ws(u).ContainerID != "" {
			t.Fatalf("workspace: %+v", r.ws(u))
		}
	}
}

func TestNoCapacity(t *testing.T) {
	r := newRig(t, func(c *Config) { c.UDPPortMin, c.UDPPortMax = 30000, 30007 }) // room for one workspace
	alice, bob := r.user("alice"), r.user("bob")
	r.mustStart(alice)
	_, err := r.m.Start(ctx, bob)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
	if r.ws(bob).State != store.StateOffline {
		t.Fatalf("bob's state = %s", r.ws(bob).State)
	}
	// Once alice's container is gone her block is released and bob fits.
	r.dock.Vanish(r.containerID("alice"))
	if s, _ := r.m.Status(ctx, alice); s.Workspace.State != store.StateOffline {
		t.Fatalf("alice's state = %s", s.Workspace.State)
	}
	r.mustStart(bob)
	if r.ws(bob).UDPPortStart != 30000 {
		t.Fatalf("bob's block starts at %d, want the released 30000", r.ws(bob).UDPPortStart)
	}
}

func TestUsersGetIsolatedNetworksAndDistinctPorts(t *testing.T) {
	r := newRig(t)
	alice, bob := r.user("alice"), r.user("bob")
	r.mustStart(alice)
	r.mustStart(bob)

	if got := r.dock.NetworkMembers("berth-net-alice"); strings.Join(got, ",") != "berth-traefik,berth-ws-alice" {
		t.Errorf("alice's network: %v", got)
	}
	if got := r.dock.NetworkMembers("berth-net-bob"); strings.Join(got, ",") != "berth-traefik,berth-ws-bob" {
		t.Errorf("bob's network: %v", got)
	}
	a, b := r.ws(alice), r.ws(bob)
	if a.UDPPortEnd >= b.UDPPortStart && b.UDPPortEnd >= a.UDPPortStart {
		t.Errorf("port blocks overlap: %d-%d and %d-%d", a.UDPPortStart, a.UDPPortEnd, b.UDPPortStart, b.UDPPortEnd)
	}
	if a.NekoPassword == b.NekoPassword {
		t.Error("two users share a neko password")
	}
}

func TestStatusCatchesUpWithContainerChanges(t *testing.T) {
	t.Run("crash", func(t *testing.T) {
		r := newRig(t)
		u := r.user("alice")
		r.mustStart(u)
		id := r.containerID("alice")
		r.dock.SetHealth(id, docker.HealthHealthy)
		r.m.Status(ctx, u)

		r.clock.Advance(time.Minute)
		r.dock.Crash(id, 137)
		// Berth only notices ten minutes later; the stop time must still be when
		// the container actually died (the reclaim timer in M4 counts from it).
		r.clock.Advance(10 * time.Minute)
		s, _ := r.m.Status(ctx, u)
		if s.Workspace.State != store.StateStopped || s.Workspace.ContainerID != id ||
			s.Workspace.LastStoppedAt == nil || !s.Workspace.LastStoppedAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("after crash: %+v", s.Workspace)
		}
	})
	t.Run("crash while starting", func(t *testing.T) {
		r := newRig(t)
		u := r.user("alice")
		r.mustStart(u)
		r.dock.Crash(r.containerID("alice"), 1)
		if s, _ := r.m.Status(ctx, u); s.Workspace.State != store.StateStopped {
			t.Fatalf("state = %s", s.Workspace.State)
		}
	})
	t.Run("removed by hand", func(t *testing.T) {
		r := newRig(t)
		u := r.user("alice")
		r.mustStart(u)
		r.dock.Vanish(r.containerID("alice"))
		s, _ := r.m.Status(ctx, u)
		if s.Workspace.State != store.StateOffline || s.Workspace.ContainerID != "" || s.Workspace.UDPPortStart != 0 {
			t.Fatalf("after vanish: %+v", s.Workspace)
		}
	})
	t.Run("started by hand while stopped", func(t *testing.T) {
		r := newRig(t)
		u := r.user("alice")
		r.mustStart(u)
		id := r.containerID("alice")
		r.dock.SetHealth(id, docker.HealthHealthy)
		r.m.Status(ctx, u)
		r.m.Stop(ctx, u)
		r.dock.StartContainer(ctx, id)
		if s, _ := r.m.Status(ctx, u); s.Workspace.State != store.StateStarting {
			t.Fatalf("state = %s, want starting (then running once healthy)", s.Workspace.State)
		}
		r.dock.SetHealth(id, docker.HealthHealthy)
		if s, _ := r.m.Status(ctx, u); s.Workspace.State != store.StateRunning {
			t.Fatalf("state = %s, want running", s.Workspace.State)
		}
	})
	t.Run("offline stays offline", func(t *testing.T) {
		r := newRig(t)
		u := r.user("alice")
		s, err := r.m.Status(ctx, u)
		if err != nil || s.Workspace.State != store.StateOffline || s.Stats != nil {
			t.Fatalf("%+v %v", s, err)
		}
		if r.dock.CallCount("InspectContainer") != 0 {
			t.Fatal("an offline workspace should not touch Docker")
		}
	})
}

func TestStatsFailureDoesNotFailStatus(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	r.dock.SetHealth(r.containerID("alice"), docker.HealthHealthy)
	r.dock.FailNext("ContainerStats", errors.New("stats are slow today"))
	s, err := r.m.Status(ctx, u)
	if err != nil || s.Workspace.State != store.StateRunning || s.Stats != nil {
		t.Fatalf("status %+v err %v", s, err)
	}
}

func TestHeartbeat(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	if err := r.m.Heartbeat(ctx, u); err != nil {
		t.Fatal(err)
	}
	if r.ws(u).LastActiveAt != nil {
		t.Fatal("an offline workspace recorded activity")
	}
	r.mustStart(u)
	r.clock.Advance(30 * time.Second)
	if err := r.m.Heartbeat(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got := r.ws(u).LastActiveAt; got == nil || !got.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("LastActiveAt = %v", got)
	}
}

func TestBadUsernameNeverReachesDocker(t *testing.T) {
	r := newRig(t)
	for _, name := range []string{"", "Alice", "al/ice", "a b", "x`) || Host(`y", "../bob"} {
		u := store.User{ID: 1, Username: name}
		if _, err := r.m.Start(ctx, u); !errors.Is(err, ErrBadUsername) {
			t.Errorf("Start(%q) err = %v", name, err)
		}
		if _, err := r.m.Stop(ctx, u); !errors.Is(err, ErrBadUsername) {
			t.Errorf("Stop(%q) err = %v", name, err)
		}
	}
	if n := len(r.dock.Calls()); n != 0 {
		t.Fatalf("%d Docker calls were made: %v", n, r.dock.Calls())
	}
}

func TestReattachTraefik(t *testing.T) {
	r := newRig(t)
	alice, bob := r.user("alice"), r.user("bob")
	r.mustStart(alice)
	r.mustStart(bob)

	// Traefik is recreated (docker compose up -d with a new image): the new
	// container has the same name but none of the old attachments.
	r.dock.Vanish(r.traefik)
	newTraefik := r.dock.AddContainer(traefikName)
	_ = newTraefik
	if got := r.dock.NetworkMembers("berth-net-alice"); strings.Contains(strings.Join(got, ","), traefikName) {
		t.Fatalf("the fake should have dropped the attachment: %v", got)
	}

	if err := r.m.ReattachTraefik(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"berth-net-alice", "berth-net-bob"} {
		if !strings.Contains(strings.Join(r.dock.NetworkMembers(n), ","), traefikName) {
			t.Errorf("Traefik not re-attached to %s: %v", n, r.dock.NetworkMembers(n))
		}
	}
	// Idempotent.
	if err := r.m.ReattachTraefik(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReattachTraefikKeepsGoingAfterOneFailure(t *testing.T) {
	r := newRig(t)
	r.mustStart(r.user("alice"))
	r.mustStart(r.user("bob"))
	r.dock.Vanish(r.traefik)
	r.dock.AddContainer(traefikName)

	r.dock.FailNext("ConnectToNetwork", errors.New("daemon hiccup")) // hits berth-net-alice
	err := r.m.ReattachTraefik(ctx)
	if err == nil || !strings.Contains(err.Error(), "berth-net-alice") {
		t.Fatalf("err = %v, want it to name the failed network", err)
	}
	if !strings.Contains(strings.Join(r.dock.NetworkMembers("berth-net-bob"), ","), traefikName) {
		t.Fatal("one network failing stopped the others from being repaired")
	}
}

func TestReattachTraefikDisabledAndErrors(t *testing.T) {
	r := newRig(t, func(c *Config) { c.TraefikContainer = "" })
	if err := r.m.ReattachTraefik(ctx); err != nil || r.dock.CallCount("ManagedNetworks") != 0 {
		t.Fatalf("disabled reattach did work: %v", err)
	}
	r2 := newRig(t)
	r2.dock.FailNext("ManagedNetworks", fmt.Errorf("docker is down"))
	if err := r2.m.ReattachTraefik(ctx); err == nil {
		t.Fatal("a Docker failure was swallowed")
	}
}

// If `docker stop` fails, the container is still running, so the workspace must
// not be recorded as stopped.
func TestStopFailureLeavesTheWorkspaceRunning(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	id := r.containerID("alice")
	r.dock.SetHealth(id, docker.HealthHealthy)
	r.m.Status(ctx, u)

	r.dock.FailNext("StopContainer", errors.New("context deadline exceeded"))
	if _, err := r.m.Stop(ctx, u); err == nil {
		t.Fatal("a failed stop was reported as success")
	}
	if ws := r.ws(u); ws.State != store.StateRunning || ws.LastStoppedAt != nil {
		t.Fatalf("workspace after a failed stop: %+v", ws)
	}
	if info, _ := r.dock.InspectContainer(ctx, id); !info.Running {
		t.Fatal("the fake stopped the container despite the failure")
	}
	// Trying again works.
	if s, err := r.m.Stop(ctx, u); err != nil || s.Workspace.State != store.StateStopped {
		t.Fatalf("retry: %v %v", s.Workspace.State, err)
	}
}

// A Docker failure while observing must surface; guessing from stale database
// state would report a dead workspace as running.
func TestStatusSurfacesDockerErrors(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	r.dock.FailNext("InspectContainer", errors.New("daemon error"))
	if _, err := r.m.Status(ctx, u); err == nil || !strings.Contains(err.Error(), "daemon error") {
		t.Fatalf("err = %v, want the Docker error", err)
	}
	// Nothing was changed by the failed observation.
	if r.ws(u).State != store.StateStarting {
		t.Fatalf("state = %s", r.ws(u).State)
	}
}
