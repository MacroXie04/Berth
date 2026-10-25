package reaper

import (
	"testing"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func at(t time.Time) *time.Time { return &t }

func TestIdle(t *testing.T) {
	const timeout = 30 * time.Minute
	running := func(active *time.Time, updated time.Time) store.Workspace {
		return store.Workspace{State: store.StateRunning, LastActiveAt: active, UpdatedAt: updated}
	}
	floor := t0.Add(-100 * time.Hour) // a process that has been up for ages

	cases := []struct {
		name  string
		ws    store.Workspace
		now   time.Time
		floor time.Time
		want  bool
	}{
		{"active a minute ago", running(at(t0), t0), t0.Add(time.Minute), floor, false},
		{"one second short of the timeout", running(at(t0), t0), t0.Add(timeout - time.Second), floor, false},
		{"exactly the timeout", running(at(t0), t0), t0.Add(timeout), floor, true},
		{"well past the timeout", running(at(t0), t0), t0.Add(5 * time.Hour), floor, true},
		{"no heartbeat ever: counted from the last update", running(nil, t0), t0.Add(timeout), floor, true},
		{"no heartbeat ever, recent update", running(nil, t0), t0.Add(time.Minute), floor, false},
		{"the heartbeat wins over an older update", running(at(t0.Add(20*time.Minute)), t0), t0.Add(timeout), floor, false},
		{"clock went backwards", running(at(t0), t0), t0.Add(-time.Hour), floor, false},
		{"no time information at all", running(nil, time.Time{}), t0.Add(100 * time.Hour), time.Time{}, false},
		// The restart floor: a process started 10 minutes ago has seen no heartbeats.
		{"floor: old activity, young process", running(at(t0.Add(-5*time.Hour)), t0), t0.Add(10 * time.Minute), t0, false},
		{"floor: counted from the process start", running(at(t0.Add(-5*time.Hour)), t0), t0.Add(timeout), t0, true},
		{"floor: activity newer than the floor wins", running(at(t0.Add(time.Hour)), t0), t0.Add(time.Hour + time.Minute), t0, false},
		// States.
		{"starting counts (a start that never became healthy)", store.Workspace{State: store.StateStarting, LastActiveAt: at(t0)}, t0.Add(timeout), floor, true},
		{"stopped never", store.Workspace{State: store.StateStopped, LastActiveAt: at(t0)}, t0.Add(100 * time.Hour), floor, false},
		{"offline never", store.Workspace{State: store.StateOffline, LastActiveAt: at(t0)}, t0.Add(100 * time.Hour), floor, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Idle(tc.ws, tc.now, tc.floor, timeout); got != tc.want {
				t.Errorf("Idle = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReclaimable(t *testing.T) {
	const after = 7 * 24 * time.Hour
	stopped := func(stoppedAt *time.Time, updated time.Time) store.Workspace {
		return store.Workspace{State: store.StateStopped, ContainerID: "c1", LastStoppedAt: stoppedAt, UpdatedAt: updated}
	}
	cases := []struct {
		name string
		ws   store.Workspace
		now  time.Time
		want bool
	}{
		{"just stopped", stopped(at(t0), t0), t0.Add(time.Minute), false},
		{"a second short", stopped(at(t0), t0), t0.Add(after - time.Second), false},
		{"exactly the age", stopped(at(t0), t0), t0.Add(after), true},
		{"older", stopped(at(t0), t0), t0.Add(after * 3), true},
		{"no stop time: counted from the last update", stopped(nil, t0), t0.Add(after), true},
		{"no stop time, recent update", stopped(nil, t0), t0.Add(time.Hour), false},
		{"the stop time wins over the update", stopped(at(t0.Add(3*24*time.Hour)), t0), t0.Add(after), false},
		{"clock went backwards", stopped(at(t0), t0), t0.Add(-time.Hour), false},
		{"no time information", stopped(nil, time.Time{}), t0.Add(after * 10), false},
		{"nothing to remove", store.Workspace{State: store.StateStopped, LastStoppedAt: at(t0)}, t0.Add(after * 2), false},
		{"running", store.Workspace{State: store.StateRunning, ContainerID: "c1", LastStoppedAt: at(t0)}, t0.Add(after * 2), false},
		{"starting", store.Workspace{State: store.StateStarting, ContainerID: "c1", LastStoppedAt: at(t0)}, t0.Add(after * 2), false},
		{"offline", store.Workspace{State: store.StateOffline, LastStoppedAt: at(t0)}, t0.Add(after * 2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Reclaimable(tc.ws, tc.now, after); got != tc.want {
				t.Errorf("Reclaimable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCPUPercent(t *testing.T) {
	read := func(sec float64, nanos uint64) docker.CPUUsage {
		return docker.CPUUsage{TotalNanos: nanos, At: t0.Add(time.Duration(sec * float64(time.Second)))}
	}
	const sec = uint64(time.Second)
	cases := []struct {
		name   string
		prev   docker.CPUUsage
		cur    docker.CPUUsage
		want   float64
		wantOK bool
	}{
		{"idle", read(0, 5*sec), read(60, 5*sec), 0, true},
		{"one core busy the whole minute", read(0, 0), read(60, 60*sec), 100, true},
		{"three seconds of a minute is 5 percent", read(0, 0), read(60, 3*sec), 5, true},
		{"two cores busy", read(0, 0), read(60, 120*sec), 200, true},
		{"a tenth of a core", read(0, 7*sec), read(10, 8*sec), 10, true},
		{"no time between the readings", read(10, 0), read(10, sec), 0, false},
		{"the readings are in the wrong order", read(20, 0), read(10, sec), 0, false},
		{"the counter went back: the container restarted", read(0, 100*sec), read(60, 2*sec), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CPUPercent(tc.prev, tc.cur)
			if ok != tc.wantOK || (ok && (got < tc.want-1e-9 || got > tc.want+1e-9)) {
				t.Errorf("CPUPercent = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
