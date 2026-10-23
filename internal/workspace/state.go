package workspace

import (
	"errors"
	"fmt"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

// Event is something that moves a workspace along the lifecycle:
//
//	offline ──start──▶ starting ──ready──▶ running
//	running ──stop / exited──▶ stopped        (container kept)
//	stopped ──start──▶ starting               (docker start, seconds)
//	stopped ──reclaim──▶ offline              (docker rm, volume kept)
//	any ──gone──▶ offline                     (container vanished)
//
// This file is pure: it decides which moves are legal and what an observed
// container implies. The Manager performs the Docker work and the database
// compare-and-swap.
type Event string

const (
	EventStart   Event = "start"   // the user powers on
	EventReady   Event = "ready"   // the container's healthcheck passes
	EventStop    Event = "stop"    // the user (or, in M4, the reaper) powers off
	EventExited  Event = "exited"  // the container exited by itself
	EventResumed Event = "resumed" // a stopped workspace's container is running again
	EventGone    Event = "gone"    // the container no longer exists
	EventReclaim Event = "reclaim" // M4: stopped for too long, container removed
)

// ErrInvalidTransition means the event is not allowed from that state.
var ErrInvalidTransition = errors.New("workspace: invalid state transition")

type edge struct {
	from []store.State
	to   store.State
}

var transitions = map[Event]edge{
	EventStart:   {[]store.State{store.StateOffline, store.StateStopped}, store.StateStarting},
	EventReady:   {[]store.State{store.StateStarting}, store.StateRunning},
	EventStop:    {[]store.State{store.StateStarting, store.StateRunning}, store.StateStopped},
	EventExited:  {[]store.State{store.StateStarting, store.StateRunning}, store.StateStopped},
	EventResumed: {[]store.State{store.StateStopped}, store.StateStarting},
	EventGone:    {[]store.State{store.StateStarting, store.StateRunning, store.StateStopped}, store.StateOffline},
	EventReclaim: {[]store.State{store.StateStopped}, store.StateOffline},
}

// Sources lists the states an event may be applied from. Pass it to
// store.TransitionWorkspace so the database enforces the same rule atomically.
func Sources(ev Event) []store.State {
	return append([]store.State(nil), transitions[ev].from...)
}

// Next returns the state an event leads to, or ErrInvalidTransition.
func Next(from store.State, ev Event) (store.State, error) {
	e, ok := transitions[ev]
	if !ok {
		return "", fmt.Errorf("%w: unknown event %q", ErrInvalidTransition, ev)
	}
	for _, s := range e.from {
		if s == from {
			return e.to, nil
		}
	}
	return "", fmt.Errorf("%w: %s from %s", ErrInvalidTransition, ev, from)
}

// Observe turns what Docker reports about the workspace's container into the
// event the database should catch up with, if any. current is the state the
// database holds.
//
// It is deliberately conservative: a container that is merely "created" or
// still starting up, an unhealthy running container, and anything transient
// (paused, restarting, removing) produce no event, so a refresh racing a
// Start in flight can never undo it.
func Observe(current store.State, info docker.ContainerInfo) (Event, bool) {
	if info.Running {
		switch info.Health {
		case docker.HealthNone, docker.HealthHealthy:
			switch current {
			case store.StateStarting:
				return EventReady, true
			case store.StateStopped:
				// Started behind Berth's back (docker start, host reboot with a
				// restart policy elsewhere). Treat it as a start in progress; the
				// next refresh promotes it once healthy.
				return EventResumed, true
			}
		case docker.HealthStarting, docker.HealthUnhealthy:
			if current == store.StateStopped {
				return EventResumed, true
			}
			// starting/running + not healthy: keep the state. The API reports the
			// health separately so the UI can show it.
		}
		return "", false
	}
	switch info.Status {
	case "exited", "dead":
		if current == store.StateStarting || current == store.StateRunning {
			return EventExited, true
		}
	}
	return "", false
}
