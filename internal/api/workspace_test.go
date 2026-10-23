package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

// wsEnv is an api env whose workspace backend is the real Manager over a fake
// Docker, so the whole request path is exercised except the Docker daemon.
type wsEnv struct {
	*env
	dock    *docker.Fake
	traefik string
}

func newWSEnv(t *testing.T, mutate ...func(*workspace.Config)) *wsEnv {
	t.Helper()
	w := &wsEnv{}
	w.env = newEnv(t, func(d *Deps) {
		w.dock = docker.NewFake(d.Now)
		w.traefik = w.dock.AddContainer("berth-traefik")
		cfg := workspace.Config{
			Image: "berth/workspace:latest", TraefikContainer: "berth-traefik", Entrypoint: "web",
			UDPPortMin: 30000, UDPPortMax: 30031, UDPPortsPerWorkspace: 8,
			Resolution: "1280x720@30", ShmSizeMB: 2048, PidsLimit: 2048,
			StopTimeout: 15 * time.Second, StartTimeout: 30 * time.Second,
		}
		for _, m := range mutate {
			m(&cfg)
		}
		mgr, err := workspace.NewManager(d.Store, w.dock, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), d.Now)
		if err != nil {
			t.Fatal(err)
		}
		d.Workspaces = mgr
	})
	return w
}

func wsBody(r resp) map[string]any { return r.json()["workspace"].(map[string]any) }

func TestWorkspaceEndpointsRequireTheAPISession(t *testing.T) {
	e := newWSEnv(t)
	_, ws := e.registerAndLoginBoth("alice", "correct horse")
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/workspace"}, {"POST", "/api/workspace/start"},
		{"POST", "/api/workspace/stop"}, {"POST", "/api/workspace/heartbeat"},
	} {
		if code := e.do(ep.method, ep.path, "", nil).Code; code != 401 {
			t.Errorf("%s %s without a session: %d, want 401", ep.method, ep.path, code)
		}
		// The workspace-scoped token must not open the API.
		asAPI := &http.Cookie{Name: sessionCookieName, Value: ws.Value}
		if code := e.do(ep.method, ep.path, "", nil, asAPI).Code; code != 401 {
			t.Errorf("%s %s with the workspace token: %d, want 401", ep.method, ep.path, code)
		}
	}
	if n := len(e.dock.Calls()); n != 0 {
		t.Errorf("unauthenticated requests reached Docker: %v", e.dock.Calls())
	}
}

func TestWorkspaceEndpointsWithoutAContainerBackend(t *testing.T) {
	e := newEnv(t) // Workspaces is nil
	c := e.registerAndLogin("alice", "correct horse")
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/workspace"}, {"POST", "/api/workspace/start"},
		{"POST", "/api/workspace/stop"}, {"POST", "/api/workspace/heartbeat"},
	} {
		if code := e.do(ep.method, ep.path, "", nil, c).Code; code != 503 {
			t.Errorf("%s %s: %d, want 503", ep.method, ep.path, code)
		}
	}
	if e.do("GET", "/api/me", "", nil, c).Code != 200 {
		t.Error("accounts must keep working without a container backend")
	}
}

