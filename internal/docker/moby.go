package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Moby implements Client on the official Docker SDK. It honours DOCKER_HOST and
// the other standard DOCKER_* variables and negotiates the API version, so it
// works across Engine versions.
type Moby struct {
	c *client.Client
}

var _ Client = (*Moby)(nil)

// NewMoby returns a client configured from the environment. It does not contact
// the daemon; call Ping for that.
func NewMoby() (*Moby, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker: new client: %w", err)
	}
	return &Moby{c: c}, nil
}

func (m *Moby) Close() error { return m.c.Close() }

// IsUnavailable reports whether err (possibly wrapped) means the daemon could
// not be reached, as opposed to the daemon refusing a request.
func IsUnavailable(err error) bool {
	return errors.Is(err, ErrUnavailable) || client.IsErrConnectionFailed(err)
}

func (m *Moby) Ping(ctx context.Context) error {
	if _, err := m.c.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return fmt.Errorf("docker: ping: %w", err)
	}
	return nil
}

// SupportsGatewayPriority reports whether the daemon, as negotiated at Ping,
// honours the endpoint gateway priority that ConnectToNetwork sends (Docker
// Engine 28, API 1.48). An older engine silently ignores it: Traefik's default
// gateway then flips to the new user network, and the first power-on of every
// new user resets the connections through Traefik. Call it after a successful
// Ping; before that the client has not negotiated a version yet.
func (m *Moby) SupportsGatewayPriority() bool {
	return apiAtLeast(m.c.ClientVersion(), 1, 48)
}

// apiAtLeast compares a "major.minor" Docker API version.
func apiAtLeast(v string, major, minor int) bool {
	var maj, min int
	if n, _ := fmt.Sscanf(v, "%d.%d", &maj, &min); n != 2 {
		return false
	}
	return maj > major || (maj == major && min >= minor)
}

func (m *Moby) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	_, err := m.c.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("docker: inspect network %s: %w", name, err)
	}
	_, err = m.c.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: labels})
	// Two requests can race to create the same user's network; the loser's
	// conflict means it now exists, which is all we wanted.
	if err != nil && !cerrdefs.IsConflict(err) && !cerrdefs.IsAlreadyExists(err) {
		if addressSpaceExhausted(err) {
			return fmt.Errorf("%w: %v", ErrNoAddressSpace, err)
		}
		return fmt.Errorf("docker: create network %s: %w", name, err)
	}
	return nil
}

// addressSpaceExhausted recognises the daemon's "no subnet left" refusal. The
// daemon reports it as a plain invalid-parameter error, so the text is all
// there is to go on; the phrases are the two it uses (fixed pools, then the
// overlap search).
func addressSpaceExhausted(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "fully subnetted") ||
		strings.Contains(msg, "could not find an available, non-overlapping IPv4 address pool")
}

func (m *Moby) ManagedNetworks(ctx context.Context) ([]string, error) {
	res, err := m.c.NetworkList(ctx, client.NetworkListOptions{
		Filters: client.Filters{}.Add("label", LabelManaged+"=true"),
	})
	if err != nil {
		return nil, fmt.Errorf("docker: list networks: %w", err)
	}
	names := make([]string, 0, len(res.Items))
	for _, n := range res.Items {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (m *Moby) ConnectToNetwork(ctx context.Context, networkName, containerName string) error {
	ci, err := m.c.ContainerInspect(ctx, containerName, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("%w: container %s", ErrNotFound, containerName)
		}
		return fmt.Errorf("docker: inspect container %s: %w", containerName, err)
	}
	id := ci.Container.ID

	attached := func() (bool, error) {
		ni, err := m.c.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return false, fmt.Errorf("%w: network %s", ErrNotFound, networkName)
			}
			return false, fmt.Errorf("docker: inspect network %s: %w", networkName, err)
		}
		_, ok := ni.Network.Containers[id]
		return ok, nil
	}
	if ok, err := attached(); err != nil || ok {
		return err
	}
	// GwPriority -1: this is a side attachment, so it must never become the
	// container's default gateway. By default Docker picks the gateway by network
	// name, and Traefik joining berth-net-<user> (which sorts before berth-proxy)
	// would move its published host port to that network: every connection through
	// Traefik, including the request that triggered the attach, is reset.
	opts := client.NetworkConnectOptions{
		Container:      id,
		EndpointConfig: &network.EndpointSettings{GwPriority: -1},
	}
	if _, err := m.c.NetworkConnect(ctx, networkName, opts); err != nil {
		// Lost a race with another attach? Then it is fine.
		if ok, aerr := attached(); aerr == nil && ok {
			return nil
		}
		return fmt.Errorf("docker: connect %s to %s: %w", containerName, networkName, err)
	}
	return nil
}

