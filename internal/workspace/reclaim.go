package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

// The operations below are what the reaper (internal/reaper) does to a
// workspace. The reaper decides by policy; the Manager guards each action. Every
// one takes the user's claim like a Start or Stop does, so it can never overlap
// a user's own power-on or power-off, and it asks the policy again about the row
// as it is read under that claim: the decision was made from a snapshot that a
// heartbeat, a power-off or a power-on may have overtaken since. When another
// operation owns the workspace the action is skipped (false, nil), not queued:
// the reaper looks again on its next pass.

// StopIfIdle stops a running workspace, keeping its container and data, if
// idle still holds for the workspace as it is now. It reports whether the
// container was stopped.
func (m *Manager) StopIfIdle(ctx context.Context, user store.User, idle func(store.Workspace) bool) (bool, error) {
	if err := checkName(user.Username); err != nil {
		return false, err
	}
	if idle == nil {
		return false, errors.New("workspace: StopIfIdle needs a policy")
	}
	if _, ok := m.flight.begin(user.ID, opStop); !ok {
		return false, nil
	}
	return m.stopClaimed(ctx, user, idle)
}

// ReclaimIf removes the container of a stopped workspace if reclaimable still
// holds for the workspace as it is now: the workspace goes offline, its UDP
// block is released, and only the data volume (and the password the next
// container will reuse) remain. It reports whether a container was removed.
func (m *Manager) ReclaimIf(ctx context.Context, user store.User, reclaimable func(store.Workspace) bool) (bool, error) {
	if err := checkName(user.Username); err != nil {
		return false, err
	}
	if reclaimable == nil {
		return false, errors.New("workspace: ReclaimIf needs a policy")
	}
	if _, ok := m.flight.begin(user.ID, opReclaim); !ok {
		return false, nil
	}
	defer m.flight.end(user.ID)
	ctx, cancel := context.WithTimeout(ctx, m.cfg.StopTimeout+30*time.Second)
	defer cancel()

	ws, err := m.store.WorkspaceByUserID(ctx, user.ID)
	if err != nil {
		return false, err
	}
	if ws.State != store.StateStopped || ws.ContainerID == "" || !reclaimable(ws) {
		return false, nil
	}
	// Prove from Docker, not from the row, that this is Berth's stopped container.
	// A row can be behind the daemon (a database restored from a backup, a
	// container started by hand); removal is a force-remove, so a running container
	// would lose its processes. Anything unexpected is left for Reconcile.
	info, err := m.docker.InspectContainer(ctx, ws.ContainerID)
	switch {
	case errors.Is(err, docker.ErrNotFound):
		// Already gone: the transition below is all that is left to do.
	case err != nil:
		return false, err
	case info.Running || !ownedBy(info, user):
		if !info.Running {
			m.log.Warn("not reclaiming a container that is not the one Berth created for this user",
				"user", user.Username, "container", ws.ContainerID)
		}
		return false, nil
	default:
		if err := m.docker.RemoveContainer(ctx, ws.ContainerID); err != nil {
			return false, err
		}
	}
	// The container is gone. Its UDP block is released only if Docker confirms
	// that nothing else under the workspace's name is left to publish those ports.
	opts := []store.TransitionOption{store.ClearContainer(), store.OnlyIfContainer(ws.ContainerID)}
	if _, nerr := m.docker.InspectContainer(ctx, ContainerName(user.Username)); errors.Is(nerr, docker.ErrNotFound) {
		opts = append(opts, store.ReleaseUDPRange())
	}
	to, _ := Next(ws.State, EventReclaim)
	_, err = m.store.TransitionWorkspace(ctx, user.ID, Sources(EventReclaim), to, m.now(), opts...)
	switch {
	case err == nil:
		m.log.Info("reclaimed a stopped workspace; its data volume is kept", "user", user.Username, "container", ws.ContainerID)
		return true, nil
	case errors.Is(err, store.ErrStateConflict):
		return false, nil
	}
	return false, err
}
