//go:build integration

// Integration tests for the Moby client against a real Docker daemon:
//
//	go test -tags integration ./internal/docker
//
// They need the image named by BERTH_TEST_IMAGE (default berth/standin:test: any
// image whose default command keeps running works). Every object they create is
// named itest-<random> and removed afterwards.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

func testImage() string {
	if v := os.Getenv("BERTH_TEST_IMAGE"); v != "" {
		return v
	}
	return "berth/standin:test"
}

func newMoby(t *testing.T) (*Moby, context.Context) {
	t.Helper()
	m, err := NewMoby()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := m.Ping(ctx); err != nil {
		t.Skipf("no Docker daemon: %v", err)
	}
	return m, ctx
}

func uniq(t *testing.T, kind string) string {
	var b [4]byte
	rand.Read(b[:])
	return "itest-" + kind + "-" + hex.EncodeToString(b[:])
}

func TestMobyNetworks(t *testing.T) {
	m, ctx := newMoby(t)
	name := uniq(t, "net")
	labels := map[string]string{LabelManaged: "true", LabelUser: "someone"}
	t.Cleanup(func() { m.c.NetworkRemove(context.Background(), name, client.NetworkRemoveOptions{}) })

	// Creating is idempotent, including under a race.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- m.EnsureNetwork(ctx, name, labels) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureNetwork under a race: %v", err)
		}
	}

	names, err := m.ManagedNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
		if n == "bridge" || n == "host" || n == "none" {
			t.Errorf("ManagedNetworks returned the built-in network %q", n)
		}
	}
	if !found {
		t.Fatalf("labelled network %s missing from %v", name, names)
	}

	if err := m.ConnectToNetwork(ctx, "no-such-network-"+name, "no-such-container-"+name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown container err = %v, want ErrNotFound", err)
	}
}

