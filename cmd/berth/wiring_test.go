package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MacroXie04/Berth/internal/config"
	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/reaper"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

func loadConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Every setting reaches the manager, under its own name: distinct values for
// all of them, so a dropped, constant or swapped field shows.
func TestWorkspaceConfigCarriesEverySetting(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		"BERTH_WORKSPACE_IMAGE":         "example/ws:1",
		"BERTH_TRAEFIK_CONTAINER":       "my-traefik",
		"BERTH_TRAEFIK_ENTRYPOINT":      "websecure",
		"BERTH_UDP_PORT_MIN":            "31000",
		"BERTH_UDP_PORT_MAX":            "31999",
		"BERTH_UDP_PORTS_PER_WORKSPACE": "4",
		"BERTH_RESOLUTION":              "1920x1080@25",
		"BERTH_PUBLIC_IP":               "203.0.113.9",
		"BERTH_SHM_MB":                  "512",
		"BERTH_PIDS_LIMIT":              "777",
		"BERTH_STOP_TIMEOUT":            "21s",
		"BERTH_START_TIMEOUT":           "55s",
	})
	got := workspaceConfig(cfg)
	want := workspace.Config{
		Image:                "example/ws:1",
		TraefikContainer:     "my-traefik",
		Entrypoint:           "websecure",
		UDPPortMin:           31000,
		UDPPortMax:           31999,
		UDPPortsPerWorkspace: 4,
		Resolution:           "1920x1080@25",
		PublicIP:             "203.0.113.9",
		ShmSizeMB:            512,
		PidsLimit:            777,
		StopTimeout:          21 * time.Second,
		StartTimeout:         55 * time.Second,
	}
	if got != want {
		t.Errorf("workspaceConfig:\n got %+v\nwant %+v", got, want)
	}
	// A field added to workspace.Config later must be mapped too: with every
	// input set, none of the outputs may stay at its zero value.
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("workspace.Config.%s is not set by workspaceConfig", v.Type().Field(i).Name)
		}
	}
}

// The defaults are what a deployment without a .env runs with: one UDP port per
// workspace and no public address.
func TestWorkspaceConfigFromTheDefaultsIsValid(t *testing.T) {
	got := workspaceConfig(loadConfig(t, nil))
	if got.UDPPortsPerWorkspace != 1 {
		t.Errorf("UDPPortsPerWorkspace = %d, want the default of 1", got.UDPPortsPerWorkspace)
	}
	if got.PublicIP != "" {
		t.Errorf("PublicIP = %q by default", got.PublicIP)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the default configuration does not validate: %v", err)
	}
}

func TestPublicIPWarning(t *testing.T) {
	if msg := publicIPWarning(loadConfig(t, nil)); !strings.Contains(msg, "BERTH_PUBLIC_IP is not set") {
		t.Errorf("no address set: warning %q, want one that says BERTH_PUBLIC_IP is not set", msg)
	}
	if msg := publicIPWarning(loadConfig(t, map[string]string{"BERTH_PUBLIC_IP": "203.0.113.9"})); msg != "" {
		t.Errorf("address set: warning %q, want none", msg)
	}
	if msg := publicIPWarning(loadConfig(t, map[string]string{"BERTH_PUBLIC_IP": "  "})); msg == "" {
		t.Error("a blank BERTH_PUBLIC_IP counts as unset and must warn")
	}
}

