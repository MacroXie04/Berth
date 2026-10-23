// Package docker is a thin, mockable wrapper over the Docker Engine API. The
// rest of the backend depends only on the Client interface and the plain types
// here, never on the SDK, so container management can be tested with Fake.
package docker

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound means the container (or network) does not exist.
	ErrNotFound = errors.New("docker: not found")
	// ErrNameInUse means a container with that name already exists.
	ErrNameInUse = errors.New("docker: container name already in use")
	// ErrUnavailable means the Docker daemon could not be reached. Callers
	// should test with IsUnavailable, which also recognises the SDK's own error.
	ErrUnavailable = errors.New("docker: daemon unavailable")
	// ErrNoAddressSpace means Docker has no subnet left for another network (its
	// default pools hold about 30). It is a capacity problem, not a bug.
	ErrNoAddressSpace = errors.New("docker: no address space for another network")
	// ErrStaleContainer means a container exists but cannot be started because
	// something it references is gone (typically its network was removed and
	// recreated). Recreating the container fixes it; the data is on the volume.
	ErrStaleContainer = errors.New("docker: container references objects that no longer exist")
	// ErrLimitsNotEnforced means the daemon created a container but silently
	// dropped a resource limit (memory or PIDs) the host cannot enforce. Berth
	// treats its limits as a security invariant, so it refuses such a container.
	ErrLimitsNotEnforced = errors.New("docker: the daemon cannot enforce the requested resource limits")
)

// Labels Berth puts on everything it creates, so it can find its own objects
// again and never touches anyone else's.
const (
	LabelManaged = "berth.managed" // "true" on every network and container Berth creates
	LabelUser    = "berth.user"    // the owning username
)

// Health is a container healthcheck result.
type Health string

const (
	HealthNone      Health = ""          // the image defines no healthcheck
	HealthStarting  Health = "starting"  // not yet known to be healthy
	HealthHealthy   Health = "healthy"   // passing
	HealthUnhealthy Health = "unhealthy" // failing
)

// ContainerInfo is what Berth needs to know about a container.
type ContainerInfo struct {
	ID         string
	Name       string
	Status     string // Docker's state: created, running, paused, restarting, removing, exited, dead
	Running    bool
	Health     Health
	ExitCode   int
	OOMKilled  bool
	StartedAt  time.Time // zero if never started
	FinishedAt time.Time // zero if still running or never started
	Labels     map[string]string
	Networks   []string // names of the networks it is attached to
}

// VolumeMount mounts a named volume into the container.
type VolumeMount struct {
	Volume string
	Target string
}

// ContainerSpec is everything needed to create a workspace container.
type ContainerSpec struct {
	Name     string
	Hostname string
	Image    string
	Env      []string // KEY=value
	Labels   map[string]string
	Network  string // the single network the container joins
	Volumes  []VolumeMount

	MemoryBytes  int64
	NanoCPUs     int64
	ShmSizeBytes int64
	PidsLimit    int64
	CapDrop      []string
	SecurityOpt  []string
	Init         bool
	// StopTimeout is how long `docker stop` waits for a graceful exit by default.
	StopTimeout time.Duration

	// UDPPortStart..UDPPortEnd (inclusive) are published on the host, port for
	// port: WebRTC advertises the in-container port, so the numbers must match.
	// Zero means publish nothing.
	UDPPortStart, UDPPortEnd int
}

// Stats is a point-in-time resource sample.
type Stats struct {
	// CPUPercent is `docker stats` style: 100 means one full core busy.
	CPUPercent    float64
	MemUsageBytes uint64 // working set: usage minus reclaimable page cache
	MemLimitBytes uint64
	Pids          uint64
}

// Client is the Docker surface Berth uses. Every method is safe to call
// repeatedly; the "Ensure" and "Connect" ones are idempotent.
type Client interface {
	Ping(ctx context.Context) error

	// EnsureNetwork creates a bridge network unless it already exists.
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	// ManagedNetworks lists the names of the networks Berth created.
	ManagedNetworks(ctx context.Context) ([]string, error)
	// ConnectToNetwork attaches an existing container to a network unless it is
	// already attached. The attachment never becomes the container's default
	// gateway (it would move the container's published ports and reset live
	// connections). ErrNotFound if the container or network is missing.
	ConnectToNetwork(ctx context.Context, network, container string) error
	// EnsureVolume creates a named volume unless it already exists.
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error

	// CreateContainer creates (does not start) a container and returns its ID.
	// ErrNameInUse if the name is taken.
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	// StartContainer starts a container; starting a running one is not an error.
	StartContainer(ctx context.Context, id string) error
	// StopContainer stops a container gracefully, killing it after timeout;
	// stopping a stopped one is not an error.
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	// RemoveContainer force-removes a container. It never removes volumes, and
	// removing a missing one is not an error.
	RemoveContainer(ctx context.Context, id string) error
	// InspectContainer takes an ID or a name. ErrNotFound if it does not exist.
	InspectContainer(ctx context.Context, idOrName string) (ContainerInfo, error)
	// ContainerStats samples CPU and memory; it takes about a second because
	// CPU needs two readings.
	ContainerStats(ctx context.Context, id string) (Stats, error)
}