func (m *Moby) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := m.c.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("docker: inspect volume %s: %w", name, err)
	}
	// VolumeCreate is itself idempotent for an existing name with the same driver.
	if _, err := m.c.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		return fmt.Errorf("docker: create volume %s: %w", name, err)
	}
	return nil
}

func (m *Moby) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	cfg := &container.Config{
		Hostname: spec.Hostname,
		Image:    spec.Image,
		Env:      spec.Env,
		Labels:   spec.Labels,
	}
	if spec.StopTimeout > 0 {
		secs := int(spec.StopTimeout / time.Second)
		cfg.StopTimeout = &secs
	}

	host := &container.HostConfig{
		NetworkMode:   container.NetworkMode(spec.Network),
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		CapDrop:       spec.CapDrop,
		SecurityOpt:   spec.SecurityOpt,
		ShmSize:       spec.ShmSizeBytes,
		Resources: container.Resources{
			Memory:   spec.MemoryBytes,
			NanoCPUs: spec.NanoCPUs,
		},
	}
	if spec.PidsLimit > 0 {
		limit := spec.PidsLimit
		host.PidsLimit = &limit
	}
	if spec.Init {
		init := true
		host.Init = &init
	}
	for _, v := range spec.Volumes {
		host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeVolume, Source: v.Volume, Target: v.Target})
	}
	if spec.UDPPortStart > 0 {
		cfg.ExposedPorts = network.PortSet{}
		host.PortBindings = network.PortMap{}
		for p := spec.UDPPortStart; p <= spec.UDPPortEnd; p++ {
			port, ok := network.PortFrom(uint16(p), network.UDP)
			if !ok {
				return "", fmt.Errorf("docker: invalid UDP port %d", p)
			}
			cfg.ExposedPorts[port] = struct{}{}
			host.PortBindings[port] = []network.PortBinding{{HostPort: fmt.Sprint(p)}}
		}
	}

	res, err := m.c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       spec.Name,
		Config:     cfg,
		HostConfig: host,
	})
	if err != nil {
		if cerrdefs.IsConflict(err) {
			return "", fmt.Errorf("%w: %s", ErrNameInUse, spec.Name)
		}
		return "", fmt.Errorf("docker: create container %s: %w", spec.Name, err)
	}
	// A host whose kernel lacks the memory or PIDs controller still gets a 201:
	// the daemon drops the limit and says so only in the warnings. Berth's limits
	// are a security invariant, so do not run a container without them.
	for _, w := range res.Warnings {
		slog.Warn("docker: container created with warning", "container", spec.Name, "warning", w)
	}
	if limitsDiscarded(res.Warnings) {
		if _, rerr := m.c.ContainerRemove(context.WithoutCancel(ctx), res.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !cerrdefs.IsNotFound(rerr) {
			slog.Error("docker: remove container with discarded limits", "container", spec.Name, "err", rerr)
		}
		return "", fmt.Errorf("%w: %s", ErrLimitsNotEnforced, strings.Join(res.Warnings, "; "))
	}
	return res.ID, nil
}

// limitsDiscarded reports whether the daemon said it dropped a limit. "Memory
// limited without swap" is not one of these: the limit holds, swap is merely
// uncapped, and many ARM boards lack swap accounting.
func limitsDiscarded(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, "discarded") {
			return true
		}
	}
	return false
}

