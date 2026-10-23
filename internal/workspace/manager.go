package workspace

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

var (
	// ErrNoCapacity means the host has no free UDP port block for another
	// container.
	ErrNoCapacity = errors.New("workspace: no capacity for another container")
	// ErrBusy means another request is in the middle of changing the workspace.
	ErrBusy = errors.New("workspace: another operation is in progress")
	// ErrBadUsername guards the names derived from a username.
	ErrBadUsername = errors.New("workspace: invalid username")
)

// Config is what the Manager needs to know about the deployment.
type Config struct {
	Image string // the workspace image, e.g. berth/workspace:latest
	// TraefikContainer is the container to attach to every user network so the
	// proxy can reach workspaces that join no shared network. Empty disables
	// attaching (for development without Traefik).
	TraefikContainer string
	Entrypoint       string // Traefik entrypoint the routers use

	UDPPortMin, UDPPortMax int
	UDPPortsPerWorkspace   int

	Resolution string // neko screen, e.g. 1280x720@30
	PublicIP   string // advertised to WebRTC peers; empty = let neko pick
	ShmSizeMB  int
	PidsLimit  int64

	StopTimeout  time.Duration // graceful stop before SIGKILL
	StartTimeout time.Duration // bound on one Start call
}

// Status is a workspace as the API reports it.
type Status struct {
	Workspace store.Workspace
	Health    docker.Health
	// Stats is set only for a running container whose sample succeeded.
	Stats *docker.Stats
}

// Manager runs the workspace lifecycle: it creates, starts, stops and observes
// containers, and keeps the database in step with what Docker reports.
type Manager struct {
	store  store.Store
	docker docker.Client
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
}

// Validate rejects a Config that would misbehave at runtime (a zero timeout
// makes every start fail instantly). Defaults live in the config package, so
// there is exactly one place that decides them.
func (c Config) Validate() error {
	switch {
	case c.Image == "":
		return errors.New("workspace: image is required")
	case c.Entrypoint == "":
		return errors.New("workspace: Traefik entrypoint is required")
	case c.Resolution == "":
		return errors.New("workspace: resolution is required")
	case c.StopTimeout <= 0 || c.StartTimeout <= 0:
		return errors.New("workspace: stop and start timeouts must be positive")
	case c.ShmSizeMB <= 0 || c.PidsLimit <= 0:
		return errors.New("workspace: shm size and pids limit must be positive")
	case c.UDPPortsPerWorkspace < 1 || c.UDPPortMin < 1 || c.UDPPortMax > 65535 ||
		c.UDPPortMax-c.UDPPortMin+1 < c.UDPPortsPerWorkspace:
		return errors.New("workspace: UDP port range must hold at least one block of the configured size")
	}
	return nil
}

// NewManager builds a Manager. now may be nil (time.Now).
func NewManager(st store.Store, dc docker.Client, cfg Config, log *slog.Logger, now func() time.Time) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{store: st, docker: dc, cfg: cfg, log: log, now: now}, nil
}

// checkName is defence in depth: usernames are validated at registration, but
// they end up in container, network and Traefik router names, so refuse
// anything unexpected right where it is used.
func checkName(username string) error {
	if !auth.ValidUsername(username) {
		return fmt.Errorf("%w: %q", ErrBadUsername, username)
	}
	return nil
}

