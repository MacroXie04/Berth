package workspace

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

// hasEnvKey reports whether the environment sets key, whatever its value.
func hasEnvKey(env []string, key string) bool {
	return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, key+"=") })
}

// One UDP port per workspace is the default (BERTH_UDP_PORTS_PER_WORKSPACE=1).
// These tests drive it from the manager, through the allocation in the store and
// the container spec, and check the literal strings neko is configured with.
func TestOnePortPerWorkspaceEndToEnd(t *testing.T) {
	r := newRig(t, func(c *Config) { c.UDPPortsPerWorkspace = 1 })
	alice, bob := r.user("alice"), r.user("bob")
	r.mustStart(alice)
	r.mustStart(bob)

	for i, u := range []store.User{alice, bob} {
		port := 30000 + i
		ws := r.ws(u)
		if ws.UDPPortStart != port || ws.UDPPortEnd != port {
			t.Errorf("%s: recorded block %d-%d, want exactly %d (start == end)", u.Username, ws.UDPPortStart, ws.UDPPortEnd, port)
		}
		spec, ok := r.dock.Spec(ws.ContainerID)
		if !ok {
			t.Fatalf("%s: no spec", u.Username)
		}
		// Exactly one port is published: the Docker wrapper publishes
		// UDPPortStart..UDPPortEnd port for port (TestCreatePublishesExactlyTheSpecifiedUDPPorts).
		if spec.UDPPortStart != port || spec.UDPPortEnd != port {
			t.Errorf("%s: spec publishes %d-%d, want only %d", u.Username, spec.UDPPortStart, spec.UDPPortEnd, port)
		}
		if want := "NEKO_WEBRTC_UDPMUX=" + itoa(port); !slices.Contains(spec.Env, want) {
			t.Errorf("%s: env lacks %q:\n%s", u.Username, want, strings.Join(spec.Env, "\n"))
		}
		if hasEnvKey(spec.Env, "NEKO_WEBRTC_EPR") {
			t.Errorf("%s: env sets NEKO_WEBRTC_EPR next to the multiplexer:\n%s", u.Username, strings.Join(spec.Env, "\n"))
		}
	}

	// The size is the configuration's, not a constant: a range that holds exactly
	// two ports holds exactly two workspaces.
	r2 := newRig(t, func(c *Config) { c.UDPPortsPerWorkspace = 1; c.UDPPortMin, c.UDPPortMax = 30000, 30001 })
	a, b, c := r2.user("alice"), r2.user("bob"), r2.user("carol")
	r2.mustStart(a)
	r2.mustStart(b)
	if _, err := r2.m.Start(ctx, c); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("third workspace in a two-port range: err = %v, want ErrNoCapacity", err)
	}
}

// A block that was recorded under an earlier BERTH_UDP_PORTS_PER_WORKSPACE keeps
// its size: the value only decides the blocks of workspaces that have none.
func TestAnExistingBlockKeepsItsSizeWhenTheDefaultChanges(t *testing.T) {
	r := newRig(t, func(c *Config) { c.UDPPortsPerWorkspace = 8 }) // the earlier default
	alice := r.user("alice")
	r.mustStart(alice)
	oldID := r.containerID("alice")
	r.dock.SetHealth(oldID, docker.HealthHealthy)
	r.m.Status(ctx, alice)
	if _, err := r.m.Stop(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if ws := r.ws(alice); ws.UDPPortStart != 30000 || ws.UDPPortEnd != 30007 {
		t.Fatalf("setup: block %d-%d, want 30000-30007", ws.UDPPortStart, ws.UDPPortEnd)
	}

	// The operator redeploys with BERTH_UDP_PORTS_PER_WORKSPACE=1.
	cfg := testConfig()
	cfg.UDPPortsPerWorkspace = 1
	m1, err := NewManager(r.st, r.dock, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), r.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	checkRange := func(what, id string) {
		t.Helper()
		ws := r.ws(alice)
		if ws.UDPPortStart != 30000 || ws.UDPPortEnd != 30007 {
			t.Errorf("%s: recorded block %d-%d, want it unchanged at 30000-30007", what, ws.UDPPortStart, ws.UDPPortEnd)
		}
		spec, ok := r.dock.Spec(id)
		if !ok {
			t.Fatalf("%s: no spec for %s", what, id)
		}
		if spec.UDPPortStart != 30000 || spec.UDPPortEnd != 30007 {
			t.Errorf("%s: spec publishes %d-%d, want 30000-30007", what, spec.UDPPortStart, spec.UDPPortEnd)
		}
		if !slices.Contains(spec.Env, "NEKO_WEBRTC_EPR=30000-30007") {
			t.Errorf("%s: env lacks NEKO_WEBRTC_EPR=30000-30007:\n%s", what, strings.Join(spec.Env, "\n"))
		}
		if hasEnvKey(spec.Env, "NEKO_WEBRTC_UDPMUX") {
			t.Errorf("%s: env sets the multiplexer for an eight-port block", what)
		}
	}

	// A kept container just starts, as it was created.
	if _, err := m1.Start(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if n := r.dock.CallCount("CreateContainer"); n != 1 {
		t.Errorf("CreateContainer called %d times; a kept container must not be recreated", n)
	}
	checkRange("kept container", oldID)

	// Removed by hand while stopped: the replacement gets the same block and the
	// same way of using it, not a fresh one-port block.
	if _, err := m1.Stop(ctx, alice); err != nil {
		t.Fatal(err)
	}
	r.dock.Vanish(oldID)
	if _, err := m1.Start(ctx, alice); err != nil {
		t.Fatal(err)
	}
	newID := r.containerID("alice")
	if newID == "" || newID == oldID {
		t.Fatalf("container id %q (was %q): expected a recreated container", newID, oldID)
	}
	checkRange("recreated container", newID)

	// A workspace without a block gets the new size, after the old block.
	bob := r.user("bob")
	if _, err := m1.Start(ctx, bob); err != nil {
		t.Fatal(err)
	}
	ws := r.ws(bob)
	if ws.UDPPortStart != 30008 || ws.UDPPortEnd != 30008 {
		t.Errorf("bob: block %d-%d, want exactly 30008", ws.UDPPortStart, ws.UDPPortEnd)
	}
	spec, _ := r.dock.Spec(ws.ContainerID)
	if !slices.Contains(spec.Env, "NEKO_WEBRTC_UDPMUX=30008") || hasEnvKey(spec.Env, "NEKO_WEBRTC_EPR") {
		t.Errorf("bob: env:\n%s", strings.Join(spec.Env, "\n"))
	}
}
