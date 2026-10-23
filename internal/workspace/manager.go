package workspace

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

var (
	// ErrNoCapacity means the host has no free UDP port block for another
	// container.
	ErrNoCapacity = errors.New("workspace: no capacity for another container")
	// ErrBusy means another request is in the middle of changing the workspace
	// (a power-off during a start, or a power-on during a power-off).
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

	// flight serialises the operations that change a workspace's container.
	// Berth is one process, so this in-memory claim is what tells "a start is
	// running right now" from "a start died with the previous process".
	flight *flights
}

const (
	opStart   = "start"
	opStop    = "stop"
	opRecover = "recover"
)

// flights records which operation, if any, currently owns each user's workspace
// and counts operations so that an observer can tell one overlapped it.
type flights struct {
	mu     sync.Mutex
	active map[int64]string // user ID -> operation in progress
	epoch  map[int64]uint64 // user ID -> operations begun so far
}

func newFlights() *flights {
	return &flights{active: map[int64]string{}, epoch: map[int64]uint64{}}
}

// begin claims the user's workspace for op. When another operation owns it, it
// returns that operation's name and false.
func (f *flights) begin(id int64, op string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur := f.active[id]; cur != "" {
		return cur, false
	}
	f.active[id] = op
	f.epoch[id]++
	return "", true
}

func (f *flights) end(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, id)
}

// mark is taken before an observation reads anything: the epoch to compare
// with unchanged later.
func (f *flights) mark(id int64) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch[id]
}

// unchanged reports that no operation began or is running since mark, so what
// an observation saw is still the truth about the container.
func (f *flights) unchanged(id int64, epoch uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[id] == "" && f.epoch[id] == epoch
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
	return &Manager{store: st, docker: dc, cfg: cfg, log: log, now: now, flight: newFlights()}, nil
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
// one caller per user claims the workspace and does the Docker work, the others
// just report the state. It returns once the container has been started;
// readiness (starting -> running) follows from Status as the image's healthcheck
// passes. A power-on while a power-off is running is refused (ErrBusy).
func (m *Manager) Start(ctx context.Context, user store.User) (Status, error) {
	if err := checkName(user.Username); err != nil {
		return Status{}, err
	}
	if other, ok := m.flight.begin(user.ID, opStart); !ok {
		if other == opStart {
			return m.Status(ctx, user) // someone is already starting it
		}
		return Status{}, ErrBusy
	}
	err := m.startClaimed(ctx, user)
	m.flight.end(user.ID)
	if err != nil {
		return Status{}, err
	}
	return m.Status(ctx, user)
}

func (m *Manager) startClaimed(ctx context.Context, user store.User) error {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StartTimeout)
	defer cancel()

	// A start that died with an earlier process left the row at `starting`.
	if err := m.recoverClaimed(ctx, user); err != nil {
		return err
	}
	now := m.now()
	ws, err := m.store.TransitionWorkspace(ctx, user.ID, Sources(EventStart), store.StateStarting, now,
		store.SetActiveAt(now))
	switch {
	case errors.Is(err, store.ErrStateConflict):
		return nil // already starting (container started, waiting for its healthcheck) or running
	case err != nil:
		return err
	}

	createdID, err := m.bringUp(ctx, user, ws)
	if err != nil {
		m.rollback(user, createdID)
		return err
	}
	return nil
}

