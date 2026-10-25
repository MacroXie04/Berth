package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run the Moby client against a stub of the Docker HTTP API, so the
// code that acts on the daemon's replies is exercised without a daemon.

var apiVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

type stubDaemon struct {
	mu       sync.Mutex
	reqs     []string
	bodies   map[string]string
	handlers map[string]http.HandlerFunc
}

func newStubDaemon(t *testing.T) (*stubDaemon, *Moby) {
	t.Helper()
	d := &stubDaemon{handlers: map[string]http.HandlerFunc{}, bodies: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.reqs = append(d.reqs, key)
		d.bodies[key] = string(body)
		h := d.handlers[key]
		d.mu.Unlock()
		w.Header().Set("API-Version", "1.51")
		w.Header().Set("Content-Type", "application/json")
		if h == nil {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no stub for `+key+`"}`)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	m, err := NewMoby()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	d.handlers["HEAD /_ping"] = reply(200, "")
	d.handlers["GET /_ping"] = reply(200, "OK")
	return d, m
}

func (d *stubDaemon) saw(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, k := range d.reqs {
		if k == key {
			return true
		}
	}
	return false
}

func (d *stubDaemon) body(key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bodies[key]
}

func reply(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

func stubCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The daemon creates a container it cannot limit and says so only in a warning.
func TestCreateRefusesAContainerWhoseLimitsWereDiscarded(t *testing.T) {
	d, m := newStubDaemon(t)
	d.handlers["POST /containers/create"] = reply(201, `{"Id":"abc","Warnings":["Your kernel does not support memory limit capabilities or the cgroup is not mounted. Limitation discarded."]}`)
	d.handlers["DELETE /containers/abc"] = reply(204, "")
	id, err := m.CreateContainer(stubCtx(t), ContainerSpec{Name: "c", Image: "i", MemoryBytes: 1 << 30})
	if !errors.Is(err, ErrLimitsNotEnforced) || id != "" {
		t.Fatalf("id=%q err=%v, want ErrLimitsNotEnforced", id, err)
	}
	if !d.saw("DELETE /containers/abc") {
		t.Fatal("the unlimited container was not removed")
	}
}

func TestCreateAcceptsAMemoryWithoutSwapWarning(t *testing.T) {
	d, m := newStubDaemon(t)
	d.handlers["POST /containers/create"] = reply(201, `{"Id":"abc","Warnings":["Your kernel does not support swap limit capabilities or the cgroup is not mounted. Memory limited without swap."]}`)
	id, err := m.CreateContainer(stubCtx(t), ContainerSpec{Name: "c", Image: "i"})
	if err != nil || id != "abc" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if d.saw("DELETE /containers/abc") {
		t.Fatal("a container whose limits hold was removed")
	}
}

// The UDP block of a spec is published port for port, and nothing else is: one
// port (the default) is one binding, a block of eight is eight, no block is none.
func TestCreatePublishesExactlyTheSpecifiedUDPPorts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end int
		want       []string
	}{
		{"one port", 30005, 30005, []string{"30005/udp"}},
		{"a block of three", 30010, 30012, []string{"30010/udp", "30011/udp", "30012/udp"}},
		{"no block", 0, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, m := newStubDaemon(t)
			d.handlers["POST /containers/create"] = reply(201, `{"Id":"abc","Warnings":[]}`)
			if _, err := m.CreateContainer(stubCtx(t), ContainerSpec{Name: "c", Image: "i", UDPPortStart: tc.start, UDPPortEnd: tc.end}); err != nil {
				t.Fatal(err)
			}
			var body struct {
				ExposedPorts map[string]struct{}
				HostConfig   struct {
					PortBindings map[string][]struct{ HostPort string }
				}
			}
			raw := d.body("POST /containers/create")
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			if len(body.ExposedPorts) != len(tc.want) || len(body.HostConfig.PortBindings) != len(tc.want) {
				t.Fatalf("exposed %v, bound %v; want exactly %v\n%s", body.ExposedPorts, body.HostConfig.PortBindings, tc.want, raw)
			}
			for _, port := range tc.want {
				b := body.HostConfig.PortBindings[port]
				// Published 1:1, so the host port is the container port.
				if _, ok := body.ExposedPorts[port]; !ok || len(b) != 1 || b[0].HostPort+"/udp" != port {
					t.Errorf("port %s: exposed=%v bindings=%v", port, body.ExposedPorts, b)
				}
			}
		})
	}
}