// From environment variables to the container a user gets, through the real
// configuration loader, the manager, the allocator and the container spec.
func TestNewWorkspaceManagerWiresTheSettingsIntoContainers(t *testing.T) {
	type result struct {
		env   []string
		start int
		end   int
		logs  string
	}
	run := func(t *testing.T, env map[string]string) result {
		t.Helper()
		ctx := context.Background()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "berth.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		dock := docker.NewFake(nil)
		dock.AddContainer("berth-traefik")
		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))

		m, err := newWorkspaceManager(loadConfig(t, env), st, dock, log)
		if err != nil {
			t.Fatal(err)
		}
		u, _, err := st.CreateUser(ctx, store.NewUser{
			Username: "alice", PasswordHash: "x", VolumeName: workspace.VolumeName("alice"), MemLimitMB: 2048, CPULimit: 2,
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Start(ctx, u); err != nil {
			t.Fatal(err)
		}
		spec, ok := dock.Spec(dock.ContainerIDByName(workspace.ContainerName("alice")))
		if !ok {
			t.Fatal("no container was created")
		}
		return result{spec.Env, spec.UDPPortStart, spec.UDPPortEnd, logs.String()}
	}
	hasKey := func(env []string, key string) bool {
		return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, key+"=") })
	}

	t.Run("defaults: one multiplexed port, no public address, a warning", func(t *testing.T) {
		r := run(t, nil)
		if r.start != 30000 || r.end != 30000 {
			t.Errorf("published ports %d-%d, want only 30000", r.start, r.end)
		}
		if !slices.Contains(r.env, "NEKO_WEBRTC_UDPMUX=30000") || hasKey(r.env, "NEKO_WEBRTC_EPR") {
			t.Errorf("env:\n%s", strings.Join(r.env, "\n"))
		}
		if hasKey(r.env, "NEKO_WEBRTC_NAT1TO1") {
			t.Error("NEKO_WEBRTC_NAT1TO1 set without BERTH_PUBLIC_IP")
		}
		if !strings.Contains(r.logs, "level=WARN") || !strings.Contains(r.logs, "BERTH_PUBLIC_IP is not set") {
			t.Errorf("no startup warning about BERTH_PUBLIC_IP:\n%s", r.logs)
		}
	})
	t.Run("a public address reaches neko and silences the warning", func(t *testing.T) {
		r := run(t, map[string]string{"BERTH_PUBLIC_IP": "203.0.113.9"})
		if !slices.Contains(r.env, "NEKO_WEBRTC_NAT1TO1=203.0.113.9") {
			t.Errorf("env lacks NEKO_WEBRTC_NAT1TO1=203.0.113.9:\n%s", strings.Join(r.env, "\n"))
		}
		if strings.Contains(r.logs, "BERTH_PUBLIC_IP is not set") {
			t.Errorf("warned although BERTH_PUBLIC_IP is set:\n%s", r.logs)
		}
	})
	t.Run("a larger block is a range, from the configured minimum", func(t *testing.T) {
		r := run(t, map[string]string{"BERTH_UDP_PORTS_PER_WORKSPACE": "3", "BERTH_UDP_PORT_MIN": "31000", "BERTH_UDP_PORT_MAX": "31099"})
		if r.start != 31000 || r.end != 31002 {
			t.Errorf("published ports %d-%d, want 31000-31002", r.start, r.end)
		}
		if !slices.Contains(r.env, "NEKO_WEBRTC_EPR=31000-31002") || hasKey(r.env, "NEKO_WEBRTC_UDPMUX") {
			t.Errorf("env:\n%s", strings.Join(r.env, "\n"))
		}
	})
}

// Every reaper setting reaches the reaper, under its own name.
func TestReaperConfigCarriesEverySetting(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		"BERTH_IDLE_TIMEOUT":          "17m",
		"BERTH_RECLAIM_AFTER":         "49h",
		"BERTH_REAPER_INTERVAL":       "37s",
		"BERTH_PROXY_ATTACH_INTERVAL": "7s",
		"BERTH_IDLE_CPU_PERCENT":      "12.5",
		"BERTH_STOP_TIMEOUT":          "21s",
	})
	got := reaperConfig(cfg)
	want := reaper.Config{
		IdleTimeout:    17 * time.Minute,
		ReclaimAfter:   49 * time.Hour,
		Interval:       37 * time.Second,
		AttachInterval: 7 * time.Second,
		IdleCPUPercent: 12.5,
		OpTimeout:      21*time.Second + 2*time.Minute,
	}
	if got != want {
		t.Errorf("reaperConfig:\n got %+v\nwant %+v", got, want)
	}
	if got.OpTimeout <= cfg.StopTimeout {
		t.Errorf("OpTimeout %v must exceed the stop timeout %v, or a graceful stop is cut short", got.OpTimeout, cfg.StopTimeout)
	}
	// Concurrency is deliberately not configurable (the reaper's default applies);
	// everything else must be set when every input is.
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		if name := v.Type().Field(i).Name; name != "Concurrency" && v.Field(i).IsZero() {
			t.Errorf("reaper.Config.%s is not set by reaperConfig", name)
		}
	}
}

