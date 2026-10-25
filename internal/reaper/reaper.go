// Package reaper runs Berth's periodic reclaim job. Every interval it looks at
// every workspace and:
//
//  1. brings the database in line with Docker (a crashed container is stopped, a
//     container removed by hand is offline);
//  2. stops a workspace that is up and has been unused for the idle timeout;
//  3. removes the container of a workspace that has been stopped for the reclaim
//     age (its data volume is never touched);
//  4. reports containers Berth created that the database does not account for (it
//     never removes them: a container is replaced at its user's next power-on, and
//     one Berth cannot attribute may belong to a database that was wiped).
//
// A second loop keeps Traefik attached to the workspaces that are up.
//
// "In use" is what the backend can see: the heartbeat the web page sends while it
// is open, and CPU load. The WebSockets of ttyd and neko are not visible to the
// backend, so a workspace whose page is closed counts as unused once its
// heartbeats stop, unless it is busy: a running workspace whose average CPU use
// between two looks was at or above the threshold is in use, so a build or a long
// job is not killed. The CPU verdict is kept here, in memory, and not written to
// the database: last_active_at stays "the user was here", the restart floor below
// covers the memory being lost, and a busy workspace costs no write per pass.
//
// The policy (policy.go) decides from a snapshot; the workspace Manager repeats
// the question under the user's claim before it acts, so a heartbeat, a power-off
// or a power-on that lands in between always wins. Nobody is stopped on a guess:
// a CPU reading that is missing or cannot be compared postpones the decision.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

// Workspaces is what the reaper needs from the workspace manager.
type Workspaces interface {
	Reconcile(ctx context.Context, user store.User) (workspace.Status, error)
	StopIfIdle(ctx context.Context, user store.User, idle func(store.Workspace) bool) (bool, error)
	ReclaimIf(ctx context.Context, user store.User, reclaimable func(store.Workspace) bool) (bool, error)
	ReattachTraefik(ctx context.Context) error
}

// Lister is the part of the store the reaper reads.
type Lister interface {
	ListWorkspaces(ctx context.Context) ([]store.UserWorkspace, error)
}

// Docker is the part of the Docker client the reaper reads.
type Docker interface {
	CPUUsage(ctx context.Context, id string) (docker.CPUUsage, error)
	ManagedContainers(ctx context.Context) ([]docker.ContainerInfo, error)
}

// Config holds the reaper's timings and thresholds.
type Config struct {
	IdleTimeout  time.Duration // a workspace that is up and unused this long is stopped
	ReclaimAfter time.Duration // a workspace stopped this long loses its container
	Interval     time.Duration // between the end of one look at all workspaces and the start of the next
	// AttachInterval is how often Traefik is re-attached to the workspaces that
	// are up. It bounds how long they are unreachable after Traefik is recreated.
	AttachInterval time.Duration
	// IdleCPUPercent: an average at or above this between two looks counts as use
	// (100 = one core). Zero ignores CPU.
	IdleCPUPercent float64
	// Concurrency bounds how many workspaces are handled at once (default 4).
	Concurrency int
	// OpTimeout bounds everything done for one workspace in one pass (default 5
	// minutes). It must exceed the workspace stop timeout. A Docker call that never
	// returns would otherwise pin a worker, and the user's claim with it: their next
	// power-on would answer 409 until it did.
	OpTimeout time.Duration
}

// Validate rejects a Config that would spin or stop everything at once.
func (c Config) Validate() error {
	switch {
	case c.IdleTimeout <= 0 || c.ReclaimAfter <= 0:
		return fmt.Errorf("reaper: idle timeout and reclaim age must be positive")
	case c.Interval <= 0 || c.AttachInterval <= 0:
		return fmt.Errorf("reaper: intervals must be positive")
	case !(c.IdleCPUPercent >= 0):
		return fmt.Errorf("reaper: idle CPU threshold must not be negative")
	case c.Concurrency < 0 || c.OpTimeout < 0:
		return fmt.Errorf("reaper: concurrency and operation timeout must not be negative")
	}
	return nil
}

const (
	// defaultReconcileTimeout bounds the one Docker inspection every workspace gets
	// per pass. A daemon that accepts the connection and never answers would
	// otherwise hold a worker for the whole OpTimeout, per workspace.
	defaultReconcileTimeout = 30 * time.Second
	// unknownWarnAfter is how many looks in a row may fail to produce a CPU verdict
	// for a container before the operator is told that idle stopping is suspended.
	unknownWarnAfter = 3
	// clockStepTolerance is how far the wall clock may disagree with the monotonic
	// one between two passes before the reaper assumes the machine slept or the
	// clock was set, and starts the restart grace over.
	clockStepTolerance = 30 * time.Second
)

