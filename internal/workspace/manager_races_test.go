package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

// gate blocks the first call of a Fake method until release is closed, so a test
// can run other requests while one is in the middle of its Docker work.
type gate struct {
	reached chan struct{}
	release chan struct{}
	armed   atomic.Bool
}

func newGate(f *docker.Fake, method string) *gate {
	g := &gate{reached: make(chan struct{}), release: make(chan struct{})}
	f.SetHook(method, func(context.Context) {
		if g.armed.CompareAndSwap(false, true) {
			close(g.reached)
			<-g.release
		}
	})
	return g
}

// afterGate holds the first InspectContainer call AFTER it has its answer, so the
// caller keeps an observation while the world moves on.
func newAfterGate(f *docker.Fake) *gate {
	g := &gate{reached: make(chan struct{}), release: make(chan struct{})}
	f.SetAfterHook("InspectContainer", func(context.Context) {
		if g.armed.CompareAndSwap(false, true) {
			close(g.reached)
			<-g.release
		}
	})
	return g
}

type startResult struct {
	st  Status
	err error
}

func (r *rig) startAsync(u store.User) <-chan startResult {
	ch := make(chan startResult, 1)
	go func() {
		s, err := r.m.Start(ctx, u)
		ch <- startResult{s, err}
	}()
	return ch
}

// running brings a user's workspace to `running` and returns its container ID.
func (r *rig) running(u store.User) string {
	r.t.Helper()
	r.mustStart(u)
	id := r.containerID(u.Username)
	r.dock.SetHealth(id, docker.HealthHealthy)
	if s, err := r.m.Status(ctx, u); err != nil || s.Workspace.State != store.StateRunning {
		r.t.Fatalf("workspace not running: %v %v", s.Workspace.State, err)
	}
	return id
}

