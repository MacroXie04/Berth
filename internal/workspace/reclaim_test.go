package workspace

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

func always(store.Workspace) bool { return true }
func never(store.Workspace) bool  { return false }

// startRunning powers a user on and lets the healthcheck pass.
func (r *rig) startRunning(name string) (store.User, string) {
	r.t.Helper()
	u := r.user(name)
	r.mustStart(u)
	id := r.containerID(name)
	r.dock.SetHealth(id, docker.HealthHealthy)
	if st, err := r.m.Status(ctx, u); err != nil || st.Workspace.State != store.StateRunning {
		r.t.Fatalf("not running: %+v, %v", st.Workspace, err)
	}
	return u, id
}

func (r *rig) up(id string) bool {
	r.t.Helper()
	info, err := r.dock.InspectContainer(ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return info.Running
}

func TestStopIfIdleStopsARunningWorkspaceAndKeepsItsContainer(t *testing.T) {
	r := newRig(t)
	u, id := r.startRunning("alice")
	r.clock.Advance(time.Hour)

	var seen store.Workspace
	stopped, err := r.m.StopIfIdle(ctx, u, func(ws store.Workspace) bool { seen = ws; return true })
	if err != nil || !stopped {
		t.Fatalf("StopIfIdle = %v, %v", stopped, err)
	}
	if seen.State != store.StateRunning || seen.ContainerID != id {
		t.Errorf("the policy was shown %+v, want the running workspace as it is now", seen)
	}
	if r.up(id) {
		t.Error("the container is still running")
	}
	ws := r.ws(u)
	if ws.State != store.StateStopped || ws.ContainerID != id {
		t.Errorf("workspace = %+v, want stopped with its container kept", ws)
	}
	if ws.LastStoppedAt == nil || !ws.LastStoppedAt.Equal(r.clock.Now()) {
		t.Errorf("last_stopped_at = %v, want %v", ws.LastStoppedAt, r.clock.Now())
	}
	if ws.UDPPortStart == 0 || !r.dock.HasVolume(VolumeName("alice")) {
		t.Errorf("a stopped workspace keeps its port block and volume: %+v", ws)
	}
	if got := r.dock.LastStopTimeout(); got != r.m.cfg.StopTimeout {
		t.Errorf("stop timeout = %v, want the configured %v", got, r.m.cfg.StopTimeout)
	}

	// The user comes back: the same container starts again, with the same password.
	pw := ws.NekoPassword
	r.mustStart(u)
	if got := r.containerID("alice"); got != id || !r.up(id) {
		t.Errorf("power-on after an idle stop: container %q running=%v, want %q running", got, r.up(id), id)
	}
	if r.ws(u).NekoPassword != pw {
		t.Error("the neko password changed")
	}
}

func TestStopIfIdleLeavesAnActiveWorkspaceAlone(t *testing.T) {
	r := newRig(t)
	u, id := r.startRunning("alice")

	stopped, err := r.m.StopIfIdle(ctx, u, never)
	if err != nil || stopped {
		t.Fatalf("StopIfIdle = %v, %v", stopped, err)
	}
	if !r.up(id) || r.ws(u).State != store.StateRunning {
		t.Fatalf("the policy said no, but the workspace was stopped: %+v", r.ws(u))
	}
	if n := r.dock.CallCount("StopContainer"); n != 0 {
		t.Errorf("StopContainer called %d times", n)
	}
}

// The reaper decides from a list it read a moment ago. A heartbeat that lands in
// between must win: the policy is asked about the row as it is read under the claim.
func TestStopIfIdleAsksThePolicyAboutTheCurrentRow(t *testing.T) {
	r := newRig(t)
	u, id := r.startRunning("alice")
	r.clock.Advance(time.Hour)
	cutoff := r.clock.Now().Add(-30 * time.Minute)
	idle := func(ws store.Workspace) bool { return ws.LastActiveAt == nil || ws.LastActiveAt.Before(cutoff) }

	// The snapshot the reaper decided on says idle...
	if !idle(r.ws(u)) {
		t.Fatal("setup: the workspace should look idle")
	}
	// ...then the user's page sends a heartbeat.
	if err := r.m.Heartbeat(ctx, u); err != nil {
		t.Fatal(err)
	}
	stopped, err := r.m.StopIfIdle(ctx, u, idle)
	if err != nil || stopped {
		t.Fatalf("StopIfIdle = %v, %v: a heartbeat must prevent the stop", stopped, err)
	}
	if !r.up(id) || r.ws(u).State != store.StateRunning {
		t.Fatalf("workspace = %+v", r.ws(u))
	}
}

func TestStopIfIdleOnlyStopsLiveWorkspaces(t *testing.T) {
	r := newRig(t)
	called := false
	policy := func(store.Workspace) bool { called = true; return true }

	// offline
	off := r.user("offline-user")
	if stopped, err := r.m.StopIfIdle(ctx, off, policy); err != nil || stopped {
		t.Fatalf("offline: %v, %v", stopped, err)
	}
	// stopped
	sp, _ := r.startRunning("stopped-user")
	if _, err := r.m.Stop(ctx, sp); err != nil {
		t.Fatal(err)
	}
	before := r.dock.CallCount("StopContainer")
	if stopped, err := r.m.StopIfIdle(ctx, sp, policy); err != nil || stopped {
		t.Fatalf("stopped: %v, %v", stopped, err)
	}
	if r.dock.CallCount("StopContainer") != before {
		t.Error("a stopped workspace was stopped again")
	}
	if called {
		t.Error("the policy was consulted for a workspace that is not up")
	}
}

// A container that never turns healthy leaves the row at `starting` for good
// (Observe gives no event for it). Once nobody is using it, it must be reapable
// like a running one, or it holds its memory forever.
func TestStopIfIdleAlsoStopsAStartingWorkspaceThatNeverBecameHealthy(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	id := r.containerID("alice")
	if st, _ := r.m.Status(ctx, u); st.Workspace.State != store.StateStarting {
		t.Fatalf("setup: %+v", st.Workspace)
	}

	if stopped, err := r.m.StopIfIdle(ctx, u, never); err != nil || stopped || !r.up(id) {
		t.Fatalf("an active starting workspace: stopped=%v err=%v up=%v", stopped, err, r.up(id))
	}
	stopped, err := r.m.StopIfIdle(ctx, u, always)
	if err != nil || !stopped {
		t.Fatalf("StopIfIdle = %v, %v", stopped, err)
	}
	if r.up(id) || r.ws(u).State != store.StateStopped {
		t.Errorf("workspace = %+v", r.ws(u))
	}
}

func TestStopIfIdleSkipsAWorkspaceAnotherOperationOwns(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	g := newGate(r.dock, "StartContainer")
	done := make(chan error, 1)
	go func() { _, err := r.m.Start(ctx, u); done <- err }()
	g.await(t) // the start owns the workspace and is inside Docker

	stopped, err := r.m.StopIfIdle(ctx, u, always)
	if err != nil || stopped {
		t.Errorf("StopIfIdle during a start = %v, %v, want a quiet skip", stopped, err)
	}
	if n := r.dock.CallCount("StopContainer"); n != 0 {
		t.Errorf("StopContainer called %d times during a start", n)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The claim is released: a later pass can act.
	r.dock.SetHealth(r.containerID("alice"), docker.HealthHealthy)
	if _, err := r.m.Status(ctx, u); err != nil {
		t.Fatal(err)
	}
	if stopped, err := r.m.StopIfIdle(ctx, u, always); err != nil || !stopped {
		t.Errorf("after the start: %v, %v", stopped, err)
	}
}

func TestStopIfIdleOfAContainerThatVanished(t *testing.T) {
	r := newRig(t)
	u, id := r.startRunning("alice")
	r.dock.Vanish(id)

	stopped, err := r.m.StopIfIdle(ctx, u, always)
	if err != nil || !stopped {
		t.Fatalf("StopIfIdle = %v, %v", stopped, err)
	}
	ws := r.ws(u)
	if ws.State != store.StateOffline || ws.ContainerID != "" || ws.UDPPortStart != 0 {
		t.Errorf("workspace = %+v, want offline with the container and port block released", ws)
	}
}

func TestStopIfIdleFailureKeepsTheStateAndReleasesTheClaim(t *testing.T) {
	r := newRig(t)
	u, id := r.startRunning("alice")
	r.dock.FailNext("StopContainer", errors.New("boom"))

	if stopped, err := r.m.StopIfIdle(ctx, u, always); err == nil || stopped {
		t.Fatalf("StopIfIdle = %v, %v, want the Docker error", stopped, err)
	}
	if ws := r.ws(u); ws.State != store.StateRunning || !r.up(id) {
		t.Fatalf("a failed stop must change nothing: %+v", ws)
	}
	if stopped, err := r.m.StopIfIdle(ctx, u, always); err != nil || !stopped {
		t.Fatalf("retry = %v, %v: the claim must have been released", stopped, err)
	}
}

func TestStopIfIdleRejectsBadInput(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	if _, err := r.m.StopIfIdle(ctx, u, nil); err == nil {
		t.Error("a nil policy must be refused, not treated as 'always'")
	}
	bad := store.User{ID: 99, Username: "Robert'); DROP TABLE"}
	if _, err := r.m.StopIfIdle(ctx, bad, always); !errors.Is(err, ErrBadUsername) {
		t.Errorf("bad username: %v", err)
	}
}

// A user's own power-off and the reaper's use the same claim and stop code.
func TestUserStopStillStopsUnconditionally(t *testing.T) {
	r := newRig(t)
	u, id := r.startRunning("alice")
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	if r.up(id) || r.ws(u).State != store.StateStopped {
		t.Fatalf("workspace = %+v", r.ws(u))
	}
}

func (r *rig) stopped(name string) (store.User, string) {
	r.t.Helper()
	u, id := r.startRunning(name)
	if _, err := r.m.Stop(ctx, u); err != nil {
		r.t.Fatal(err)
	}
	if r.ws(u).State != store.StateStopped {
		r.t.Fatalf("not stopped: %+v", r.ws(u))
	}
	return u, id
}

func TestReclaimIfRemovesTheContainerAndKeepsTheData(t *testing.T) {
	r := newRig(t)
	u, id := r.stopped("alice")
	before := r.ws(u)
	if before.UDPPortStart == 0 || before.NekoPassword == "" {
		t.Fatalf("setup: %+v", before)
	}
	r.clock.Advance(8 * 24 * time.Hour)

	ok, err := r.m.ReclaimIf(ctx, u, always)
	if err != nil || !ok {
		t.Fatalf("ReclaimIf = %v, %v", ok, err)
	}
	if got := r.dock.ContainerIDByName(ContainerName("alice")); got != "" {
		t.Errorf("container %s still exists", got)
	}
	ws := r.ws(u)
	if ws.State != store.StateOffline || ws.ContainerID != "" {
		t.Errorf("workspace = %+v, want offline without a container", ws)
	}
	if ws.UDPPortStart != 0 || ws.UDPPortEnd != 0 {
		t.Errorf("the port block %d-%d was not released", ws.UDPPortStart, ws.UDPPortEnd)
	}
	if !r.dock.HasVolume(VolumeName("alice")) || ws.VolumeName != VolumeName("alice") {
		t.Error("the data volume must survive")
	}
	if ws.NekoPassword != before.NekoPassword || ws.LastStoppedAt == nil || !ws.LastStoppedAt.Equal(*before.LastStoppedAt) {
		t.Errorf("password and last_stopped_at are kept: before %+v after %+v", before, ws)
	}

	// Power on again: a new container, the same volume and password, a port block.
	r.mustStart(u)
	newID := r.containerID("alice")
	if newID == "" || newID == id {
		t.Fatalf("container after power-on = %q (old %q), want a new one", newID, id)
	}
	spec, _ := r.dock.Spec(newID)
	if len(spec.Volumes) != 1 || spec.Volumes[0].Volume != VolumeName("alice") {
		t.Errorf("volumes = %+v", spec.Volumes)
	}
	if got := r.ws(u); got.NekoPassword != before.NekoPassword || got.UDPPortStart == 0 || got.State != store.StateStarting {
		t.Errorf("after power-on: %+v", got)
	}
}

func TestReclaimIfAsksThePolicyAboutTheCurrentRow(t *testing.T) {
	r := newRig(t)
	u, id := r.stopped("alice")

	var seen store.Workspace
	ok, err := r.m.ReclaimIf(ctx, u, func(ws store.Workspace) bool { seen = ws; return false })
	if err != nil || ok {
		t.Fatalf("ReclaimIf = %v, %v", ok, err)
	}
	if seen.State != store.StateStopped || seen.ContainerID != id {
		t.Errorf("the policy was shown %+v", seen)
	}
	if r.dock.ContainerIDByName(ContainerName("alice")) != id || r.ws(u).State != store.StateStopped {
		t.Error("the policy said no, but the workspace was reclaimed")
	}
	if n := r.dock.CallCount("RemoveContainer"); n != 0 {
		t.Errorf("RemoveContainer called %d times", n)
	}
}

func TestReclaimIfOnlyReclaimsStoppedWorkspaces(t *testing.T) {
	r := newRig(t)
	called := false
	policy := func(store.Workspace) bool { called = true; return true }

	off := r.user("offline-user")
	run, runID := r.startRunning("running-user")
	start := r.user("starting-user")
	r.mustStart(start)
	for name, u := range map[string]store.User{"offline": off, "running": run, "starting": start} {
		if ok, err := r.m.ReclaimIf(ctx, u, policy); err != nil || ok {
			t.Errorf("%s: ReclaimIf = %v, %v", name, ok, err)
		}
	}
	if called {
		t.Error("the policy was consulted for a workspace that is not stopped")
	}
	if !r.up(runID) || !r.up(r.containerID("starting-user")) {
		t.Error("a live container was removed")
	}
	if n := r.dock.CallCount("RemoveContainer"); n != 0 {
		t.Errorf("RemoveContainer called %d times", n)
	}
}

// A stopped workspace whose container someone started by hand is in use.
func TestReclaimIfNeverRemovesARunningContainer(t *testing.T) {
	r := newRig(t)
	u, id := r.stopped("alice")
	if err := r.dock.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	ok, err := r.m.ReclaimIf(ctx, u, always)
	if err != nil || ok {
		t.Fatalf("ReclaimIf = %v, %v", ok, err)
	}
	if !r.up(id) || r.dock.RemovedWhileRunning() != 0 || r.dock.CallCount("RemoveContainer") != 0 {
		t.Error("a running container was removed")
	}
	if ws := r.ws(u); ws.State != store.StateStopped || ws.ContainerID != id {
		t.Errorf("the row must be left for Reconcile: %+v", ws)
	}
}

// The row names a container ID; if that is not Berth's container for this user
// (a restored database, a recycled ID), removing it is not ours to do.
func TestReclaimIfOnlyRemovesBerthsContainerForThatUser(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"not labelled":      nil,
		"another user's":    {docker.LabelManaged: "true", docker.LabelUser: "mallory"},
		"labelled, no user": {docker.LabelManaged: "true"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			u, _ := r.stopped("alice")
			r.dock.Vanish(r.containerID("alice"))
			foreign, err := r.dock.CreateContainer(ctx, docker.ContainerSpec{Name: "someone-elses", Labels: labels})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.st.TransitionWorkspace(ctx, u.ID, []store.State{store.StateStopped}, store.StateStopped, r.clock.Now(),
				store.SetContainerID(foreign)); err != nil {
				t.Fatal(err)
			}
			ok, err := r.m.ReclaimIf(ctx, u, always)
			if err != nil || ok {
				t.Fatalf("ReclaimIf = %v, %v", ok, err)
			}
			if _, err := r.dock.InspectContainer(ctx, foreign); err != nil {
				t.Errorf("a container that is not Berth's for this user was removed: %v", err)
			}
			if ws := r.ws(u); ws.State != store.StateStopped || ws.ContainerID != foreign {
				t.Errorf("workspace = %+v", ws)
			}
		})
	}
}

