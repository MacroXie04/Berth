package workspace

import (
	"errors"
	"testing"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

var allStates = []store.State{store.StateOffline, store.StateStarting, store.StateRunning, store.StateStopped}

func TestNextEveryStateAndEvent(t *testing.T) {
	off, sta, run, stp := store.StateOffline, store.StateStarting, store.StateRunning, store.StateStopped
	// want[event][from] = to; a missing entry means the transition is invalid.
	want := map[Event]map[store.State]store.State{
		EventStart:   {off: sta, stp: sta},
		EventReady:   {sta: run},
		EventStop:    {sta: stp, run: stp},
		EventExited:  {sta: stp, run: stp},
		EventResumed: {stp: sta},
		EventGone:    {sta: off, run: off, stp: off},
		EventReclaim: {stp: off},
	}
	for ev, table := range want {
		for _, from := range allStates {
			got, err := Next(from, ev)
			to, legal := table[from]
			switch {
			case legal && (err != nil || got != to):
				t.Errorf("Next(%s, %s) = %q, %v; want %q", from, ev, got, err, to)
			case !legal && !errors.Is(err, ErrInvalidTransition):
				t.Errorf("Next(%s, %s) = %q, %v; want ErrInvalidTransition", from, ev, got, err)
			}
		}
	}
	if _, err := Next(off, Event("explode")); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("unknown event err = %v", err)
	}
}

// Sources is what the database compares against, so it must agree with Next:
// an event is applicable from exactly the states Next accepts.
func TestSourcesAgreeWithNext(t *testing.T) {
	for ev := range transitions {
		srcs := map[store.State]bool{}
		for _, s := range Sources(ev) {
			srcs[s] = true
		}
		for _, from := range allStates {
			_, err := Next(from, ev)
			if (err == nil) != srcs[from] {
				t.Errorf("event %s from %s: Next ok=%v but Sources contains it=%v", ev, from, err == nil, srcs[from])
			}
		}
	}
	// The returned slice is a copy: callers cannot corrupt the table.
	s := Sources(EventStart)
	s[0] = store.StateRunning
	if Sources(EventStart)[0] == store.StateRunning {
		t.Error("Sources leaked the internal slice")
	}
}

func TestObserve(t *testing.T) {
	running := func(h docker.Health) docker.ContainerInfo {
		return docker.ContainerInfo{Running: true, Status: "running", Health: h}
	}
	notRunning := func(status string) docker.ContainerInfo { return docker.ContainerInfo{Status: status} }

	cases := []struct {
		name    string
		current store.State
		info    docker.ContainerInfo
		want    Event
		wantOK  bool
	}{
		{"starting + healthy => ready", store.StateStarting, running(docker.HealthHealthy), EventReady, true},
		{"starting + no healthcheck => ready", store.StateStarting, running(docker.HealthNone), EventReady, true},
		{"starting + still starting => wait", store.StateStarting, running(docker.HealthStarting), "", false},
		{"starting + unhealthy => keep", store.StateStarting, running(docker.HealthUnhealthy), "", false},
		{"running + healthy => nothing", store.StateRunning, running(docker.HealthHealthy), "", false},
		{"running + unhealthy => keep running", store.StateRunning, running(docker.HealthUnhealthy), "", false},
		{"running + health restarts => keep running", store.StateRunning, running(docker.HealthStarting), "", false},
		{"stopped but container is running => resumed", store.StateStopped, running(docker.HealthHealthy), EventResumed, true},
		{"stopped but running and still starting => resumed", store.StateStopped, running(docker.HealthStarting), EventResumed, true},
		{"stopped but running and unhealthy => resumed", store.StateStopped, running(docker.HealthUnhealthy), EventResumed, true},
		{"offline but a container shows up => ignored (reconcile is M4)", store.StateOffline, running(docker.HealthHealthy), "", false},

		{"running + exited => stopped", store.StateRunning, notRunning("exited"), EventExited, true},
		{"starting + exited (crashed on boot) => stopped", store.StateStarting, notRunning("exited"), EventExited, true},
		{"running + dead => stopped", store.StateRunning, notRunning("dead"), EventExited, true},
		{"stopped + exited => nothing", store.StateStopped, notRunning("exited"), "", false},
		{"offline + exited => nothing", store.StateOffline, notRunning("exited"), "", false},

		// A refresh that races Start must never undo it.
		// During a start a created container is normal; Manager.Status applies this
		// only when no start of this process is in flight (the start died before
		// `docker start`).
		{"starting + created (never started) => stopped", store.StateStarting, notRunning("created"), EventExited, true},
		{"stopped + created => nothing", store.StateStopped, notRunning("created"), "", false},
		{"running + created => nothing", store.StateRunning, notRunning("created"), "", false},
		{"running + paused => nothing", store.StateRunning, notRunning("paused"), "", false},
		{"running + restarting => nothing", store.StateRunning, notRunning("restarting"), "", false},
		{"running + removing => nothing", store.StateRunning, notRunning("removing"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Observe(tc.current, tc.info)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("Observe = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
			// Whatever Observe proposes must be legal from the current state,
			// or the database compare-and-swap would reject it.
			if ok {
				if _, err := Next(tc.current, got); err != nil {
					t.Fatalf("Observe proposed %s, which is illegal from %s: %v", got, tc.current, err)
				}
			}
		})
	}
}