// Reaper is the periodic job. Build it with New; run it with Run.
type Reaper struct {
	store Lister
	ws    Workspaces
	dock  Docker
	cfg   Config
	log   *slog.Logger
	now   func() time.Time

	reconcileTimeout time.Duration // defaultReconcileTimeout; a field so a test can shorten it

	mu sync.Mutex
	// floor is the earliest moment any activity can be taken to have begun: the
	// process start, moved forward when the clock steps (see lastActivity). It
	// gives a restarted backend a full idle timeout to receive heartbeats again
	// before it stops anyone.
	floor   time.Time
	cpu     map[string]docker.CPUUsage // previous cumulative reading per container ID
	busyAt  map[string]time.Time       // when a container was last seen busy
	busyNow map[string]bool            // the last verdict, to log only changes
	unknown map[string]int             // consecutive looks without a verdict
	warned  map[string]bool            // unaccounted containers already reported
	lastSum string                     // the last pass's failure class, to log only changes
	lastAtt string                     // same for the re-attach loop
}

// New builds a Reaper. now may be nil (time.Now).
func New(st Lister, ws Workspaces, dc Docker, cfg Config, log *slog.Logger, now func() time.Time) (*Reaper, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 4
	}
	if cfg.OpTimeout == 0 {
		cfg.OpTimeout = 5 * time.Minute
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reaper{
		store: st, ws: ws, dock: dc, cfg: cfg, log: log, now: now,
		reconcileTimeout: defaultReconcileTimeout,
		floor:            now(),
		cpu:              map[string]docker.CPUUsage{},
		busyAt:           map[string]time.Time{},
		busyNow:          map[string]bool{},
		unknown:          map[string]int{},
		warned:           map[string]bool{},
	}, nil
}

// Run starts the sweep loop and the Traefik attach loop and returns when both
// have ended, i.e. after ctx is cancelled.
func (r *Reaper) Run(ctx context.Context) {
	r.log.Info("reaper started", "idle_timeout", r.cfg.IdleTimeout, "reclaim_after", r.cfg.ReclaimAfter,
		"interval", r.cfg.Interval, "idle_cpu_percent", r.cfg.IdleCPUPercent)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.attachLoop(ctx) }()
	go func() { defer wg.Done(); r.sweepLoop(ctx) }()
	wg.Wait()
}

// sweepLoop looks at all workspaces one interval after the end of the previous
// look, and for the first time one interval after start: a restarted backend gets
// that long to receive heartbeats and for Docker to settle. A pass that overruns
// the interval therefore delays the next one instead of being followed by an
// immediate second.
func (r *Reaper) sweepLoop(ctx context.Context) {
	prev := time.Now()
	for {
		timer := time.NewTimer(r.cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		cur := time.Now()
		// Round(0) drops the monotonic reading: the difference of the two is how far
		// the wall clock moved against the clock that does not jump.
		r.tick(ctx, cur.Round(0).Sub(prev.Round(0)), cur.Sub(prev))
		prev = time.Now()
	}
}

// tick is one beat of the sweep loop: the wall clock and the monotonic clock have
// advanced by wall and mono since the previous beat. If they disagree the machine
// slept or the clock was set: every heartbeat that was due in the meantime was
// missed, so everyone gets a fresh timeout (the floor moves to now), exactly as
// after a restart. Then it looks at the workspaces; a panic costs one pass, not
// the backend.
func (r *Reaper) tick(ctx context.Context, wall, mono time.Duration) {
	if clockStepped(wall, mono, clockStepTolerance) {
		r.log.Warn("the clock stepped; nobody is stopped for the idle timeout from now",
			"wall", wall, "monotonic", mono)
		r.raiseFloor(r.now())
	}
	r.safely("sweep", func() { r.Sweep(ctx) })
}

// clockStepped reports whether the wall clock and the monotonic clock disagree by
// more than tol over the same interval.
func clockStepped(wall, mono, tol time.Duration) bool {
	d := wall - mono
	return d > tol || d < -tol
}

func (r *Reaper) raiseFloor(t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.After(r.floor) {
		r.floor = t
	}
}

// floorFor is the earliest moment the container's workspace can be taken to have
// been in use: never before the floor, nor before the container was last busy.
func (r *Reaper) floorFor(containerID string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.busyAt[containerID]; b.After(r.floor) {
		return b
	}
	return r.floor
}

// safely runs fn and logs a panic instead of letting it end the process: the
// reaper runs outside the HTTP server's recoverer, and an unexpected reply from
// Docker must cost one pass, not the whole backend.
func (r *Reaper) safely(what string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("reaper: panic", "during", what, "panic", p)
		}
	}()
	fn()
}