// If something is still sitting under the workspace's name, it may publish the
// ports of the block: the block stays allocated.
func TestReclaimIfKeepsThePortBlockWhileAnotherContainerHoldsTheName(t *testing.T) {
	r := newRig(t)
	u, id := r.stopped("alice")
	block := r.ws(u).UDPPortStart
	r.dock.Vanish(id)
	if _, err := r.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name:   ContainerName("alice"),
		Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: "alice"},
	}); err != nil {
		t.Fatal(err)
	}

	ok, err := r.m.ReclaimIf(ctx, u, always)
	if err != nil || !ok {
		t.Fatalf("ReclaimIf = %v, %v", ok, err)
	}
	ws := r.ws(u)
	if ws.State != store.StateOffline || ws.ContainerID != "" {
		t.Errorf("workspace = %+v, want offline", ws)
	}
	if ws.UDPPortStart != block || block == 0 {
		t.Errorf("port block %d, want it kept (%d) while a container still holds the name", ws.UDPPortStart, block)
	}
}

func TestReclaimIfWhenTheContainerIsAlreadyGone(t *testing.T) {
	r := newRig(t)
	u, id := r.stopped("alice")
	r.dock.Vanish(id)
	ok, err := r.m.ReclaimIf(ctx, u, always)
	if err != nil || !ok {
		t.Fatalf("ReclaimIf = %v, %v", ok, err)
	}
	if ws := r.ws(u); ws.State != store.StateOffline || ws.ContainerID != "" || ws.UDPPortStart != 0 {
		t.Errorf("workspace = %+v", ws)
	}
}