func TestEnsureNetworkReportsExhaustedAddressSpace(t *testing.T) {
	d, m := newStubDaemon(t)
	d.handlers["GET /networks/n"] = reply(404, `{"message":"network n not found"}`)
	d.handlers["POST /networks/create"] = reply(400, `{"message":"all predefined address pools have been fully subnetted"}`)
	if err := m.EnsureNetwork(stubCtx(t), "n", nil); !errors.Is(err, ErrNoAddressSpace) {
		t.Fatalf("err = %v, want ErrNoAddressSpace", err)
	}
	d.handlers["POST /networks/create"] = reply(400, `{"message":"invalid network name"}`)
	if err := m.EnsureNetwork(stubCtx(t), "n", nil); err == nil || errors.Is(err, ErrNoAddressSpace) {
		t.Fatalf("an unrelated refusal was reported as exhausted address space: %v", err)
	}
}

// A 404 on start means "gone" only if the container is really gone.
func TestStartTellsAMissingContainerFromAStaleOne(t *testing.T) {
	d, m := newStubDaemon(t)
	d.handlers["POST /containers/stale/start"] = reply(404, `{"message":"network abc not found"}`)
	d.handlers["GET /containers/stale/json"] = reply(200, `{"Id":"stale","Name":"/c","State":{"Status":"exited"},"Config":{},"HostConfig":{},"NetworkSettings":{}}`)
	d.handlers["POST /containers/gone/start"] = reply(404, `{"message":"No such container: gone"}`)
	d.handlers["GET /containers/gone/json"] = reply(404, `{"message":"No such container: gone"}`)
	if err := m.StartContainer(stubCtx(t), "stale"); !errors.Is(err, ErrStaleContainer) || errors.Is(err, ErrNotFound) {
		t.Fatalf("stale: %v", err)
	}
	if err := m.StartContainer(stubCtx(t), "gone"); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrStaleContainer) {
		t.Fatalf("gone: %v", err)
	}
	// An already running container is success, not an error.
	d.handlers["POST /containers/up/start"] = reply(304, "")
	if err := m.StartContainer(stubCtx(t), "up"); err != nil {
		t.Fatalf("already running: %v", err)
	}
}

// The attachment must never become the container's default gateway.
func TestConnectUsesALowGatewayPriority(t *testing.T) {
	d, m := newStubDaemon(t)
	d.handlers["GET /containers/tr/json"] = reply(200, `{"Id":"trid","Name":"/tr","State":{"Status":"running"},"Config":{},"HostConfig":{},"NetworkSettings":{}}`)
	d.handlers["GET /networks/n"] = reply(200, `{"Name":"n","Id":"nid","Containers":{}}`)
	d.handlers["POST /networks/n/connect"] = reply(200, `{}`)
	if err := m.ConnectToNetwork(stubCtx(t), "n", "tr"); err != nil {
		t.Fatal(err)
	}
	var body struct {
		EndpointConfig struct{ GwPriority int }
	}
	raw := d.body("POST /networks/n/connect")
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if body.EndpointConfig.GwPriority >= 0 {
		t.Fatalf("connect body %s: the gateway priority must be negative", raw)
	}
}

