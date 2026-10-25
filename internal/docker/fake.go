package docker

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Fake is an in-memory Client for tests. It models the semantics the real
// daemon has (idempotent ensure/start/stop, name conflicts, volumes outliving
// containers) and lets a test change the world behind the backend's back:
// SetHealth, Crash, Vanish, FailNext.
type Fake struct {
	mu         sync.Mutex
	now        func() time.Time
	seq        int
	networks   map[string]*fakeNetwork
	volumes    map[string]map[string]string
	containers map[string]*fakeContainer // by ID
	failures   map[string][]error        // method name -> queued errors
	calls      []string
	hooks      map[string]func(context.Context)
	afterHooks map[string]func(context.Context)
	stopWait   time.Duration // the timeout of the last StopContainer call
	killed     int           // RemoveContainer calls that hit a running container
	lostCreate []error       // CreateContainer calls that do their work, then fail

	// StartHealth is the health a container reports right after it is started;
	// the zero value means HealthStarting, like an image with a healthcheck.
	StartHealth Health
	// NoHealthcheck makes started containers report HealthNone (an image that
	// defines no healthcheck), overriding StartHealth.
	NoHealthcheck bool
}

type fakeNetwork struct {
	labels  map[string]string
	members map[string]bool // container IDs
}

type fakeContainer struct {
	info  ContainerInfo
	spec  ContainerSpec
	stats Stats

	// CPU accounting: cpuNanos grows by cpuCores x elapsed fake time, up to cpuAt.
	cpuNanos float64
	cpuCores float64
	cpuAt    time.Time
}

var _ Client = (*Fake)(nil)

// NewFake returns an empty fake. now supplies timestamps (time.Now if nil).
func NewFake(now func() time.Time) *Fake {
	if now == nil {
		now = time.Now
	}
	return &Fake{
		now:        now,
		networks:   map[string]*fakeNetwork{},
		volumes:    map[string]map[string]string{},
		containers: map[string]*fakeContainer{},
		failures:   map[string][]error{},
		hooks:      map[string]func(context.Context){},
		afterHooks: map[string]func(context.Context){},
	}
}

// SetAfterHook runs fn after every call to the named method has finished and its
// result is fixed, outside the fake's lock: a test can hold a caller that already
// holds an answer which then goes stale. Only InspectContainer calls it.
func (f *Fake) SetAfterHook(method string, fn func(ctx context.Context)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fn == nil {
		delete(f.afterHooks, method)
		return
	}
	f.afterHooks[method] = fn
}

func (f *Fake) runAfterHook(ctx context.Context, method string) {
	f.mu.Lock()
	fn := f.afterHooks[method]
	f.mu.Unlock()
	if fn != nil {
		fn(ctx)
	}
}

// SetHook runs fn at the start of every call to the named Client method, before
// the fake's lock is taken, so a test can block a call midway (to interleave
// another request with it) or cancel a context. nil removes the hook.
func (f *Fake) SetHook(method string, fn func(ctx context.Context)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fn == nil {
		delete(f.hooks, method)
		return
	}
	f.hooks[method] = fn
}

func (f *Fake) runHook(ctx context.Context, method string) {
	f.mu.Lock()
	fn := f.hooks[method]
	f.mu.Unlock()
	if fn != nil {
		fn(ctx)
	}
}

// LastStopTimeout is the grace period the most recent StopContainer call asked for.
func (f *Fake) LastStopTimeout() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopWait
}

// LoseNextCreateResponse makes the next CreateContainer create the container
// and then return err, like a connection that drops after the daemon did the
// work: the caller never learns the container ID.
func (f *Fake) LoseNextCreateResponse(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lostCreate = append(f.lostCreate, err)
}

func (f *Fake) record(call string) error {
	f.calls = append(f.calls, call)
	method := call
	for i := 0; i < len(call); i++ {
		if call[i] == ' ' {
			method = call[:i]
			break
		}
	}
	if q := f.failures[method]; len(q) > 0 {
		f.failures[method] = q[1:]
		return q[0]
	}
	return nil
}

// FailNext makes the next call to the named Client method (e.g. "StartContainer")
// return err instead of doing anything. Calls queue up.
func (f *Fake) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = append(f.failures[method], err)
}

// Calls returns every Client call made so far, as "Method arg arg" strings.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// CallCount counts calls whose text starts with prefix.
func (f *Fake) CallCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// AddContainer registers a running, healthy container that Berth did not create
// (the Traefik container, in tests).
func (f *Fake) AddContainer(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("fake%04d", f.seq)
	f.containers[id] = &fakeContainer{info: ContainerInfo{
		ID: id, Name: name, Status: "running", Running: true, Health: HealthHealthy,
		StartedAt: f.now(), Labels: map[string]string{},
	}}
	return id
}