// The port block is released only once the container that publishes it is gone.
func TestReclaimIfFailureKeepsEverythingAndRetries(t *testing.T) {
	r := newRig(t)
	u, id := r.stopped("alice")
	before := r.ws(u)
	r.dock.FailNext("RemoveContainer", errors.New("boom"))

	if ok, err := r.m.ReclaimIf(ctx, u, always); err == nil || ok {
		t.Fatalf("ReclaimIf = %v, %v, want the Docker error", ok, err)
	}
	ws := r.ws(u)
	if ws.State != store.StateStopped || ws.ContainerID != id || ws.UDPPortStart != before.UDPPortStart {
		t.Fatalf("a failed removal must change nothing: %+v", ws)
	}
	if ok, err := r.m.ReclaimIf(ctx, u, always); err != nil || !ok {
		t.Fatalf("retry = %v, %v", ok, err)
	}
}

func TestReclaimIfSkipsAWorkspaceAnotherOperationOwns(t *testing.T) {
	r := newRig(t)
	u, _ := r.stopped("alice")
	g := newGate(r.dock, "StartContainer")
	done := make(chan error, 1)
	go func() { _, err := r.m.Start(ctx, u); done <- err }() // the user powers on again
	g.await(t)

	ok, err := r.m.ReclaimIf(ctx, u, always)
	if err != nil || ok {
		t.Errorf("ReclaimIf during a start = %v, %v, want a quiet skip", ok, err)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := r.ws(u); got.State != store.StateStarting || r.dock.ContainerIDByName(ContainerName("alice")) == "" {
		t.Errorf("the start must be undisturbed: %+v", got)
	}
}

func TestReclaimIfRejectsBadInput(t *testing.T) {
	r := newRig(t)
	u, _ := r.stopped("alice")
	if _, err := r.m.ReclaimIf(ctx, u, nil); err == nil {
		t.Error("a nil policy must be refused, not treated as 'always'")
	}
	if _, err := r.m.ReclaimIf(ctx, store.User{ID: u.ID, Username: "../x"}, always); !errors.Is(err, ErrBadUsername) {
		t.Errorf("bad username: %v", err)
	}
}

// Two users' workspaces are independent: reclaiming one touches nothing of the other.
func TestReclaimIfTouchesOnlyThatUsersWorkspace(t *testing.T) {
	r := newRig(t)
	alice, _ := r.stopped("alice")
	bob, bobID := r.stopped("bob")
	bobBefore := r.ws(bob)
	if ok, err := r.m.ReclaimIf(ctx, alice, always); err != nil || !ok {
		t.Fatalf("ReclaimIf = %v, %v", ok, err)
	}
	if got := r.ws(bob); !reflect.DeepEqual(got, bobBefore) || r.dock.ContainerIDByName(ContainerName("bob")) != bobID {
		t.Errorf("bob's workspace changed: %+v", got)
	}
}

func TestReconcileFollowsDockerWithoutSamplingResources(t *testing.T) {
	r := newRig(t)
	alice, aliceID := r.startRunning("alice")
	bob, bobID := r.startRunning("bob")
	r.dock.SetStats(aliceID, docker.Stats{CPUPercent: 50})

	samples := r.dock.CallCount("ContainerStats")
	st, err := r.m.Reconcile(ctx, alice)
	if err != nil || st.Workspace.State != store.StateRunning || st.Stats != nil {
		t.Fatalf("Reconcile = %+v, %v, want running without a sample", st, err)
	}
	if n := r.dock.CallCount("ContainerStats") - samples; n != 0 {
		t.Errorf("Reconcile took %d resource samples", n)
	}
	if st, err := r.m.Status(ctx, alice); err != nil || st.Stats == nil || st.Stats.CPUPercent != 50 {
		t.Errorf("Status still samples: %+v, %v", st, err)
	}

	r.dock.Crash(aliceID, 137)
	r.dock.Vanish(bobID)
	if st, err := r.m.Reconcile(ctx, alice); err != nil || st.Workspace.State != store.StateStopped {
		t.Errorf("a crashed container: %+v, %v", st.Workspace, err)
	}
	if st, err := r.m.Reconcile(ctx, bob); err != nil || st.Workspace.State != store.StateOffline || st.Workspace.UDPPortStart != 0 {
		t.Errorf("a vanished container: %+v, %v", st.Workspace, err)
	}
}

// A container started behind Berth's back carries the last activity of a session
// that ended long ago; the idle clock must start when Berth notices it.
func TestResumedBehindBacksBackStartsTheIdleClockAgain(t *testing.T) {
	r := newRig(t)
	u, id := r.stopped("alice")
	old := r.ws(u).LastActiveAt
	r.clock.Advance(48 * time.Hour)
	if err := r.dock.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}

	if st, err := r.m.Reconcile(ctx, u); err != nil || st.Workspace.State != store.StateStarting {
		t.Fatalf("Reconcile = %+v, %v", st.Workspace, err)
	}
	ws := r.ws(u)
	if ws.LastActiveAt == nil || !ws.LastActiveAt.Equal(r.clock.Now()) || ws.LastActiveAt.Equal(*old) {
		t.Errorf("last_active_at = %v, want now (%v), not the old %v", ws.LastActiveAt, r.clock.Now(), old)
	}
}