// abandoned leaves a user's row the way a backend that died mid-start does: the
// claim taken, a UDP block allocated, no container recorded.
func (r *rig) abandoned(u store.User) {
	r.t.Helper()
	if _, err := r.st.TransitionWorkspace(ctx, u.ID, Sources(EventStart), store.StateStarting, t0); err != nil {
		r.t.Fatal(err)
	}
	if _, _, err := r.st.AllocateUDPRange(ctx, u.ID, 30000, 30031, 8, t0); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) orphan(user string) string {
	r.t.Helper()
	id, err := r.dock.CreateContainer(ctx, docker.ContainerSpec{
		Name: ContainerName(user), Image: "x",
		Labels: map[string]string{docker.LabelManaged: "true", docker.LabelUser: user},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func TestStatusDuringTheRestartOfAKeptContainerDoesNotUndoIt(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	id := r.running(u)
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	startsBefore := r.dock.CallCount("StartContainer")

	g := newGate(r.dock, "StartContainer")
	first := r.startAsync(u)
	<-g.reached // blocked in docker start: the kept container is still "exited"

	for i := 0; i < 3; i++ {
		s, err := r.m.Status(ctx, u)
		if err != nil || s.Workspace.State != store.StateStarting {
			t.Fatalf("Status during the start = %v, %v; want starting (an exited container is not a crash yet)", s.Workspace.State, err)
		}
	}
	// A double-clicked "Power on" reports the state and does no second round of work.
	s, err := r.m.Start(ctx, u)
	if err != nil || s.Workspace.State != store.StateStarting {
		t.Fatalf("second Start = %v, %v", s.Workspace.State, err)
	}
	close(g.release)
	if res := <-first; res.err != nil {
		t.Fatal(res.err)
	}
	if n := r.dock.CallCount("StartContainer") - startsBefore; n != 1 {
		t.Fatalf("docker start ran %d times, want exactly 1", n)
	}
	if ws := r.ws(u); ws.State != store.StateStarting || ws.ContainerID != id {
		t.Fatalf("row after the start: %+v", ws)
	}
	if info, _ := r.dock.InspectContainer(ctx, id); !info.Running {
		t.Fatal("the container is not running")
	}
}

func TestConcurrentStartsOfAStoppedWorkspaceStartTheContainerOnce(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.running(u)
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	startsBefore := r.dock.CallCount("StartContainer")

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.m.Start(ctx, u); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Start: %v", err)
	}
	if n := r.dock.CallCount("StartContainer") - startsBefore; n != 1 {
		t.Fatalf("docker start ran %d times for 12 simultaneous starts, want 1", n)
	}
}

func TestStatusDuringAStartDoesNotDeclareTheKeptContainerGone(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	id := r.running(u)
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	r.dock.Vanish(id) // removed by hand while stopped
	before := r.ws(u)

	g := newGate(r.dock, "EnsureVolume")
	first := r.startAsync(u)
	<-g.reached
	s, err := r.m.Status(ctx, u)
	if err != nil || s.Workspace.State != store.StateStarting || s.Workspace.ContainerID != id ||
		s.Workspace.UDPPortStart != before.UDPPortStart {
		t.Fatalf("Status during the start: %+v, %v; the start's claim and ports must survive", s.Workspace, err)
	}
	close(g.release)
	if res := <-first; res.err != nil {
		t.Fatal(res.err)
	}
	ws := r.ws(u)
	if ws.State != store.StateStarting || ws.ContainerID == "" || ws.ContainerID == id || ws.UDPPortStart != before.UDPPortStart {
		t.Fatalf("row after the start: %+v", ws)
	}
}

func TestStopDuringAStartIsBusyAndTheStartSurvives(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	g := newGate(r.dock, "CreateContainer")
	first := r.startAsync(u)
	<-g.reached
	if _, err := r.m.Stop(ctx, u); !errors.Is(err, ErrBusy) {
		t.Fatalf("Stop during a start = %v, want ErrBusy", err)
	}
	close(g.release)
	if res := <-first; res.err != nil {
		t.Fatal(res.err)
	}
	id := r.containerID("alice")
	if info, _ := r.dock.InspectContainer(ctx, id); !info.Running || r.ws(u).State != store.StateStarting {
		t.Fatalf("the start did not complete: running=%v row=%+v", info.Running, r.ws(u))
	}
	// Once it has finished, power off works.
	if s, err := r.m.Stop(ctx, u); err != nil || s.Workspace.State != store.StateStopped {
		t.Fatalf("Stop after the start = %v, %v", s.Workspace.State, err)
	}
}

func TestStartDuringAStopIsBusyAndAnotherStopIsIdempotent(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.running(u)

	g := newGate(r.dock, "StopContainer")
	stopped := make(chan startResult, 1)
	go func() { s, err := r.m.Stop(ctx, u); stopped <- startResult{s, err} }()
	<-g.reached

	if _, err := r.m.Start(ctx, u); !errors.Is(err, ErrBusy) {
		t.Fatalf("Start during a stop = %v, want ErrBusy", err)
	}
	if s, err := r.m.Stop(ctx, u); err != nil || s.Workspace.State != store.StateRunning {
		t.Fatalf("second Stop = %v, %v; it reports the state and waits for the first", s.Workspace.State, err)
	}
	close(g.release)
	if res := <-stopped; res.err != nil || res.st.Workspace.State != store.StateStopped {
		t.Fatalf("first Stop = %v, %v", res.st.Workspace.State, res.err)
	}
}

func TestAbandonedStartIsRecoveredByStatus(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.abandoned(u) // the previous process died before recording a container

	s, err := r.m.Status(ctx, u)
	if err != nil || s.Workspace.State != store.StateOffline {
		t.Fatalf("Status = %v, %v; want offline", s.Workspace.State, err)
	}
	if ws := r.ws(u); ws.State != store.StateOffline || ws.UDPPortStart != 0 || ws.ContainerID != "" {
		t.Fatalf("row: %+v; the port block must be released", ws)
	}
	r.mustStart(u) // and the user can power on again
}

func TestAbandonedStartRemovesItsOrphanButNotAForeignContainer(t *testing.T) {
	r := newRig(t)
	alice, bob := r.user("alice"), r.user("bob")
	r.abandoned(alice)
	orphan := r.orphan("alice")
	if _, err := r.m.Status(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dock.InspectContainer(ctx, orphan); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("the orphan was not removed: %v", err)
	}
	if ws := r.ws(alice); ws.State != store.StateOffline || ws.UDPPortStart != 0 {
		t.Fatalf("row: %+v", ws)
	}

	// Something else under bob's workspace name is left alone.
	if _, err := r.st.TransitionWorkspace(ctx, bob.ID, Sources(EventStart), store.StateStarting, t0); err != nil {
		t.Fatal(err)
	}
	foreign, _ := r.dock.CreateContainer(ctx, docker.ContainerSpec{Name: ContainerName("bob"), Image: "other"})
	if _, err := r.m.Status(ctx, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dock.InspectContainer(ctx, foreign); err != nil {
		t.Fatalf("a container Berth did not create was removed: %v", err)
	}
}

func TestStopOfAnAbandonedStartRecovers(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.abandoned(u)
	s, err := r.m.Stop(ctx, u)
	if err != nil || s.Workspace.State != store.StateOffline {
		t.Fatalf("Stop = %v, %v; a start that died with the old process must not wedge power-off", s.Workspace.State, err)
	}
}

func TestStartRecoversAnAbandonedStartAndProceeds(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.abandoned(u)
	orphan := r.orphan("alice")
	r.mustStart(u)
	ws := r.ws(u)
	if ws.State != store.StateStarting || ws.ContainerID == "" || ws.ContainerID == orphan {
		t.Fatalf("row: %+v", ws)
	}
	if info, _ := r.dock.InspectContainer(ctx, ws.ContainerID); !info.Running {
		t.Fatal("the new container is not running")
	}
}

func TestRollbackWithAnUnreachableDaemonKeepsTheKeptContainer(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	id := r.running(u)
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	before := r.ws(u)

	// The daemon goes away: the start fails, and so does the rollback's look at
	// the kept container. "I could not ask" is not "it is gone".
	down := fmt.Errorf("dial unix /var/run/docker.sock: %w", docker.ErrUnavailable)
	r.dock.FailNext("EnsureNetwork", down)
	r.dock.FailNext("InspectContainer", down)
	if _, err := r.m.Start(ctx, u); err == nil {
		t.Fatal("Start succeeded with the daemon down")
	}
	ws := r.ws(u)
	if ws.State != store.StateStopped || ws.ContainerID != id || ws.UDPPortStart != before.UDPPortStart || ws.UDPPortStart == 0 {
		t.Fatalf("row after the failed start: %+v (before %+v); the kept container and its ports must be remembered", ws, before)
	}
	// The daemon is back: the same container starts.
	r.mustStart(u)
	if r.ws(u).ContainerID != id {
		t.Fatalf("container id changed to %q", r.ws(u).ContainerID)
	}
}

func TestFailedRemovalOfAHalfCreatedContainerKeepsItsPorts(t *testing.T) {
	r := newRig(t)
	alice, bob := r.user("alice"), r.user("bob")
	down := fmt.Errorf("remove: %w", docker.ErrUnavailable)
	r.dock.FailNext("StartContainer", errors.New("boom"))
	r.dock.FailNext("RemoveContainer", down)
	if _, err := r.m.Start(ctx, alice); err == nil {
		t.Fatal("Start succeeded")
	}
	ws := r.ws(alice)
	if ws.State != store.StateOffline || ws.ContainerID != "" || ws.UDPPortStart == 0 {
		t.Fatalf("row: %+v; the block must stay allocated while its container may still exist", ws)
	}
	leftover := r.containerID("alice")
	if leftover == "" {
		t.Fatal("the fake lost the container the test meant to leave behind")
	}

	// Bob must not be handed the block the leftover still publishes.
	r.mustStart(bob)
	if r.ws(bob).UDPPortStart == ws.UDPPortStart {
		t.Fatalf("bob was given alice's block %d", ws.UDPPortStart)
	}
	// Alice's next start replaces the leftover and keeps her block.
	r.mustStart(alice)
	if after := r.ws(alice); after.UDPPortStart != ws.UDPPortStart || after.ContainerID == leftover {
		t.Fatalf("alice after retry: %+v (leftover %s)", after, leftover)
	}
}

func TestLostCreateResponseLeavesNothingBehind(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	// The daemon created the container, but the reply was lost (the request's
	// context expired): the backend never learned the ID.
	r.dock.LoseNextCreateResponse(context.Canceled)
	if _, err := r.m.Start(ctx, u); err == nil {
		t.Fatal("Start succeeded")
	}
	if id := r.containerID("alice"); id != "" {
		t.Fatalf("container %s was left behind", id)
	}
	if ws := r.ws(u); ws.State != store.StateOffline || ws.UDPPortStart != 0 {
		t.Fatalf("row: %+v", ws)
	}
	r.mustStart(u)
}

func TestStaleKeptContainerIsReplacedKeepingPortsAndPassword(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	old := r.running(u)
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	before := r.ws(u)
	r.dock.FailNext("StartContainer", fmt.Errorf("start: %w", docker.ErrStaleContainer))
	r.mustStart(u)
	ws := r.ws(u)
	if ws.ContainerID == old || ws.ContainerID != r.containerID("alice") {
		t.Fatalf("container id = %q (old %q); want a replacement", ws.ContainerID, old)
	}
	if _, err := r.dock.InspectContainer(ctx, old); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("the stale container still exists: %v", err)
	}
	if ws.UDPPortStart != before.UDPPortStart || ws.NekoPassword != before.NekoPassword {
		t.Fatalf("ports/password changed: %+v vs %+v", ws, before)
	}
}

func TestRunningOutOfDockerAddressSpaceIsACapacityError(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.dock.FailNext("EnsureNetwork", fmt.Errorf("%w: all predefined address pools have been fully subnetted", docker.ErrNoAddressSpace))
	if _, err := r.m.Start(ctx, u); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity (the API answers 503)", err)
	}
	if r.ws(u).State != store.StateOffline {
		t.Fatalf("state = %s", r.ws(u).State)
	}
}

func TestStoppedWorkspaceReportsNoHealth(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.running(u)
	s, err := r.m.Stop(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	// Docker keeps a stopped container's last healthcheck result ("healthy").
	if s.Health != docker.HealthNone {
		t.Fatalf("Stop reports health %q for a stopped workspace", s.Health)
	}
	if s, _ = r.m.Status(ctx, u); s.Health != docker.HealthNone {
		t.Fatalf("Status reports health %q for a stopped workspace", s.Health)
	}
}

func TestRollbackSurvivesTheRequestContextBeingCancelled(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	reqCtx, cancel := context.WithCancel(ctx)
	r.dock.SetHook("StartContainer", func(context.Context) { cancel() }) // the client goes away mid-start
	r.dock.FailNext("StartContainer", context.Canceled)
	if _, err := r.m.Start(reqCtx, u); err == nil {
		t.Fatal("Start succeeded after the client went away")
	}
	if id := r.containerID("alice"); id != "" {
		t.Fatalf("container %s was left behind", id)
	}
	if ws := r.ws(u); ws.State != store.StateOffline || ws.UDPPortStart != 0 {
		t.Fatalf("row: %+v; the rollback must not depend on the dead request context", ws)
	}
}

func TestStartIsBoundedByStartTimeout(t *testing.T) {
	r := newRig(t, func(c *Config) { c.StartTimeout = 50 * time.Millisecond })
	u := r.user("alice")
	// A daemon that hangs until the deadline (or, if the code under test sets
	// none, for three seconds), then answers.
	r.dock.SetHook("EnsureNetwork", func(c context.Context) {
		select {
		case <-c.Done():
		case <-time.After(3 * time.Second):
		}
	})
	start := time.Now()
	_, err := r.m.Start(ctx, u)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the start deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Start did not honour StartTimeout")
	}
	if ws := r.ws(u); ws.State != store.StateOffline {
		t.Fatalf("row: %+v", ws)
	}
}

func TestStopUsesTheConfiguredGracePeriod(t *testing.T) {
	r := newRig(t, func(c *Config) { c.StopTimeout = 7 * time.Second })
	u := r.user("alice")
	r.running(u)
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got := r.dock.LastStopTimeout(); got != 7*time.Second {
		t.Fatalf("docker stop grace period = %v, want 7s (0 would be an immediate SIGKILL)", got)
	}
}

func TestConcurrentStatusCallsAgreeOnReadiness(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	r.mustStart(u)
	r.dock.SetHealth(r.containerID("alice"), docker.HealthHealthy)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := r.m.Status(ctx, u)
			if err == nil && s.Workspace.State != store.StateRunning {
				err = fmt.Errorf("state = %s, want running", s.Workspace.State)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A Status that inspected the container before another request replaced it must
// not apply what it saw to the new container's row.
func TestStaleGoneObservationCannotEraseAReplacementContainer(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	old := r.running(u)
	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	before := r.ws(u)
	r.dock.Vanish(old) // removed by hand

	g := newAfterGate(r.dock)
	done := make(chan error, 1)
	go func() { _, err := r.m.Status(ctx, u); done <- err }()
	<-g.reached // this Status has seen "container gone" and is holding that answer

	r.mustStart(u) // recreates the container...
	replacement := r.ws(u).ContainerID
	if replacement == "" || replacement == old {
		t.Fatalf("no replacement container: %+v", r.ws(u))
	}
	if _, err := r.m.Stop(ctx, u); err != nil { // ...and the user powers off again
		t.Fatal(err)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ws := r.ws(u)
	if ws.State != store.StateStopped || ws.ContainerID != replacement || ws.UDPPortStart != before.UDPPortStart || ws.UDPPortStart == 0 {
		t.Fatalf("row after the stale observation: %+v; it must keep the replacement %s and its ports", ws, replacement)
	}
	if _, err := r.dock.InspectContainer(ctx, replacement); err != nil {
		t.Fatalf("the replacement container was orphaned: %v", err)
	}
}

func TestStaleCrashObservationCannotStopARestartedWorkspace(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	id := r.running(u)
	r.dock.Crash(id, 1)

	g := newAfterGate(r.dock)
	done := make(chan error, 1)
	go func() { _, err := r.m.Status(ctx, u); done <- err }()
	<-g.reached // this Status has seen "exited" while the row says running

	if _, err := r.m.Stop(ctx, u); err != nil {
		t.Fatal(err)
	}
	r.mustStart(u)
	r.dock.SetHealth(id, docker.HealthHealthy)
	if s, _ := r.m.Status(ctx, u); s.Workspace.State != store.StateRunning {
		t.Fatalf("workspace not running again: %s", s.Workspace.State)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ws := r.ws(u); ws.State != store.StateRunning {
		t.Fatalf("state = %s; an observation made before the restart must not stop the running workspace", ws.State)
	}
}

// A panic in the middle of an operation (the HTTP layer recovers it and answers
// 500) must release the user's claim, or every later start and stop is refused.
func TestAPanicDoesNotWedgeTheWorkspace(t *testing.T) {
	r := newRig(t)
	u := r.user("alice")
	armed := true
	r.dock.SetHook("CreateContainer", func(context.Context) {
		if armed {
			armed = false
			panic("boom")
		}
	})
	func() {
		defer func() { _ = recover() }()
		r.m.Start(ctx, u)
	}()
	// The row is left at `starting` (the rollback never ran); the next request
	// recovers it instead of being told the workspace is busy.
	if s, err := r.m.Stop(ctx, u); err != nil || s.Workspace.State != store.StateOffline {
		t.Fatalf("Stop after a panicked start = %v, %v", s.Workspace.State, err)
	}
	r.mustStart(u)
}