func TestAPIAtLeast(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"1.48", true}, {"1.56", true}, {"1.100", true}, {"2.0", true},
		{"1.47", false}, {"1.43", false}, {"1.9", false}, {"", false}, {"garbage", false}, {"1", false},
	} {
		if got := apiAtLeast(tc.v, 1, 48); got != tc.want {
			t.Errorf("apiAtLeast(%q, 1, 48) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

// The daemon's API version is negotiated down to what an old Engine speaks; the
// gateway priority only exists from API 1.48 (Engine 28).
func TestGatewayPrioritySupportFollowsTheNegotiatedVersion(t *testing.T) {
	for version, want := range map[string]bool{"1.51": true, "1.48": true, "1.47": false, "1.43": false} {
		t.Run(version, func(t *testing.T) {
			d, m := newStubDaemon(t)
			d.handlers["HEAD /_ping"] = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("API-Version", version)
				w.WriteHeader(200)
			}
			if err := m.Ping(stubCtx(t)); err != nil {
				t.Fatal(err)
			}
			if got := m.SupportsGatewayPriority(); got != want {
				t.Fatalf("SupportsGatewayPriority() = %v with API %s, want %v", got, version, want)
			}
		})
	}
}

// CPUUsage takes one reading without the daemon's second-long wait for a previous
// sample, and reports the daemon's own timestamp.
func TestCPUUsageIsASingleSampleWithTheDaemonsClock(t *testing.T) {
	d, m := newStubDaemon(t)
	var query string
	d.handlers["GET /containers/c1/stats"] = func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		io.WriteString(w, `{"read":"2026-01-02T03:04:05.5Z","cpu_stats":{"cpu_usage":{"total_usage":123456789},"online_cpus":2}}`)
	}
	d.handlers["GET /containers/none/stats"] = reply(404, `{"message":"No such container: none"}`)

	got, err := m.CPUUsage(stubCtx(t), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalNanos != 123456789 || !got.At.Equal(time.Date(2026, 1, 2, 3, 4, 5, 500_000_000, time.UTC)) {
		t.Fatalf("CPUUsage = %+v", got)
	}
	if !strings.Contains(query, "one-shot=true") || !strings.Contains(query, "stream=false") {
		t.Fatalf("query %q: want a one-shot, non-streaming request (a previous sample costs the daemon a second)", query)
	}
	if _, err := m.CPUUsage(stubCtx(t), "none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing container: %v", err)
	}
}

// ManagedContainers asks the daemon for Berth's own containers only, running or not.
func TestManagedContainersFiltersByLabelAndIncludesStoppedOnes(t *testing.T) {
	d, m := newStubDaemon(t)
	var query string
	d.handlers["GET /containers/json"] = func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		io.WriteString(w, `[
			{"Id":"b","Names":["/berth-ws-bob"],"State":"exited","Labels":{"berth.managed":"true","berth.user":"bob"}},
			{"Id":"a","Names":["/berth-ws-alice"],"State":"running","Labels":{"berth.managed":"true","berth.user":"alice"}}]`)
	}
	got, err := m.ManagedContainers(stubCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "all=1") && !strings.Contains(query, "all=true") {
		t.Errorf("query %q lacks all=: stopped containers would be missed", query)
	}
	if !strings.Contains(query, "berth.managed%3Dtrue") && !strings.Contains(query, "berth.managed=true") {
		t.Errorf("query %q lacks the berth.managed label filter", query)
	}
	if len(got) != 2 || got[0].Name != "berth-ws-alice" || got[1].Name != "berth-ws-bob" {
		t.Fatalf("containers = %+v, want alice then bob sorted by name without the leading slash", got)
	}
	if !got[0].Running || got[0].Status != "running" || got[0].Labels["berth.user"] != "alice" || got[0].ID != "a" {
		t.Errorf("alice = %+v", got[0])
	}
	if got[1].Running || got[1].Status != "exited" {
		t.Errorf("bob = %+v", got[1])
	}
}

// Removing a workspace's container is a force-remove (a stuck container must go)
// that never takes its volume: the user's files outlive their container.
func TestRemoveContainerForcesButNeverRemovesVolumes(t *testing.T) {
	d, m := newStubDaemon(t)
	var query string
	d.handlers["DELETE /containers/c1"] = func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(204)
	}
	if err := m.RemoveContainer(stubCtx(t), "c1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "force=1") && !strings.Contains(query, "force=true") {
		t.Errorf("query %q: the removal must be forced", query)
	}
	for _, bad := range []string{"v=1", "v=true"} {
		if strings.Contains(query, bad) {
			t.Errorf("query %q asks the daemon to remove the container's volumes", query)
		}
	}
	// A container that is already gone is not an error.
	d.handlers["DELETE /containers/gone"] = reply(404, `{"message":"No such container: gone"}`)
	if err := m.RemoveContainer(stubCtx(t), "gone"); err != nil {
		t.Errorf("removing a missing container: %v", err)
	}
}
