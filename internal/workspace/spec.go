package workspace

import (
	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
)

// homeMount is where the user's data volume lives inside the container.
const homeMount = "/home/user"

// buildSpec turns a workspace row into the container to create.
//
// Hardening, all of it compatible with Chromium under neko (verified against
// the image): no NET_RAW (stops ARP/ICMP spoofing on the user's network),
// no-new-privileges (no setuid escalation), a PID cap (fork bombs), and no
// restart policy (the backend owns the lifecycle).
func buildSpec(cfg Config, user store.User, ws store.Workspace, userPassword, adminPassword string,
	udpStart, udpEnd int) docker.ContainerSpec {
	name, net := ContainerName(user.Username), NetworkName(user.Username)
	return docker.ContainerSpec{
		Name:     name,
		Hostname: name,
		Image:    cfg.Image,
		Env:      nekoEnv(cfg, user.Username, userPassword, adminPassword, udpStart, udpEnd),
		Labels:   Labels(user.Username, net, cfg.Entrypoint),
		Network:  net,
		Volumes:  []docker.VolumeMount{{Volume: ws.VolumeName, Target: homeMount}},

		MemoryBytes:  int64(ws.MemLimitMB) << 20,
		NanoCPUs:     int64(ws.CPULimit * 1e9),
		ShmSizeBytes: int64(cfg.ShmSizeMB) << 20,
		PidsLimit:    cfg.PidsLimit,
		CapDrop:      []string{"NET_RAW"},
		SecurityOpt:  []string{"no-new-privileges"},
		Init:         true,
		StopTimeout:  cfg.StopTimeout,

		UDPPortStart: udpStart,
		UDPPortEnd:   udpEnd,
	}
}
