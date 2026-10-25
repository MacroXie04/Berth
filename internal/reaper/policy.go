package reaper

import (
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

// The policy is plain functions of a workspace row and the time, so every rule
// is tested without Docker, a database or a clock.

// lastActivity is when the workspace was last known to be in use: the later of
// its last heartbeat (a workspace that never had one counts from its last update)
// and floor. The floor is the moment this process started, which gives a
// restarted backend a full idle timeout to receive heartbeats again before it
// stops anyone: a redeploy that took the API down for an hour must not read as
// "everyone has been idle for an hour".
func lastActivity(ws store.Workspace, floor time.Time) time.Time {
	t := ws.UpdatedAt
	if ws.LastActiveAt != nil {
		t = *ws.LastActiveAt
	}
	if t.Before(floor) {
		t = floor
	}
	return t
}

// Idle reports whether a workspace that is up has been unused for at least
// timeout. "Up" includes starting: a container that never turns healthy leaves
// its row at starting for good, and must not hold its memory forever. A start
// that is really under way is not idle, because Start records activity. A
// last-activity time in the future (the clock went backwards) is not idle.
func Idle(ws store.Workspace, now, floor time.Time, timeout time.Duration) bool {
	if ws.State != store.StateRunning && ws.State != store.StateStarting {
		return false
	}
	last := lastActivity(ws, floor)
	if last.IsZero() {
		return false // no information at all: never stop on that
	}
	return now.Sub(last) >= timeout
}

// Reclaimable reports whether a stopped workspace has been stopped for at least
// after, so its container can go. A workspace with no recorded stop time counts
// from its last update.
func Reclaimable(ws store.Workspace, now time.Time, after time.Duration) bool {
	if ws.State != store.StateStopped || ws.ContainerID == "" {
		return false
	}
	t := ws.UpdatedAt
	if ws.LastStoppedAt != nil {
		t = *ws.LastStoppedAt
	}
	if t.IsZero() {
		return false
	}
	return now.Sub(t) >= after
}

// CPUPercent is the average CPU use between two cumulative readings of the same
// container, `docker stats` style (100 = one core fully busy). ok is false when
// the readings cannot be compared: no time between them, or a counter that went
// backwards because the container restarted.
func CPUPercent(prev, cur docker.CPUUsage) (percent float64, ok bool) {
	elapsed := cur.At.Sub(prev.At)
	if elapsed <= 0 || cur.TotalNanos < prev.TotalNanos {
		return 0, false
	}
	return float64(cur.TotalNanos-prev.TotalNanos) / float64(elapsed) * 100, true
}