// SetHealth changes a running container's reported health.
func (f *Fake) SetHealth(id string, h Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		c.info.Health = h
	}
}

// Crash makes a container exit on its own with the given exit code.
func (f *Fake) Crash(id string, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		c.exit(f.now(), exitCode)
	}
}

// Vanish deletes a container as if someone ran `docker rm -f` by hand.
func (f *Fake) Vanish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeLocked(id)
}

// SetStats sets what ContainerStats reports for a container.
func (f *Fake) SetStats(id string, s Stats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		c.stats = s
	}
}

// SetCPUCores makes a container burn that many cores from now on (1 = one core
// fully busy, 0.05 = 5% of one core). CPUUsage integrates it over the fake clock.
func (f *Fake) SetCPUCores(id string, cores float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[id]; c != nil {
		c.advanceCPU(f.now())
		c.cpuCores = cores
	}
}

// advanceCPU accounts the CPU time used up to now at the current rate.
func (c *fakeContainer) advanceCPU(now time.Time) {
	if !c.cpuAt.IsZero() && now.After(c.cpuAt) && c.info.Running {
		c.cpuNanos += c.cpuCores * float64(now.Sub(c.cpuAt))
	}
	c.cpuAt = now
}

// Spec returns the ContainerSpec a container was created with.
func (f *Fake) Spec(id string) (ContainerSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return ContainerSpec{}, false
	}
	return c.spec, true
}

// ContainerIDByName finds a container's ID, or "" if there is none.
func (f *Fake) ContainerIDByName(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idByNameLocked(name)
}

// Containers returns the names of all containers, sorted.
func (f *Fake) Containers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, c := range f.containers {
		names = append(names, c.info.Name)
	}
	sort.Strings(names)
	return names
}

// HasVolume reports whether a named volume exists.
func (f *Fake) HasVolume(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.volumes[name]
	return ok
}

// NetworkMembers returns the names of the containers attached to a network.
func (f *Fake) NetworkMembers(network string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.networks[network]
	if n == nil {
		return nil
	}
	var out []string
	for id := range n.members {
		if c := f.containers[id]; c != nil {
			out = append(out, c.info.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (c *fakeContainer) exit(at time.Time, code int) {
	c.advanceCPU(at)
	c.info.Running = false
	c.info.Status = "exited"
	c.info.ExitCode = code
	c.info.FinishedAt = at
	// Health is deliberately left as it was: the real daemon keeps reporting the
	// last healthcheck status of a container that has exited.
}

func (f *Fake) idByNameLocked(name string) string {
	for id, c := range f.containers {
		if c.info.Name == name {
			return id
		}
	}
	return ""
}

func (f *Fake) resolveLocked(idOrName string) *fakeContainer {
	if c := f.containers[idOrName]; c != nil {
		return c
	}
	if id := f.idByNameLocked(idOrName); id != "" {
		return f.containers[id]
	}
	return nil
}

func (f *Fake) removeLocked(id string) {
	delete(f.containers, id)
	for _, n := range f.networks {
		delete(n.members, id)
	}
}

func (f *Fake) Ping(ctx context.Context) error {
	f.runHook(ctx, "Ping")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record("Ping")
}

func (f *Fake) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	f.runHook(ctx, "EnsureNetwork")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("EnsureNetwork " + name); err != nil {
		return err
	}
	if f.networks[name] == nil {
		f.networks[name] = &fakeNetwork{labels: copyLabels(labels), members: map[string]bool{}}
	}
	return nil
}

func (f *Fake) ManagedNetworks(ctx context.Context) ([]string, error) {
	f.runHook(ctx, "ManagedNetworks")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ManagedNetworks"); err != nil {
		return nil, err
	}
	var names []string
	for name, n := range f.networks {
		if n.labels[LabelManaged] == "true" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (f *Fake) ConnectToNetwork(ctx context.Context, network, container string) error {
	f.runHook(ctx, "ConnectToNetwork")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ConnectToNetwork " + network + " " + container); err != nil {
		return err
	}
	c := f.resolveLocked(container)
	if c == nil {
		return fmt.Errorf("%w: container %s", ErrNotFound, container)
	}
	n := f.networks[network]
	if n == nil {
		return fmt.Errorf("%w: network %s", ErrNotFound, network)
	}
	n.members[c.info.ID] = true
	return nil
}

func (f *Fake) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	f.runHook(ctx, "EnsureVolume")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("EnsureVolume " + name); err != nil {
		return err
	}
	if _, ok := f.volumes[name]; !ok {
		f.volumes[name] = copyLabels(labels)
	}
	return nil
}

func (f *Fake) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	f.runHook(ctx, "CreateContainer")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CreateContainer " + spec.Name); err != nil {
		return "", err
	}
	if f.idByNameLocked(spec.Name) != "" {
		return "", fmt.Errorf("%w: %s", ErrNameInUse, spec.Name)
	}
	f.seq++
	id := fmt.Sprintf("fake%04d", f.seq)
	c := &fakeContainer{
		spec: spec,
		info: ContainerInfo{ID: id, Name: spec.Name, Status: "created", Labels: copyLabels(spec.Labels)},
	}
	if spec.Network != "" {
		n := f.networks[spec.Network]
		if n == nil {
			return "", fmt.Errorf("%w: network %s", ErrNotFound, spec.Network)
		}
		n.members[id] = true
	}
	f.containers[id] = c
	if len(f.lostCreate) > 0 {
		err := f.lostCreate[0]
		f.lostCreate = f.lostCreate[1:]
		return "", err
	}
	return id, nil
}