// The defaults produce a configuration the reaper accepts.
func TestReaperConfigFromTheDefaultsIsValid(t *testing.T) {
	got := reaperConfig(loadConfig(t, nil))
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.IdleTimeout != 30*time.Minute || got.ReclaimAfter != 7*24*time.Hour || got.Interval != time.Minute || got.IdleCPUPercent != 5 {
		t.Errorf("defaults: %+v", got)
	}
}

func TestShortIdleTimeoutWarning(t *testing.T) {
	for _, tc := range []struct {
		timeout string
		warn    bool
	}{{"1m", true}, {"4m59s", true}, {"5m", false}, {"30m", false}} {
		msg := shortIdleTimeoutWarning(loadConfig(t, map[string]string{"BERTH_IDLE_TIMEOUT": tc.timeout}))
		if (msg != "") != tc.warn {
			t.Errorf("BERTH_IDLE_TIMEOUT=%s: warning %q, want one: %v", tc.timeout, msg, tc.warn)
		}
		if tc.warn && !strings.Contains(msg, tc.timeout) {
			t.Errorf("the warning should name the value: %q", msg)
		}
	}
}

// startReaper runs the reaper's loops and its stop function ends them: nothing is
// left running to touch the database or Docker after the process closes them.
func TestStartReaperRunsAndStopsTheLoops(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "berth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dock := docker.NewFake(nil)
	dock.AddContainer("berth-traefik")
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	m, err := newWorkspaceManager(loadConfig(t, nil), st, dock, log)
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := st.CreateUser(ctx, store.NewUser{
		Username: "alice", PasswordHash: "x", VolumeName: workspace.VolumeName("alice"), MemLimitMB: 2048, CPULimit: 2,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, u); err != nil {
		t.Fatal(err)
	}

	cfg := reaperConfig(loadConfig(t, nil))
	cfg.AttachInterval, cfg.Interval = 5*time.Millisecond, 5*time.Millisecond // the loops, fast
	rp, err := reaper.New(st, m, dock, cfg, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := dock.CallCount("ConnectToNetwork")
	stop := startReaper(ctx, rp, log)
	deadline := time.Now().Add(5 * time.Second)
	for dock.CallCount("ConnectToNetwork")-before < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := dock.CallCount("ConnectToNetwork") - before; n < 3 {
		t.Fatalf("the attach loop ran %d times, want it repeated", n)
	}
	began := time.Now()
	stop()
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("stop took %v", took)
	}
	n := dock.CallCount("ConnectToNetwork")
	time.Sleep(60 * time.Millisecond)
	if dock.CallCount("ConnectToNetwork") != n {
		t.Error("the reaper kept running after stop returned")
	}
	stop() // idempotent: a second call must not hang or panic
}

// The point of stop(): the database and the Docker client are closed right after
// it, so it must not return while a pass is still using them.
func TestStopReaperWaitsForAPassThatIsStillRunning(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "berth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dock := docker.NewFake(nil)
	dock.AddContainer("berth-traefik")
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	m, err := newWorkspaceManager(loadConfig(t, nil), st, dock, log)
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := st.CreateUser(ctx, store.NewUser{
		Username: "alice", PasswordHash: "x", VolumeName: workspace.VolumeName("alice"), MemLimitMB: 2048, CPULimit: 2,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, u); err != nil {
		t.Fatal(err)
	}

	var entered, finished atomic.Bool
	dock.SetHook("InspectContainer", func(context.Context) {
		if entered.CompareAndSwap(false, true) {
			time.Sleep(300 * time.Millisecond) // a Docker call that does not return at once (hooks ignore the context)
			finished.Store(true)
		}
	})
	cfg := reaperConfig(loadConfig(t, nil))
	cfg.Interval, cfg.AttachInterval = 5*time.Millisecond, time.Hour
	rp, err := reaper.New(st, m, dock, cfg, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := startReaper(ctx, rp, log)
	deadline := time.Now().Add(5 * time.Second)
	for !entered.Load() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !entered.Load() {
		t.Fatal("the reaper never reached Docker")
	}
	stop()
	if !finished.Load() {
		t.Error("stop returned while the pass was still inside Docker: the database would be closed under it")
	}
}
