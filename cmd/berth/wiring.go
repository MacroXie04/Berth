package main

import (
	"log/slog"
	"time"

	"github.com/MacroXie04/Berth/internal/config"
	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/reaper"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

// workspaceConfig maps the environment-derived configuration onto what the
// workspace manager runs with. It is a function of its own so a test can tell
// that every setting arrives (a dropped BERTH_PUBLIC_IP or
// BERTH_UDP_PORTS_PER_WORKSPACE would otherwise change nothing the compiler or
// the manager's own tests could see).
func workspaceConfig(cfg config.Config) workspace.Config {
	return workspace.Config{
		Image:                cfg.WorkspaceImage,
		TraefikContainer:     cfg.TraefikContainer,
		Entrypoint:           cfg.TraefikEntrypoint,
		UDPPortMin:           cfg.UDPPortMin,
		UDPPortMax:           cfg.UDPPortMax,
		UDPPortsPerWorkspace: cfg.UDPPortsPerWorkspace,
		Resolution:           cfg.Resolution,
		PublicIP:             cfg.PublicIP,
		ShmSizeMB:            cfg.ShmSizeMB,
		PidsLimit:            int64(cfg.PidsLimit),
		StopTimeout:          cfg.StopTimeout,
		StartTimeout:         cfg.StartTimeout,
	}
}

// publicIPWarning is the startup warning owed to a deployment that did not set
// BERTH_PUBLIC_IP, or "" when none is due. Without the address neko advertises
// only the workspace's private bridge address as its WebRTC candidate, so video
// connects for no browser off the Docker host.
func publicIPWarning(cfg config.Config) string {
	if cfg.PublicIP != "" {
		return ""
	}
	return "BERTH_PUBLIC_IP is not set: workspace video (WebRTC) only connects for browsers that can reach " +
		"the Docker bridge network, i.e. the Docker host itself. Set it to the address browsers use to reach this " +
		"host (its LAN address on a LAN, its public address on the internet)."
}

// newWorkspaceManager builds the manager from the configuration and logs what
// the operator must be told about it.
func newWorkspaceManager(cfg config.Config, st store.Store, dock docker.Client, log *slog.Logger) (*workspace.Manager, error) {
	m, err := workspace.NewManager(st, dock, workspaceConfig(cfg), log, nil)
	if err != nil {
		return nil, err
	}
	if msg := publicIPWarning(cfg); msg != "" {
		log.Warn(msg)
	}
	return m, nil
}

// reaperConfig maps the environment-derived configuration onto the reaper. Like
// workspaceConfig it is a function of its own, so a test can tell that every
// setting arrives: a dropped BERTH_IDLE_TIMEOUT would silently fall back to the
// reaper's zero value (a config the reaper refuses) or, worse, to a default.
func reaperConfig(cfg config.Config) reaper.Config {
	return reaper.Config{
		IdleTimeout:    cfg.IdleTimeout,
		ReclaimAfter:   cfg.ReclaimAfter,
		Interval:       cfg.ReaperInterval,
		AttachInterval: cfg.ProxyAttachInterval,
		IdleCPUPercent: cfg.IdleCPUPercent,
		// One pass over a workspace may include a graceful stop; leave room for it.
		OpTimeout: cfg.StopTimeout + 2*time.Minute,
	}
}

// shortIdleTimeoutWarning is the startup warning owed to a deployment whose idle
// timeout is short enough to stop people who are still at their desk, or "" when
// none is due. The page sends a heartbeat every 30 s, but a browser may hold
// timers of a background tab to once a minute or less, and the reaper only looks
// once per interval; a timeout of a few minutes leaves no slack for that.
func shortIdleTimeoutWarning(cfg config.Config) string {
	if cfg.IdleTimeout >= minSaneIdleTimeout {
		return ""
	}
	return "BERTH_IDLE_TIMEOUT is " + cfg.IdleTimeout.String() + ": a workspace whose page only misses a few heartbeats " +
		"(a background tab, a sleeping laptop) will be powered off, ending its programs. Values under " +
		minSaneIdleTimeout.String() + " are meant for testing."
}

const minSaneIdleTimeout = 5 * time.Minute