func TestMobyContainerLifecycle(t *testing.T) {
	m, ctx := newMoby(t)
	netName, netName2 := uniq(t, "net"), uniq(t, "net2")
	vol := uniq(t, "vol")
	name := uniq(t, "ctr")
	other := uniq(t, "other")

	bg := context.Background()
	t.Cleanup(func() {
		m.RemoveContainer(bg, name)
		m.RemoveContainer(bg, other)
		m.c.NetworkRemove(bg, netName, client.NetworkRemoveOptions{})
		m.c.NetworkRemove(bg, netName2, client.NetworkRemoveOptions{})
		m.c.VolumeRemove(bg, vol, client.VolumeRemoveOptions{Force: true})
	})

	labels := map[string]string{LabelManaged: "true", LabelUser: "tester"}
	for _, n := range []string{netName, netName2} {
		if err := m.EnsureNetwork(ctx, n, labels); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.EnsureVolume(ctx, vol, labels); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureVolume(ctx, vol, labels); err != nil { // idempotent
		t.Fatalf("second EnsureVolume: %v", err)
	}

	spec := ContainerSpec{
		Name:         name,
		Hostname:     name,
		Image:        testImage(),
		Env:          []string{"NAME=itest"},
		Labels:       map[string]string{LabelManaged: "true", LabelUser: "tester", "x.custom": "1"},
		Network:      netName,
		Volumes:      []VolumeMount{{Volume: vol, Target: "/home/user"}},
		MemoryBytes:  256 << 20,
		NanoCPUs:     500_000_000,
		ShmSizeBytes: 64 << 20,
		PidsLimit:    512,
		CapDrop:      []string{"NET_RAW"},
		SecurityOpt:  []string{"no-new-privileges"},
		Init:         true,
		StopTimeout:  7 * time.Second,
		UDPPortStart: 0, // published ports are checked separately below
	}
	id, err := m.CreateContainer(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateContainer(ctx, spec); !errors.Is(err, ErrNameInUse) {
		t.Fatalf("duplicate name err = %v, want ErrNameInUse", err)
	}

	// What Docker actually recorded is what we asked for.
	raw, err := m.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hc, cfg := raw.Container.HostConfig, raw.Container.Config
	if hc.Memory != 256<<20 || hc.NanoCPUs != 500_000_000 || hc.ShmSize != 64<<20 {
		t.Errorf("resources: mem=%d cpus=%d shm=%d", hc.Memory, hc.NanoCPUs, hc.ShmSize)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != 512 {
		t.Errorf("pids limit = %v", hc.PidsLimit)
	}
	if len(hc.CapDrop) != 1 || strings.TrimPrefix(hc.CapDrop[0], "CAP_") != "NET_RAW" { // the daemon prefixes CAP_
		t.Errorf("cap drop = %v", hc.CapDrop)
	}
	if len(hc.SecurityOpt) != 1 || hc.SecurityOpt[0] != "no-new-privileges" {
		t.Errorf("security opt = %v", hc.SecurityOpt)
	}
	if hc.Init == nil || !*hc.Init {
		t.Errorf("init = %v", hc.Init)
	}
	if !hc.RestartPolicy.IsNone() {
		t.Errorf("restart policy = %+v; the backend, not Docker, owns restarts", hc.RestartPolicy)
	}
	if hc.Privileged {
		t.Error("container is privileged")
	}
	if cfg.StopTimeout == nil || *cfg.StopTimeout != 7 {
		t.Errorf("stop timeout = %v", cfg.StopTimeout)
	}
	if len(raw.Container.Mounts) != 1 || raw.Container.Mounts[0].Name != vol || raw.Container.Mounts[0].Destination != "/home/user" {
		t.Errorf("mounts = %+v", raw.Container.Mounts)
	}

	info, err := m.InspectContainer(ctx, name) // by name
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != id || info.Name != name || info.Running || info.Status != "created" || info.Health != HealthNone ||
		info.Labels["x.custom"] != "1" || len(info.Networks) != 1 || info.Networks[0] != netName {
		t.Errorf("created container info: %+v", info)
	}

	// Start (twice), observe, attach a second network (twice), stats.
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatalf("starting a running container must not fail: %v", err)
	}
	if info, _ = m.InspectContainer(ctx, id); !info.Running || info.Status != "running" || info.StartedAt.IsZero() || !info.FinishedAt.IsZero() {
		t.Errorf("running container info: %+v", info)
	}
	for i := 0; i < 2; i++ {
		if err := m.ConnectToNetwork(ctx, netName2, name); err != nil {
			t.Fatalf("ConnectToNetwork #%d: %v", i+1, err)
		}
	}
	if info, _ = m.InspectContainer(ctx, id); len(info.Networks) != 2 {
		t.Errorf("networks after connect: %v", info.Networks)
	}
	st, err := m.ContainerStats(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.MemUsageBytes == 0 || st.MemLimitBytes != 256<<20 || st.CPUPercent < 0 || st.Pids == 0 {
		t.Errorf("implausible stats: %+v", st)
	}

	// CPUUsage is one cumulative reading that does not wait for a second sample,
	// stamped with the daemon's clock; two readings never go backwards.
	began := time.Now()
	u1, err := m.CPUUsage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 800*time.Millisecond {
		t.Errorf("CPUUsage took %v: it must not wait for a previous sample", took)
	}
	time.Sleep(50 * time.Millisecond)
	u2, err := m.CPUUsage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u1.At.IsZero() || !u2.At.After(u1.At) || u2.TotalNanos < u1.TotalNanos {
		t.Errorf("CPUUsage readings %+v then %+v", u1, u2)
	}
	// ManagedContainers finds Berth's container by label, running or not.
	listed, err := m.ManagedContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *ContainerInfo
	for i := range listed {
		if listed[i].ID == id {
			found = &listed[i]
		}
	}
	if found == nil || found.Name != name || !found.Running || found.Status != "running" || found.Labels[LabelUser] != "tester" {
		t.Errorf("ManagedContainers = %+v, want the running container %s", listed, name)
	}

	// Stop (twice) is graceful and idempotent.
	if err := m.StopContainer(ctx, id, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := m.StopContainer(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("stopping a stopped container must not fail: %v", err)
	}
	if info, _ = m.InspectContainer(ctx, id); info.Running || info.Status != "exited" || info.FinishedAt.IsZero() {
		t.Errorf("stopped container info: %+v", info)
	}
	if listed, err = m.ManagedContainers(ctx); err != nil {
		t.Fatal(err)
	}
	found = nil
	for i := range listed {
		if listed[i].ID == id {
			found = &listed[i]
		}
	}
	if found == nil || found.Running || found.Status != "exited" {
		t.Errorf("a stopped container must still be listed: %+v", found)
	}
	// ... and a stopped container can be started again (the "returning user" path).
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatalf("restart: %v", err)
	}

	// Removal is forced, idempotent, and leaves the data volume alone.
	if err := m.RemoveContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveContainer(ctx, id); err != nil {
		t.Fatalf("removing a missing container must not fail: %v", err)
	}
	if _, err := m.InspectContainer(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inspect after remove err = %v, want ErrNotFound", err)
	}
	if _, err := m.c.VolumeInspect(ctx, vol, client.VolumeInspectOptions{}); err != nil {
		t.Fatalf("the data volume must survive container removal: %v", err)
	}
	if err := m.StartContainer(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("start of a removed container err = %v, want ErrNotFound", err)
	}
	if _, err := m.ContainerStats(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("stats of a removed container err = %v, want ErrNotFound", err)
	}
}

func TestMobyPublishesTheUDPRangeIdentically(t *testing.T) {
	m, ctx := newMoby(t)
	name := uniq(t, "udp")
	netName := uniq(t, "net")
	bg := context.Background()
	t.Cleanup(func() {
		m.RemoveContainer(bg, name)
		m.c.NetworkRemove(bg, netName, client.NetworkRemoveOptions{})
	})
	if err := m.EnsureNetwork(ctx, netName, map[string]string{LabelManaged: "true"}); err != nil {
		t.Fatal(err)
	}
	// Pick a high range unlikely to be taken.
	const start, end = 30400, 30403
	id, err := m.CreateContainer(ctx, ContainerSpec{
		Name: name, Image: testImage(), Network: netName, UDPPortStart: start, UDPPortEnd: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := m.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bindings := raw.Container.HostConfig.PortBindings
	if len(bindings) != end-start+1 {
		t.Fatalf("%d port bindings, want %d: %v", len(bindings), end-start+1, bindings)
	}
	for p := start; p <= end; p++ {
		port, _ := network.PortFrom(uint16(p), network.UDP)
		b := bindings[port]
		if len(b) != 1 || b[0].HostPort != strconv.Itoa(p) {
			t.Errorf("udp/%d binding = %+v, want identity mapping", p, b)
		}
	}
}

// Attaching a side network must not change which network is the container's
// default gateway. Docker picks it by network name unless told otherwise, and
// when it flips, the container's published host ports are re-created on the new
// network: a plain `docker network connect` of Traefik to berth-net-<user> reset
// every connection through Traefik (measured: 4 of 400 polled requests failed
// during the connect; with a lower gateway priority none did).
func TestConnectToNetworkKeepsDefaultGateway(t *testing.T) {
	m, ctx := newMoby(t)
	// "anet" sorts before "znet": the case that flips the gateway without a priority.
	home, side := uniq(t, "znet"), uniq(t, "anet")
	name := uniq(t, "ctr")
	t.Cleanup(func() {
		bg := context.Background()
		m.RemoveContainer(bg, name)
		m.c.NetworkRemove(bg, home, client.NetworkRemoveOptions{})
		m.c.NetworkRemove(bg, side, client.NetworkRemoveOptions{})
	})
	labels := map[string]string{LabelManaged: "true"}
	for _, n := range []string{home, side} {
		if err := m.EnsureNetwork(ctx, n, labels); err != nil {
			t.Fatal(err)
		}
	}
	id, err := m.CreateContainer(ctx, ContainerSpec{Name: name, Image: testImage(), Network: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.ConnectToNetwork(ctx, side, name); err != nil {
		t.Fatal(err)
	}
	raw, err := m.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nets := raw.Container.NetworkSettings.Networks
	if ep := nets[side]; ep == nil || ep.GwPriority >= 0 {
		t.Errorf("side network endpoint %+v: its gateway priority must be below the home network's (0)", ep)
	}
	if ep := nets[home]; ep == nil || ep.GwPriority != 0 {
		t.Errorf("home network endpoint %+v: gateway priority must stay 0", ep)
	}
}