func (f *Fake) StartContainer(ctx context.Context, id string) error {
	f.runHook(ctx, "StartContainer")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("StartContainer " + id); err != nil {
		return err
	}
	c := f.containers[id]
	if c == nil {
		return fmt.Errorf("%w: container %s", ErrNotFound, id)
	}
	if c.info.Running {
		return nil
	}
	c.info.Running = true
	c.info.Status = "running"
	c.cpuNanos, c.cpuAt = 0, f.now() // the daemon's counters start again with the container
	c.info.StartedAt = f.now()
	c.info.FinishedAt = time.Time{}
	c.info.ExitCode = 0
	switch {
	case f.NoHealthcheck:
		c.info.Health = HealthNone
	case f.StartHealth != "":
		c.info.Health = f.StartHealth
	default:
		c.info.Health = HealthStarting
	}
	return nil
}

func (f *Fake) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	f.runHook(ctx, "StopContainer")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("StopContainer " + id); err != nil {
		return err
	}
	f.stopWait = timeout
	c := f.containers[id]
	if c == nil {
		return fmt.Errorf("%w: container %s", ErrNotFound, id)
	}
	if c.info.Running {
		c.exit(f.now(), 0)
	}
	return nil
}

func (f *Fake) RemoveContainer(ctx context.Context, id string) error {
	f.runHook(ctx, "RemoveContainer")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("RemoveContainer " + id); err != nil {
		return err
	}
	if c := f.containers[id]; c != nil && c.info.Running {
		f.killed++
	}
	f.removeLocked(id)
	return nil
}

// RemovedWhileRunning counts the containers that were force-removed while they
// were running: a test that must never see one asserts it stays zero.
func (f *Fake) RemovedWhileRunning() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

func (f *Fake) InspectContainer(ctx context.Context, idOrName string) (ContainerInfo, error) {
	f.runHook(ctx, "InspectContainer")
	defer f.runAfterHook(ctx, "InspectContainer") // deferred first: runs after the lock is released
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("InspectContainer " + idOrName); err != nil {
		return ContainerInfo{}, err
	}
	c := f.resolveLocked(idOrName)
	if c == nil {
		return ContainerInfo{}, fmt.Errorf("%w: container %s", ErrNotFound, idOrName)
	}
	info := c.info
	info.Labels = copyLabels(c.info.Labels)
	info.Networks = nil
	for name, n := range f.networks {
		if n.members[c.info.ID] {
			info.Networks = append(info.Networks, name)
		}
	}
	sort.Strings(info.Networks)
	return info, nil
}

func (f *Fake) ContainerStats(ctx context.Context, id string) (Stats, error) {
	f.runHook(ctx, "ContainerStats")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ContainerStats " + id); err != nil {
		return Stats{}, err
	}
	c := f.containers[id]
	if c == nil {
		return Stats{}, fmt.Errorf("%w: container %s", ErrNotFound, id)
	}
	return c.stats, nil
}

func (f *Fake) CPUUsage(ctx context.Context, id string) (CPUUsage, error) {
	f.runHook(ctx, "CPUUsage")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CPUUsage " + id); err != nil {
		return CPUUsage{}, err
	}
	c := f.containers[id]
	if c == nil {
		return CPUUsage{}, fmt.Errorf("%w: container %s", ErrNotFound, id)
	}
	now := f.now()
	c.advanceCPU(now)
	return CPUUsage{TotalNanos: uint64(c.cpuNanos), At: now}, nil
}

func (f *Fake) ManagedContainers(ctx context.Context) ([]ContainerInfo, error) {
	f.runHook(ctx, "ManagedContainers")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ManagedContainers"); err != nil {
		return nil, err
	}
	var out []ContainerInfo
	for _, c := range f.containers {
		if c.info.Labels[LabelManaged] != "true" {
			continue
		}
		info := c.info
		info.Labels = copyLabels(c.info.Labels)
		info.Networks = nil
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