func TestWorkspaceLifecycleOverHTTP(t *testing.T) {
	e := newWSEnv(t)
	c := e.registerAndLogin("alice", "correct horse")

	// Offline: nothing to report beyond the limits.
	r := e.do("GET", "/api/workspace", "", nil, c)
	if r.Code != 200 {
		t.Fatalf("status %d: %s", r.Code, r.Body)
	}
	ws := wsBody(r)
	if ws["state"] != "offline" || ws["health"] != "none" || ws["usage"] != nil || ws["mem_limit_mb"] != float64(2048) ||
		ws["cpu_limit"] != float64(2) || ws["last_active_at"] != nil {
		t.Errorf("offline workspace: %v", ws)
	}
	if _, has := ws["neko_password"]; has {
		t.Error("an offline workspace exposes a neko password")
	}

	// Power on: the container is started, readiness comes later.
	r = e.do("POST", "/api/workspace/start", "", nil, c)
	if r.Code != 200 {
		t.Fatalf("start: %d %s", r.Code, r.Body)
	}
	ws = wsBody(r)
	pw, _ := ws["neko_password"].(string)
	if ws["state"] != "starting" || ws["health"] != "starting" || len(pw) < 20 || ws["usage"] != nil {
		t.Errorf("after start: %v", ws)
	}
	if ws["last_active_at"] == nil {
		t.Error("starting did not record activity")
	}

	// The healthcheck passes: the next poll reports running with usage numbers.
	id := e.dock.ContainerIDByName("berth-ws-alice")
	e.dock.SetHealth(id, docker.HealthHealthy)
	e.dock.SetStats(id, docker.Stats{CPUPercent: 37.5, MemUsageBytes: 512 << 20, MemLimitBytes: 2048 << 20, Pids: 80})
	ws = wsBody(e.do("GET", "/api/workspace", "", nil, c))
	usage, _ := ws["usage"].(map[string]any)
	if ws["state"] != "running" || ws["health"] != "healthy" || ws["neko_password"] != pw ||
		usage["cpu_percent"] != 37.5 || usage["mem_bytes"] != float64(512<<20) ||
		usage["mem_limit_bytes"] != float64(2048<<20) || usage["pids"] != float64(80) {
		t.Errorf("running workspace: %v", ws)
	}

	// Starting again while running changes nothing.
	if code := e.do("POST", "/api/workspace/start", "", nil, c).Code; code != 200 {
		t.Errorf("second start: %d", code)
	}
	if n := e.dock.CallCount("CreateContainer"); n != 1 {
		t.Errorf("CreateContainer called %d times", n)
	}

	// Heartbeat.
	e.clock.Advance(30 * time.Second)
	if code := e.do("POST", "/api/workspace/heartbeat", "", nil, c).Code; code != 204 {
		t.Errorf("heartbeat: %d", code)
	}

	// Power off: stopped, the password is no longer handed out, the stop time is set.
	e.clock.Advance(time.Minute)
	r = e.do("POST", "/api/workspace/stop", "", nil, c)
	ws = wsBody(r)
	if r.Code != 200 || ws["state"] != "stopped" || ws["last_stopped_at"] == nil || ws["usage"] != nil {
		t.Errorf("stop: %d %v", r.Code, ws)
	}
	if _, has := ws["neko_password"]; has {
		t.Error("a stopped workspace exposes the neko password")
	}
	// /api/me reflects it too.
	if st := e.do("GET", "/api/me", "", nil, c).json()["workspace"].(map[string]any)["state"]; st != "stopped" {
		t.Errorf("/api/me workspace state = %v", st)
	}
}

func TestTrailingSlashOnTheWorkspaceResource(t *testing.T) {
	e := newWSEnv(t)
	c := e.registerAndLogin("alice", "correct horse")
	for _, path := range []string{"/api/workspace", "/api/workspace/"} {
		if code := e.do("GET", path, "", nil, c).Code; code != 200 {
			t.Errorf("GET %s: %d", path, code)
		}
	}
}

func TestOneUserCannotTouchAnothersWorkspace(t *testing.T) {
	e := newWSEnv(t)
	alice := e.registerAndLogin("alice", "correct horse")
	bob := e.registerAndLogin("bob", "battery staple")

	e.do("POST", "/api/workspace/start", "", nil, alice)
	if got := e.dock.Containers(); strings.Join(got, ",") != "berth-traefik,berth-ws-alice" {
		t.Fatalf("containers after alice's start: %v", got)
	}
	// Bob's view is his own, untouched workspace.
	if st := wsBody(e.do("GET", "/api/workspace", "", nil, bob))["state"]; st != "offline" {
		t.Errorf("bob sees state %v", st)
	}
	// Bob powering off does not stop Alice's container.
	e.do("POST", "/api/workspace/stop", "", nil, bob)
	info, _ := e.dock.InspectContainer(ctx0, "berth-ws-alice")
	if !info.Running {
		t.Error("bob's stop request stopped alice's container")
	}
	// There is no way to name another user in a request: the body is ignored.
	e.do("POST", "/api/workspace/stop", `{"username":"alice"}`, nil, bob)
	info, _ = e.dock.InspectContainer(ctx0, "berth-ws-alice")
	if !info.Running {
		t.Error("a body naming alice stopped her container")
	}
}