// attachLoop reconnects Traefik to the workspaces that are up at startup and then
// periodically. Docker drops those attachments whenever the Traefik container is
// recreated; without this every workspace would answer 504 until its owner
// happened to restart it. A failure is logged when it first appears and again
// when it clears, not every interval. It is a loop of its own, not part of the
// sweep: a sweep can take a while, and this one bounds the outage after a
// Traefik redeploy (measured: 42 s with a 60 s interval, 5 s with 10 s).
func (r *Reaper) attachLoop(ctx context.Context) {
	tick := time.NewTicker(r.cfg.AttachInterval)
	defer tick.Stop()
	for {
		r.attachOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (r *Reaper) attachOnce(ctx context.Context) {
	var err error
	r.safely("attach", func() {
		attachCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err = r.ws.ReattachTraefik(attachCtx)
	})
	if ctx.Err() != nil {
		return // shutting down: not a failure of the proxy
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err != nil && err.Error() != r.lastAtt:
		r.log.Warn("could not attach the proxy to user networks", "err", err)
		r.lastAtt = err.Error()
	case err == nil && r.lastAtt != "":
		r.log.Info("proxy attached to all user networks again")
		r.lastAtt = ""
	}
}

// failure is one workspace the pass could not look after.
type failure struct {
	user string // "" for a failure that is not about one workspace
	err  error
}

// outcome counts what one pass did.
type outcome struct {
	mu                 sync.Mutex
	stopped, reclaimed int
	failures           []failure
	// abort is set when Docker cannot be reached or does not answer: the rest of
	// the pass would only fail the same way, workspace after workspace.
	abort atomic.Bool
}

func (o *outcome) fail(user string, err error) {
	o.mu.Lock()
	o.failures = append(o.failures, failure{user, err})
	o.mu.Unlock()
	if docker.IsUnavailable(err) || errors.Is(err, context.DeadlineExceeded) {
		o.abort.Store(true)
	}
}

func (o *outcome) count(stopped, reclaimed int) {
	o.mu.Lock()
	o.stopped += stopped
	o.reclaimed += reclaimed
	o.mu.Unlock()
}

// Sweep looks at every workspace once. Failures of single workspaces are
// collected, not fatal: one user's stuck container must not stop everyone else
// from being looked after.
func (r *Reaper) Sweep(ctx context.Context) {
	var out outcome
	rows, err := r.store.ListWorkspaces(ctx)
	if err != nil {
		if ctx.Err() == nil {
			out.fail("", fmt.Errorf("list workspaces: %w", err))
			r.report(&out)
		}
		return
	}
	r.forEach(ctx, rows, &out, func(uw store.UserWorkspace) { r.handle(ctx, uw, &out) })
	if ctx.Err() != nil {
		return // shutting down: whatever was cut short is picked up by the next backend
	}
	r.forgetAllButUp(rows)

	// Containers Berth created that the database does not account for. The list
	// comes after the pass (a container the pass just removed must not be in it) and
	// the rows are read again straight after it (a container recorded since the
	// pass began must be in them); between those two reads only an operator can
	// remove a container.
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	containers, cerr := r.dock.ManagedContainers(listCtx)
	cancel()
	if cerr != nil {
		out.fail("", fmt.Errorf("list containers: %w", cerr))
	} else if fresh, err := r.store.ListWorkspaces(ctx); err == nil {
		r.reportUnaccounted(fresh, containers)
	}
	r.log.Debug("reaper pass done", "workspaces", len(rows), "stopped", out.stopped, "reclaimed", out.reclaimed,
		"failures", len(out.failures))
	r.report(&out)
}

// forEach runs fn for every workspace that has anything to look at, at most
// Concurrency at a time. A panic in fn is logged and does not take the backend
// down. It stops dispatching when ctx ends or Docker has become unreachable.
func (r *Reaper) forEach(ctx context.Context, rows []store.UserWorkspace, out *outcome, fn func(store.UserWorkspace)) {
	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
dispatch:
	for _, uw := range rows {
		if ctx.Err() != nil || out.abort.Load() {
			break
		}
		if uw.Workspace.State == store.StateOffline && uw.Workspace.ContainerID == "" {
			continue // nothing to look at
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			r.safely("workspace "+uw.User.Username, func() { fn(uw) })
		}()
	}
	wg.Wait()
}

// handle looks after one workspace. A shutdown cancels the Docker call in
// progress; nothing is lost by that: a stop the daemon completes anyway is
// noticed by the next Reconcile (the container exited), and so is a removal.
func (r *Reaper) handle(ctx context.Context, uw store.UserWorkspace, out *outcome) {
	if ctx.Err() != nil || out.abort.Load() {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, r.cfg.OpTimeout)
	defer cancel()
	user := uw.User

	recCtx, recCancel := context.WithTimeout(opCtx, r.reconcileTimeout)
	st, err := r.ws.Reconcile(recCtx, user)
	recCancel()
	if err != nil {
		out.fail(user.Username, fmt.Errorf("reconcile: %w", err))
		return
	}
	ws := st.Workspace
	if ws.ContainerID == "" {
		return // offline, or abandoned: Reconcile has dealt with it
	}

	switch ws.State {
	case store.StateRunning:
		// The CPU rule applies to a running workspace only: that is where a user's
		// job can be. A start that never turns healthy is judged by its heartbeat.
		if v := r.cpuVerdict(opCtx, user.Username, ws); v == busy || v == unknown {
			return
		}
		r.stopIfIdle(opCtx, user, ws, out)
	case store.StateStarting:
		r.stopIfIdle(opCtx, user, ws, out)
	case store.StateStopped:
		if !Reclaimable(ws, r.now(), r.cfg.ReclaimAfter) {
			return
		}
		ok, err := r.ws.ReclaimIf(opCtx, user, func(w store.Workspace) bool {
			return Reclaimable(w, r.now(), r.cfg.ReclaimAfter)
		})
		switch {
		case err != nil:
			out.fail(user.Username, fmt.Errorf("reclaim: %w", err))
		case ok:
			out.count(0, 1)
		}
	}
}

// stopIfIdle stops a workspace that is up if it has been unused for the idle
// timeout. The decision is repeated under the user's claim by the Manager, against
// the workspace as it is then.
func (r *Reaper) stopIfIdle(ctx context.Context, user store.User, ws store.Workspace, out *outcome) {
	idle := func(w store.Workspace) bool {
		return Idle(w, r.now(), r.floorFor(ws.ContainerID), r.cfg.IdleTimeout)
	}
	if !idle(ws) {
		return
	}
	idleFor := r.now().Sub(lastActivity(ws, r.floorFor(ws.ContainerID)))
	stopped, err := r.ws.StopIfIdle(ctx, user, idle)
	switch {
	case err != nil:
		out.fail(user.Username, fmt.Errorf("stop idle workspace: %w", err))
	case stopped:
		out.count(1, 0)
		r.log.Info("stopped an idle workspace; its container and data are kept",
			"user", user.Username, "idle_for", idleFor.Round(time.Second), "state", ws.State)
	}
}

// verdict is what the CPU reading says about a running workspace.
type verdict int

const (
	quiet   verdict = iota // below the threshold (or the check is off): the heartbeat rule decides
	busy                   // at or above it: in use
	unknown                // no usable answer this time: decide nothing
)

// cpuVerdict compares the container's cumulative CPU time with the previous look
// and says whether its average load since then reached the threshold. It is
// unknown at the first look at a container (nothing to compare with), when the
// reading fails, and when two readings cannot be compared (the container
// restarted). A busy container is remembered as used at this moment.
func (r *Reaper) cpuVerdict(ctx context.Context, username string, ws store.Workspace) verdict {
	if r.cfg.IdleCPUPercent == 0 {
		return quiet
	}
	id := ws.ContainerID
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cur, err := r.dock.CPUUsage(readCtx, id)
	if err != nil {
		r.log.Debug("could not read the container's CPU use", "user", username, "err", err)
		return r.noVerdict(username, "the reading failed: "+err.Error(), id)
	}
	r.mu.Lock()
	prev, had := r.cpu[id]
	r.cpu[id] = cur
	r.mu.Unlock()
	if !had {
		return r.noVerdict(username, "", id) // the first look: nothing to compare with yet
	}
	pct, ok := CPUPercent(prev, cur)
	if !ok {
		r.log.Debug("two CPU readings cannot be compared", "user", username, "prev", prev, "cur", cur)
		return r.noVerdict(username, "the readings cannot be compared", id)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.unknown, id)
	if pct >= r.cfg.IdleCPUPercent {
		r.busyAt[id] = r.now()
		if !r.busyNow[id] {
			r.busyNow[id] = true
			r.log.Info("workspace is busy; it is not stopped while it is",
				"user", username, "cpu_percent", fmt.Sprintf("%.1f", pct), "threshold", r.cfg.IdleCPUPercent)
		}
		return busy
	}
	if r.busyNow[id] {
		r.busyNow[id] = false
		r.log.Info("workspace is no longer busy; the idle timeout counts from now",
			"user", username, "cpu_percent", fmt.Sprintf("%.1f", pct))
	}
	return quiet
}

// noVerdict records one more look without a verdict for the container and
// returns unknown. When it keeps happening the operator is told once, because the
// effect is silent and large: idle stopping does nothing for that workspace. The
// first look at a container is no reason to warn (why is empty).
func (r *Reaper) noVerdict(username, why, id string) verdict {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unknown[id]++
	if r.unknown[id] == unknownWarnAfter && why != "" {
		r.log.Warn("no usable CPU reading for a running workspace; it is not idle-stopped until there is one "+
			"(set BERTH_IDLE_CPU_PERCENT=0 to ignore CPU load)", "user", username, "looks", unknownWarnAfter, "why", why)
	}
	return unknown
}

// forgetAllButUp drops what is remembered about containers that the workspaces
// read at the start of the pass do not show as up (stopped, removed, offline).
// A workspace the pass did not get to, because Docker was unreachable or its
// inspection failed, keeps its memory: forgetting a busy container's CPU baseline
// would let the next pass see an idle workspace where there was a busy one.
func (r *Reaper) forgetAllButUp(rows []store.UserWorkspace) {
	up := make(map[string]bool, len(rows))
	for _, uw := range rows {
		if st := uw.Workspace.State; (st == store.StateRunning || st == store.StateStarting) && uw.Workspace.ContainerID != "" {
			up[uw.Workspace.ContainerID] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.cpu {
		if !up[id] {
			delete(r.cpu, id)
		}
	}
	for id := range r.busyAt {
		if !up[id] {
			delete(r.busyAt, id)
		}
	}
	for id := range r.busyNow {
		if !up[id] {
			delete(r.busyNow, id)
		}
	}
	for id := range r.unknown {
		if !up[id] {
			delete(r.unknown, id)
		}
	}
}

// reportUnaccounted logs, once per container, each container carrying Berth's
// labels that the database does not record: one of a user the database does not
// know (a wiped database), or an unrecorded one of a known user (the leftover of
// a start that died; it is replaced at that user's next power-on). It removes
// nothing: labels are not proof of origin (a committed image keeps them), a
// stopped leftover holds no memory or port, and a running one may be in use. The
// set of reported containers is pruned to the ones that still exist.
func (r *Reaper) reportUnaccounted(rows []store.UserWorkspace, containers []docker.ContainerInfo) {
	recorded := make(map[string]bool, len(rows))
	known := make(map[string]bool, len(rows))
	for _, uw := range rows {
		known[uw.User.Username] = true
		if uw.Workspace.ContainerID != "" {
			recorded[uw.Workspace.ContainerID] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	present := make(map[string]bool, len(containers))
	for _, c := range containers {
		present[c.ID] = true
		if recorded[c.ID] || r.warned[c.ID] {
			continue
		}
		r.warned[c.ID] = true
		user := c.Labels[docker.LabelUser]
		msg := "a container labelled as Berth's is not recorded in the database; it will be replaced at its user's next power-on"
		if !known[user] {
			msg = "a container labelled as Berth's belongs to a user the database does not know; leaving it alone"
		}
		r.log.Warn(msg, "container", c.Name, "id", c.ID, "user", user, "status", c.Status)
	}
	for id := range r.warned {
		if !present[id] {
			delete(r.warned, id)
		}
	}
}

// failureClass groups failures that mean the same thing for the operator, so a
// steady outage is one log line and not one per pass or per workspace.
func failureClass(err error) string {
	switch {
	case docker.IsUnavailable(err):
		return "the Docker daemon is unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "a Docker call timed out"
	}
	return err.Error()
}

// report logs the failures of a pass once per change of kind.
func (r *Reaper) report(out *outcome) {
	out.mu.Lock()
	fs := append([]failure(nil), out.failures...)
	out.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(fs) == 0 {
		if r.lastSum != "" {
			r.log.Info("reaper: running normally again")
			r.lastSum = ""
		}
		return
	}
	// Whichever workspace happens to fail first must not decide what is "new".
	sort.Slice(fs, func(i, j int) bool { return fs[i].user < fs[j].user })
	class := failureClass(fs[0].err)
	if class != r.lastSum {
		r.log.Warn("reaper: could not look after every workspace", "failures", len(fs), "first_user", fs[0].user, "first", fs[0].err)
		r.lastSum = class
	}
}