// bringUp does the Docker work of a start. It returns the ID of a container it
// created in this attempt (so a failure can remove it), or "".
func (m *Manager) bringUp(ctx context.Context, user store.User, ws store.Workspace) (createdID string, err error) {
	net := NetworkName(user.Username)
	labels := map[string]string{docker.LabelManaged: "true", docker.LabelUser: user.Username}

	if err := m.docker.EnsureNetwork(ctx, net, labels); err != nil {
		if errors.Is(err, docker.ErrNoAddressSpace) {
			m.log.Error("docker has no address space left for another user network; "+
				"set default-address-pools in daemon.json (README)", "err", err)
			return "", ErrNoCapacity
		}
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
		switch {
		case err == nil:
			return "", nil
		case errors.Is(err, docker.ErrNotFound):
			m.log.Warn("workspace container vanished while stopped; recreating",
				"user", user.Username, "container", ws.ContainerID)
		case errors.Is(err, docker.ErrStaleContainer):
			// It references something that no longer exists (its network was removed
			// and recreated). The data is on the volume: replace the container.
			m.log.Warn("workspace container cannot start; replacing it",
				"user", user.Username, "container", ws.ContainerID, "err", err)
			if rerr := m.docker.RemoveContainer(ctx, ws.ContainerID); rerr != nil {
				return "", rerr
			}
		default:
			return "", err
		}
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
	if errors.Is(err, docker.ErrNameInUse) {
		id, err = m.replaceOrphan(ctx, user, spec)
	}
	if err != nil {
		return "", err
	}
	createdID = id

	if _, err := m.store.TransitionWorkspace(ctx, user.ID, []store.State{store.StateStarting}, store.StateStarting,
		m.now(), store.SetContainerID(id), store.SetNekoPassword(userPW)); err != nil {
		return createdID, err
	}
	if err := m.docker.StartContainer(ctx, id); err != nil {
		return createdID, err
	}
	return createdID, nil
}

// ownedBy reports whether a container is one Berth created for this user.
func ownedBy(info docker.ContainerInfo, user store.User) bool {
	return info.Labels[docker.LabelManaged] == "true" && info.Labels[docker.LabelUser] == user.Username
}

// replaceOrphan deals with a container that already has the workspace's name
// although the database records no such container: the leftover of an attempt
// that died before recording it, or of a wiped database. It was built with a
// password and UDP block the database no longer matches, and everything worth
// keeping is on the volume, so it is replaced rather than adopted. Anything Berth
// did not create is never touched.
func (m *Manager) replaceOrphan(ctx context.Context, user store.User, spec docker.ContainerSpec) (string, error) {
	info, err := m.docker.InspectContainer(ctx, spec.Name)
	if err != nil {
		return "", err
	}
	if !ownedBy(info, user) {
		return "", fmt.Errorf("container name %s is taken by something Berth did not create", spec.Name)
	}
	m.log.Warn("replacing an orphaned workspace container", "user", user.Username, "container", info.ID)
	if err := m.docker.RemoveContainer(ctx, info.ID); err != nil {
		return "", err
	}
	return m.docker.CreateContainer(ctx, spec)
}

// sweepOrphan removes a container named for the user's workspace that the
// database does not record. It reports whether it is certain none is left, which
// is what makes freeing the UDP block safe: a container that survives keeps its
// ports published.
func (m *Manager) sweepOrphan(ctx context.Context, user store.User) bool {
	info, err := m.docker.InspectContainer(ctx, ContainerName(user.Username))
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return true
	case err != nil:
		return false
	case !ownedBy(info, user):
		return true // not ours: it neither holds our ports nor is ours to remove
	}
	if err := m.docker.RemoveContainer(ctx, info.ID); err != nil {
		m.log.Error("remove orphaned workspace container", "user", user.Username, "container", info.ID, "err", err)
		return false
	}
	return true
}

// rollback undoes a failed start. It uses its own short context: the request's
// may be the very thing that expired. It frees the UDP block only when it knows
// no container is left holding it, and it concludes that a kept container is gone
// only when Docker says so, never because Docker could not be asked.
func (m *Manager) rollback(user store.User, createdID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log := m.log.With("user", user.Username)

	createdGone := true
	if createdID != "" {
		if err := m.docker.RemoveContainer(ctx, createdID); err != nil {
			log.Error("rollback: remove container", "container", createdID, "err", err)
			createdGone = false
		}
	}
	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		log.Error("rollback: reload workspace", "err", err)
		return
	}

	// A container recorded before this attempt: it survives unless Docker says it
	// does not exist.
	keptSurvives := false
	if ws.ContainerID != "" && ws.ContainerID != createdID {
		_, ierr := m.docker.InspectContainer(ctx, ws.ContainerID)
		keptSurvives = ierr == nil || !errors.Is(ierr, docker.ErrNotFound)
	}

	now := m.now()
	var event Event
	var opts []store.TransitionOption
	if keptSurvives {
		event, opts = EventExited, []store.TransitionOption{store.SetStoppedAt(now)}
	} else {
		// Nothing recorded survives. A create whose reply was lost can still have
		// left a container of this attempt behind: find it by name.
		event, opts = EventGone, []store.TransitionOption{store.ClearContainer()}
		if createdGone && m.sweepOrphan(ctx, user) {
			opts = append(opts, store.ReleaseUDPRange())
		}
	}
	to, _ := Next(store.StateStarting, event)
	if _, err := m.store.TransitionWorkspace(ctx, user.ID, []store.State{store.StateStarting}, to, now, opts...); err != nil &&
		!errors.Is(err, store.ErrStateConflict) {
		log.Error("rollback: restore state", "err", err)
	}
}

// recoverAbandoned resets a workspace whose Start died with an earlier process
// (a redeploy or crash between claiming the row and recording a container).
func (m *Manager) recoverAbandoned(ctx context.Context, user store.User) error {
	if _, ok := m.flight.begin(user.ID, opRecover); !ok {
		return nil // an operation is running: not abandoned
	}
	defer m.flight.end(user.ID)
	return m.recoverClaimed(ctx, user)
}