// While the reaper removes the container of a stopped workspace, the user's own
// requests behave sensibly: a power-off just reports (the workspace is powered off
// either way), a power-on is refused as busy and succeeds on the retry.
func TestUsersRequestsDuringAReclaim(t *testing.T) {
	r := newRig(t)
	u, _ := r.stopped("alice")
	g := newGate(r.dock, "RemoveContainer")
	done := make(chan error, 1)
	go func() { _, err := r.m.ReclaimIf(ctx, u, always); done <- err }()
	g.await(t) // the reaper holds the claim, inside Docker

	if st, err := r.m.Stop(ctx, u); err != nil || st.Workspace.State != store.StateStopped {
		t.Errorf("Stop during a reclaim = %+v, %v, want the workspace reported as it is", st.Workspace, err)
	}
	if _, err := r.m.Start(ctx, u); !errors.Is(err, ErrBusy) {
		t.Errorf("Start during a reclaim = %v, want ErrBusy", err)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ws := r.ws(u); ws.State != store.StateOffline {
		t.Fatalf("workspace = %+v, want the reclaim to have finished", ws)
	}
	// The retry works: a new container.
	if _, err := r.m.Start(ctx, u); err != nil {
		t.Fatalf("Start after the reclaim: %v", err)
	}
	if r.ws(u).State != store.StateStarting || r.containerID("alice") == "" {
		t.Errorf("workspace = %+v", r.ws(u))
	}
}

// And during an idle stop (same claim name as the user's own power-off).
func TestUsersRequestsDuringAnIdleStop(t *testing.T) {
	r := newRig(t)
	u, _ := r.startRunning("alice")
	g := newGate(r.dock, "StopContainer")
	done := make(chan error, 1)
	go func() { _, err := r.m.StopIfIdle(ctx, u, always); done <- err }()
	g.await(t)

	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Errorf("Stop during an idle stop: %v, want a plain report", err)
	}
	if _, err := r.m.Start(ctx, u); !errors.Is(err, ErrBusy) {
		t.Errorf("Start during an idle stop = %v, want ErrBusy", err)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.ws(u).State != store.StateStopped {
		t.Errorf("workspace = %+v", r.ws(u))
	}
}

// Something else changes the row while the container is being removed (a second
// backend process, an operator with a SQL shell): the transition applies only to
// the container this reclaim removed, and says nothing was reclaimed.
func TestReclaimIfDoesNotOverwriteARowThatMovedOnMeanwhile(t *testing.T) {
	r := newRig(t)
	u, _ := r.stopped("alice")
	r.dock.SetHook("RemoveContainer", func(context.Context) {
		r.dock.SetHook("RemoveContainer", nil)
		if _, err := r.st.TransitionWorkspace(ctx, u.ID, []store.State{store.StateStopped}, store.StateStopped, r.clock.Now(),
			store.SetContainerID("another-container")); err != nil {
			t.Error(err)
		}
	})
	ok, err := r.m.ReclaimIf(ctx, u, always)
	if err != nil || ok {
		t.Fatalf("ReclaimIf = %v, %v, want a plain false", ok, err)
	}
	if ws := r.ws(u); ws.State != store.StateStopped || ws.ContainerID != "another-container" {
		t.Errorf("the row that moved on was overwritten: %+v", ws)
	}
}