func TestWorkspaceErrorsAreMappedAndNeverLeakInternals(t *testing.T) {
	const secret = "dial unix /var/run/docker.sock: secret-host-detail"
	cases := []struct {
		name   string
		setup  func(*wsEnv)
		status int
		want   string
	}{
		{"docker daemon down", func(e *wsEnv) {
			e.dock.FailNext("EnsureNetwork", errors.Join(docker.ErrUnavailable, errors.New(secret)))
		}, 503, "unavailable"},
		{"unexpected docker failure", func(e *wsEnv) { e.dock.FailNext("EnsureVolume", errors.New(secret)) }, 500, "internal error"},
		{"image missing", func(e *wsEnv) {
			e.dock.FailNext("CreateContainer", errors.New("No such image: berth/workspace:latest "+secret))
		}, 500, "internal error"},
		{"proxy container missing", func(e *wsEnv) { e.dock.Vanish(e.traefik) }, 500, "internal error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newWSEnv(t)
			c := e.registerAndLogin("alice", "correct horse")
			tc.setup(e)
			r := e.do("POST", "/api/workspace/start", "", nil, c)
			if r.Code != tc.status || !strings.Contains(r.Body.String(), tc.want) {
				t.Fatalf("status %d body %s, want %d containing %q", r.Code, r.Body, tc.status, tc.want)
			}
			for _, leak := range []string{"secret-host-detail", "docker.sock", "berth-traefik", "No such image", "berth-ws-"} {
				if strings.Contains(r.Body.String(), leak) {
					t.Errorf("response leaks %q: %s", leak, r.Body)
				}
			}
			// A failed start leaves the workspace startable.
			if st := wsBody(e.do("GET", "/api/workspace", "", nil, c))["state"]; st != "offline" {
				t.Errorf("state after a failed start = %v", st)
			}
		})
	}
}

func TestNoCapacityIs503(t *testing.T) {
	e := newWSEnv(t, func(c *workspace.Config) { c.UDPPortMin, c.UDPPortMax = 30000, 30007 })
	alice := e.registerAndLogin("alice", "correct horse")
	bob := e.registerAndLogin("bob", "battery staple")
	if code := e.do("POST", "/api/workspace/start", "", nil, alice).Code; code != 200 {
		t.Fatalf("alice: %d", code)
	}
	r := e.do("POST", "/api/workspace/start", "", nil, bob)
	if r.Code != 503 || !strings.Contains(r.Body.String(), "capacity") {
		t.Fatalf("bob: %d %s", r.Code, r.Body)
	}
}

func TestStopWhileAStartIsCreatingTheContainerIsAConflict(t *testing.T) {
	e := newWSEnv(t)
	c := e.registerAndLogin("alice", "correct horse")
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.dock.SetHook("CreateContainer", func(context.Context) {
		once.Do(func() { close(reached); <-release })
	})
	started := make(chan int, 1)
	go func() { started <- e.do("POST", "/api/workspace/start", "", nil, c).Code }()
	<-reached // the start is inside docker create
	r := e.do("POST", "/api/workspace/stop", "", nil, c)
	if r.Code != 409 || !strings.Contains(r.Body.String(), "busy") {
		t.Fatalf("stop during a start: %d %s, want 409", r.Code, r.Body)
	}
	close(release)
	if code := <-started; code != 200 {
		t.Fatalf("the start: %d", code)
	}
}

