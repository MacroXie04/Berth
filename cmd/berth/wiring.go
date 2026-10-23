package main

import (
	"log/slog"

	"github.com/MacroXie04/Berth/internal/config"
	"github.com/MacroXie04/Berth/internal/docker"
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