func (m *Moby) StartContainer(ctx context.Context, id string) error {
	if _, err := m.c.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		if cerrdefs.IsNotModified(err) {
			return nil // already running
		}
		if cerrdefs.IsNotFound(err) {
			// The daemon also answers 404 when the container exists but a network
			// it references does not (removed and recreated with a new ID while the
			// workspace was stopped). Only a failed lookup means the container is gone.
			if _, ierr := m.c.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); ierr == nil {
				return fmt.Errorf("%w: start %s: %v", ErrStaleContainer, id, err)
			} else if !cerrdefs.IsNotFound(ierr) {
				return fmt.Errorf("docker: inspect %s after failed start: %w", id, ierr)
			}
			return fmt.Errorf("%w: container %s", ErrNotFound, id)
		}
		return fmt.Errorf("docker: start %s: %w", id, err)
	}
	return nil
}

func (m *Moby) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout / time.Second)
	if _, err := m.c.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &secs}); err != nil {
		if cerrdefs.IsNotModified(err) {
			return nil // already stopped
		}
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("%w: container %s", ErrNotFound, id)
		}
		return fmt.Errorf("docker: stop %s: %w", id, err)
	}
	return nil
}

func (m *Moby) RemoveContainer(ctx context.Context, id string) error {
	// RemoveVolumes stays false: a workspace's data volume must outlive its container.
	_, err := m.c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("docker: remove %s: %w", id, err)
	}
	return nil
}

func (m *Moby) InspectContainer(ctx context.Context, idOrName string) (ContainerInfo, error) {
	res, err := m.c.ContainerInspect(ctx, idOrName, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return ContainerInfo{}, fmt.Errorf("%w: container %s", ErrNotFound, idOrName)
		}
		return ContainerInfo{}, fmt.Errorf("docker: inspect %s: %w", idOrName, err)
	}
	c := res.Container
	info := ContainerInfo{ID: c.ID, Name: trimSlash(c.Name)}
	if c.Config != nil {
		info.Labels = c.Config.Labels
	}
	if st := c.State; st != nil {
		info.Status = string(st.Status)
		info.Running = st.Running
		info.ExitCode = st.ExitCode
		info.OOMKilled = st.OOMKilled
		info.StartedAt = parseDockerTime(st.StartedAt)
		info.FinishedAt = parseDockerTime(st.FinishedAt)
		if st.Health != nil {
			switch string(st.Health.Status) {
			case "healthy":
				info.Health = HealthHealthy
			case "unhealthy":
				info.Health = HealthUnhealthy
			case "starting":
				info.Health = HealthStarting
			}
		}
	}
	if c.NetworkSettings != nil {
		for name := range c.NetworkSettings.Networks {
			info.Networks = append(info.Networks, name)
		}
		sort.Strings(info.Networks)
	}
	return info, nil
}

func (m *Moby) ContainerStats(ctx context.Context, id string) (Stats, error) {
	res, err := m.c.ContainerStats(ctx, id, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return Stats{}, fmt.Errorf("%w: container %s", ErrNotFound, id)
		}
		return Stats{}, fmt.Errorf("docker: stats %s: %w", id, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Stats{}, fmt.Errorf("docker: read stats %s: %w", id, err)
	}
	var s container.StatsResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return Stats{}, fmt.Errorf("docker: decode stats %s: %w", id, err)
	}
	return statsFrom(s), nil
}

// statsFrom converts the daemon's raw sample, following the formulas of
// `docker stats`: CPU from the delta against the previous sample, memory
// without the reclaimable page cache.
func statsFrom(s container.StatsResponse) Stats {
	out := Stats{MemLimitBytes: s.MemoryStats.Limit, Pids: s.PidsStats.Current}

	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if sysDelta > 0 && cpuDelta > 0 && cpus > 0 {
		out.CPUPercent = cpuDelta / sysDelta * cpus * 100
	}

	used := s.MemoryStats.Usage
	for _, key := range []string{"inactive_file", "total_inactive_file"} { // cgroup v2, v1
		if v, ok := s.MemoryStats.Stats[key]; ok {
			if v < used {
				used -= v
			}
			break
		}
	}
	out.MemUsageBytes = used
	return out
}

func trimSlash(name string) string {
	if len(name) > 0 && name[0] == '/' {
		return name[1:]
	}
	return name
}

// parseDockerTime parses Docker's RFC 3339 timestamps; the zero time Docker
// reports for "never" (0001-01-01) becomes the zero time.Time.
func parseDockerTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}