func TestStopOfAStartThatDiedWithTheOldProcessRecovers(t *testing.T) {
	e := newWSEnv(t)
	c := e.registerAndLogin("alice", "correct horse")
	u, _ := e.store.UserByUsername(ctx0, "alice")
	// The previous backend process claimed the row and died: nothing is running.
	if _, err := e.store.TransitionWorkspace(ctx0, u.ID, []store.State{store.StateOffline}, store.StateStarting, t0); err != nil {
		t.Fatal(err)
	}
	r := e.do("POST", "/api/workspace/stop", "", nil, c)
	if r.Code != 200 || wsBody(r)["state"] != "offline" {
		t.Fatalf("stop: %d %s; the workspace must not stay wedged in 'starting'", r.Code, r.Body)
	}
}

// A hostile page on a sibling origin of the same site can make the victim's
// browser send a cookie-bearing POST, but only as a "simple" request: a form
// encoding, never JSON. These endpoints have no body, so the content type is the
// only thing that tells such a request from the SPA's.
func TestStateChangingRequestsMustBeDeclaredAsJSON(t *testing.T) {
	e := newWSEnv(t)
	c := e.registerAndLogin("alice", "correct horse")
	e.do("POST", "/api/workspace/start", "", nil, c)
	for _, path := range []string{"/api/workspace/start", "/api/workspace/stop", "/api/workspace/heartbeat", "/api/auth/logout"} {
		for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", "multipart/form-data; boundary=x", ""} {
			r := e.do("POST", path, "", map[string]string{"Content-Type": ct}, c)
			if r.Code != 415 {
				t.Errorf("POST %s as %q: %d, want 415", path, ct, r.Code)
			}
		}
	}
	if st := wsBody(e.do("GET", "/api/workspace", "", nil, c))["state"]; st != "starting" {
		t.Errorf("a refused request changed the workspace: state = %v", st)
	}
	// The session survived the refused logout.
	if code := e.do("GET", "/api/me", "", nil, c).Code; code != 200 {
		t.Errorf("/api/me after refused logouts: %d", code)
	}
	// Without a session the answer is still 401, not a hint about content types.
	if code := e.do("POST", "/api/workspace/stop", "", map[string]string{"Content-Type": "text/plain"}).Code; code != 401 {
		t.Errorf("anonymous: %d, want 401", code)
	}
}

func TestHeartbeatRecordsActivity(t *testing.T) {
	e := newWSEnv(t)
	c := e.registerAndLogin("alice", "correct horse")
	e.do("POST", "/api/workspace/start", "", nil, c)
	id := e.dock.ContainerIDByName("berth-ws-alice")
	e.dock.SetHealth(id, docker.HealthHealthy)
	started := wsBody(e.do("GET", "/api/workspace", "", nil, c))["last_active_at"]

	e.clock.Advance(10 * time.Minute)
	if code := e.do("POST", "/api/workspace/heartbeat", "", nil, c).Code; code != 204 {
		t.Fatalf("heartbeat: %d", code)
	}
	after := wsBody(e.do("GET", "/api/workspace", "", nil, c))["last_active_at"]
	if after == nil || after == started {
		t.Fatalf("last_active_at did not move: %v -> %v", started, after)
	}
	want := e.clock.Now().UTC().Format(time.RFC3339)
	if after != want {
		t.Fatalf("last_active_at = %v, want %v", after, want)
	}
}

func TestWorkspaceResponsesAreNotCacheable(t *testing.T) {
	e := newWSEnv(t)
	c := e.registerAndLogin("alice", "correct horse")
	e.do("POST", "/api/workspace/start", "", nil, c)
	r := e.do("GET", "/api/workspace", "", nil, c)
	if r.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q: the response carries a password", r.Header().Get("Cache-Control"))
	}
}