// recoverClaimed does the work of recoverAbandoned for a caller that already
// owns the workspace. A `starting` row without a container is abandoned for
// certain: a live start records the container (or rolls back) before it
// releases its claim, and nothing is running now.
func (m *Manager) recoverClaimed(ctx context.Context, user store.User) error {
	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		return err
	}
	if ws.State != store.StateStarting || ws.ContainerID != "" {
		return nil
	}
	m.log.Warn("recovering a start that never finished", "user", user.Username)
	opts := []store.TransitionOption{store.ClearContainer()}
	if m.sweepOrphan(ctx, user) {
		opts = append(opts, store.ReleaseUDPRange())
	}
	to, _ := Next(store.StateStarting, EventGone)
	if _, err := m.store.TransitionWorkspace(ctx, user.ID, []store.State{store.StateStarting}, to, m.now(), opts...); err != nil &&
		!errors.Is(err, store.ErrStateConflict) {
		return err
	}
	return nil
}

// Stop powers a workspace off, keeping its container and data. It is
// idempotent: a workspace that is not up is reported as is. A power-off while a
// start is still creating the container is refused (ErrBusy): it could not stop
// a container that is not running yet, and the start would then run it anyway.
func (m *Manager) Stop(ctx context.Context, user store.User) (Status, error) {
	if err := checkName(user.Username); err != nil {
		return Status{}, err
	}
	if other, ok := m.flight.begin(user.ID, opStop); !ok {
		if other == opStop {
			return m.Status(ctx, user)
		}
		return Status{}, ErrBusy
	}
	err := m.stopClaimed(ctx, user)
	m.flight.end(user.ID)
	if err != nil {
		return Status{}, err
	}
	return m.Status(ctx, user)
}

func (m *Manager) stopClaimed(ctx context.Context, user store.User) error {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StopTimeout+30*time.Second)
	defer cancel()

	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		return err
	}
	if _, err := Next(ws.State, EventStop); err != nil {
		return nil // offline or already stopped
	}
	if ws.ContainerID == "" {
		// `starting` with no container while no start is running: abandoned.
		return m.recoverClaimed(ctx, user)
	}

	event, opts := EventStop, []store.TransitionOption{store.SetStoppedAt(m.now())}
	if err := m.docker.StopContainer(ctx, ws.ContainerID, m.cfg.StopTimeout); err != nil {
		if !errors.Is(err, docker.ErrNotFound) {
			return err
		}
		event, opts = EventGone, []store.TransitionOption{store.ClearContainer(), store.ReleaseUDPRange()}
	}
	to, _ := Next(ws.State, event)
	if _, err := m.store.TransitionWorkspace(ctx, user.ID, Sources(event), to, m.now(), opts...); err != nil &&
		!errors.Is(err, store.ErrStateConflict) {
		return err
	}
	return nil
}

// Status brings the database in line with what Docker reports about the
// workspace's container, then returns the result with a resource sample when
// it is running. Errors from Docker are returned only when they leave the
// database state unknowable; a failed stats sample just omits the numbers.
//
// If a start, stop or recovery owns the workspace, or began or ended while Status
// was looking, Status changes nothing and reports what it saw: a container that
// is merely not running yet is not a crash, and an answer from before the
// operation says nothing about the container it left behind.
func (m *Manager) Status(ctx context.Context, user store.User) (Status, error) {
	epoch := m.flight.mark(user.ID)
	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		return Status{}, err
	}
	st := Status{Workspace: ws}
	if ws.State == store.StateStarting && ws.ContainerID == "" {
		// Nothing is starting it (it would hold the claim): abandoned.
		if err := m.recoverAbandoned(ctx, user); err != nil {
			return Status{}, err
		}
		if cur, rerr := m.store.WorkspaceByUserID(ctx, user.ID); rerr == nil {
			st.Workspace = cur
		}
		return st, nil
	}
	if ws.ContainerID == "" {
		return st, nil // offline: nothing to observe
	}

	info, err := m.docker.InspectContainer(ctx, ws.ContainerID)
	switch {
	case errors.Is(err, docker.ErrNotFound):
		to, nerr := Next(ws.State, EventGone)
		if nerr != nil || !m.flight.unchanged(user.ID, epoch) {
			return st, nil
		}
		m.log.Warn("workspace container is gone", "user", user.Username, "container", ws.ContainerID)
		updated, terr := m.store.TransitionWorkspace(ctx, user.ID, []store.State{ws.State}, to, m.now(),
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
	if info.Running {
		// A stopped container keeps its last healthcheck result; it says nothing now.
		st.Health = info.Health
	}

	if ev, ok := Observe(ws.State, info); ok && m.flight.unchanged(user.ID, epoch) {
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