// Start powers a workspace on. It is idempotent and safe to call concurrently:
// the database compare-and-swap lets exactly one caller do the Docker work, and
// the others just report the state. It returns once the container has been
// started; readiness (starting -> running) follows from Status as the image's
// healthcheck passes.
func (m *Manager) Start(ctx context.Context, user store.User) (Status, error) {
	if err := checkName(user.Username); err != nil {
		return Status{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StartTimeout)
	defer cancel()

	now := m.now()
	ws, err := m.store.TransitionWorkspace(ctx, user.ID, Sources(EventStart), store.StateStarting, now,
		store.SetActiveAt(now))
	switch {
	case errors.Is(err, store.ErrStateConflict):
		return m.Status(ctx, user) // already starting or running
	case err != nil:
		return Status{}, err
	}

	createdID, err := m.bringUp(ctx, user, ws)
	if err != nil {
		m.rollback(user, createdID)
		return Status{}, err
	}
	return m.Status(ctx, user)
}

// bringUp does the Docker work of a start. It returns the ID of a container it
// created in this attempt (so a failure can remove it), or "".
func (m *Manager) bringUp(ctx context.Context, user store.User, ws store.Workspace) (createdID string, err error) {
	net := NetworkName(user.Username)
	labels := map[string]string{docker.LabelManaged: "true", docker.LabelUser: user.Username}

	if err := m.docker.EnsureNetwork(ctx, net, labels); err != nil {
		return "", err
	}
	if m.cfg.TraefikContainer != "" {
		if err := m.docker.ConnectToNetwork(ctx, net, m.cfg.TraefikContainer); err != nil {
			return "", fmt.Errorf("attach proxy %q to %s: %w", m.cfg.TraefikContainer, net, err)
		}
	}
	if err := m.docker.EnsureVolume(ctx, ws.VolumeName, labels); err != nil {
		return "", err
	}

	// A returning user: the container was kept, just start it.
	if ws.ContainerID != "" {
		err := m.docker.StartContainer(ctx, ws.ContainerID)
		if err == nil {
			return "", nil
		}
		if !errors.Is(err, docker.ErrNotFound) {
			return "", err
		}
		m.log.Warn("workspace container vanished while stopped; recreating",
			"user", user.Username, "container", ws.ContainerID)
	}

	// A new container: it needs its UDP block and the neko password.
	udpStart, udpEnd, err := m.store.AllocateUDPRange(ctx, user.ID,
		m.cfg.UDPPortMin, m.cfg.UDPPortMax, m.cfg.UDPPortsPerWorkspace, m.now())
	if errors.Is(err, store.ErrNoPortsFree) {
		return "", ErrNoCapacity
	} else if err != nil {
		return "", err
	}
	userPW := ws.NekoPassword
	if userPW == "" {
		if userPW, err = randomSecret(); err != nil {
			return "", err
		}
	}
	adminPW, err := randomSecret()
	if err != nil {
		return "", err
	}

	spec := buildSpec(m.cfg, user, ws, userPW, adminPW, udpStart, udpEnd)
	id, err := m.docker.CreateContainer(ctx, spec)
	switch {
	case errors.Is(err, docker.ErrNameInUse):
		// An earlier attempt died between creating the container and recording
		// it. Adopt that container if it is ours; never touch anything else.
		info, ierr := m.docker.InspectContainer(ctx, spec.Name)
		if ierr != nil {
			return "", ierr
		}
		if info.Labels[docker.LabelManaged] != "true" || info.Labels[docker.LabelUser] != user.Username {
			return "", fmt.Errorf("container name %s is taken by something Berth did not create", spec.Name)
		}
		id = info.ID
	case err != nil:
		return "", err
	default:
		createdID = id
	}

	if _, err := m.store.TransitionWorkspace(ctx, user.ID, []store.State{store.StateStarting}, store.StateStarting,
		m.now(), store.SetContainerID(id), store.SetNekoPassword(userPW)); err != nil {
		return createdID, err
	}
	if err := m.docker.StartContainer(ctx, id); err != nil {
		return createdID, err
	}
	return createdID, nil
}

// rollback undoes a failed start. It uses its own short context: the request's
// may be the very thing that expired.
func (m *Manager) rollback(user store.User, createdID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log := m.log.With("user", user.Username)

	if createdID != "" {
		if err := m.docker.RemoveContainer(ctx, createdID); err != nil {
			log.Error("rollback: remove container", "container", createdID, "err", err)
		}
	}
	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		log.Error("rollback: reload workspace", "err", err)
		return
	}
	now := m.now()
	var event Event
	var opts []store.TransitionOption
	if ws.ContainerID != "" && createdID != ws.ContainerID {
		if _, err := m.docker.InspectContainer(ctx, ws.ContainerID); err == nil {
			// The kept container still exists: the workspace is simply stopped.
			event, opts = EventExited, []store.TransitionOption{store.SetStoppedAt(now)}
		}
	}
	if event == "" {
		event, opts = EventGone, []store.TransitionOption{store.ClearContainer(), store.ReleaseUDPRange()}
	}
	to, _ := Next(store.StateStarting, event)
	if _, err := m.store.TransitionWorkspace(ctx, user.ID, []store.State{store.StateStarting}, to, now, opts...); err != nil &&
		!errors.Is(err, store.ErrStateConflict) {
		log.Error("rollback: restore state", "err", err)
	}
}

// Stop powers a workspace off, keeping its container and data. It is
// idempotent: a workspace that is not up is reported as is.
func (m *Manager) Stop(ctx context.Context, user store.User) (Status, error) {
	if err := checkName(user.Username); err != nil {
		return Status{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StopTimeout+30*time.Second)
	defer cancel()

	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		return Status{}, err
	}
	if _, err := Next(ws.State, EventStop); err != nil {
		return m.Status(ctx, user) // offline or already stopped
	}
	if ws.ContainerID == "" {
		return Status{}, ErrBusy // a start is still creating the container
	}

	event, opts := EventStop, []store.TransitionOption{store.SetStoppedAt(m.now())}
	if err := m.docker.StopContainer(ctx, ws.ContainerID, m.cfg.StopTimeout); err != nil {
		if !errors.Is(err, docker.ErrNotFound) {
			return Status{}, err
		}
		event, opts = EventGone, []store.TransitionOption{store.ClearContainer(), store.ReleaseUDPRange()}
	}
	to, _ := Next(ws.State, event)
	if _, err := m.store.TransitionWorkspace(ctx, user.ID, Sources(event), to, m.now(), opts...); err != nil &&
		!errors.Is(err, store.ErrStateConflict) {
		return Status{}, err
	}
	return m.Status(ctx, user)
}

// Status brings the database in line with what Docker reports about the
// workspace's container, then returns the result with a resource sample when
// it is running. Errors from Docker are returned only when they leave the
// database state unknowable; a failed stats sample just omits the numbers.
func (m *Manager) Status(ctx context.Context, user store.User) (Status, error) {
	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		return Status{}, err
	}
	st := Status{Workspace: ws}
	if ws.ContainerID == "" {
		return st, nil // nothing to observe (offline, or a start still creating it)
	}

	info, err := m.docker.InspectContainer(ctx, ws.ContainerID)
	switch {
	case errors.Is(err, docker.ErrNotFound):
		to, nerr := Next(ws.State, EventGone)
		if nerr != nil {
			return st, nil
		}
		m.log.Warn("workspace container is gone", "user", user.Username, "container", ws.ContainerID)
		updated, terr := m.store.TransitionWorkspace(ctx, user.ID, Sources(EventGone), to, m.now(),
			store.ClearContainer(), store.ReleaseUDPRange())
		if terr != nil && !errors.Is(terr, store.ErrStateConflict) {
			return Status{}, terr
		}
		if terr == nil {
			st.Workspace = updated
		}
		return st, nil
	case err != nil:
		return Status{}, err
	}
	st.Health = info.Health

	if ev, ok := Observe(ws.State, info); ok {
		to, _ := Next(ws.State, ev)
		var opts []store.TransitionOption
		if to == store.StateStopped {
			stopped := info.FinishedAt
			if stopped.IsZero() {
				stopped = m.now()
			}
			opts = append(opts, store.SetStoppedAt(stopped))
		}
		updated, terr := m.store.TransitionWorkspace(ctx, user.ID, []store.State{ws.State}, to, m.now(), opts...)
		switch {
		case terr == nil:
			st.Workspace = updated
		case errors.Is(terr, store.ErrStateConflict):
			// Another request moved it first; report what is there now.
			if cur, rerr := m.store.WorkspaceByUserID(ctx, user.ID); rerr == nil {
				st.Workspace = cur
			}
		default:
			return Status{}, terr
		}
	}

	if st.Workspace.State == store.StateRunning && info.Running {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if stats, serr := m.docker.ContainerStats(sctx, ws.ContainerID); serr == nil {
			st.Stats = &stats
		} else {
			m.log.Debug("workspace stats unavailable", "user", user.Username, "err", serr)
		}
	}
	return st, nil
}

// Heartbeat records that the user is actively using the workspace.
func (m *Manager) Heartbeat(ctx context.Context, user store.User) error {
	return m.store.TouchLastActive(ctx, user.ID, m.now())
}

// ReattachTraefik connects the proxy to every user network. Docker drops the
// attachment when the Traefik container is recreated (for example by
// `docker compose up -d` after an image update), after which every workspace
// would answer 504 until something reconnected it. It is idempotent and cheap,
// so the backend runs it at startup and periodically.
func (m *Manager) ReattachTraefik(ctx context.Context) error {
	if m.cfg.TraefikContainer == "" {
		return nil
	}
	nets, err := m.docker.ManagedNetworks(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range nets {
		if err := m.docker.ConnectToNetwork(ctx, n, m.cfg.TraefikContainer); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n, err))
		}
	}
	return errors.Join(errs...)
}

// randomSecret returns 18 random bytes, URL-safe, for the neko passwords.
func randomSecret() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("workspace: random secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
