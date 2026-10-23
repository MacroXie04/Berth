package workspace

import (
	"fmt"
	"strconv"
)

// The environment the workspace image is configured with. The names are neko
// v3's; images/workspace/README.md documents the contract from the image side.
const (
	envNekoScreen        = "NEKO_DESKTOP_SCREEN"
	envNekoUserPassword  = "NEKO_MEMBER_MULTIUSER_USER_PASSWORD"
	envNekoAdminPassword = "NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD"
	envNekoEPR           = "NEKO_WEBRTC_EPR"
	envNekoUDPMux        = "NEKO_WEBRTC_UDPMUX"
	envNekoNAT1TO1       = "NEKO_WEBRTC_NAT1TO1"
	envNekoPathPrefix    = "NEKO_SERVER_PATH_PREFIX"
	envNekoProxy         = "NEKO_SERVER_PROXY"
	envNekoHosting       = "NEKO_SESSION_IMPLICIT_HOSTING"
	envNekoMetrics       = "NEKO_SERVER_METRICS"
)

// nekoEnv builds the KEY=value list for one workspace container.
//
// userPassword is what the web UI logs in with; the frontend gets it from
// GET /api/workspace. adminPassword is generated per container and never
// returned by the API: the UI user is a regular neko user. Both are plain
// environment variables, so code running in the container (same uid as neko)
// can read either; neither protects anything from the workspace's own user.
func nekoEnv(cfg Config, username, userPassword, adminPassword string, udpStart, udpEnd int) []string {
	env := []string{
		envNekoScreen + "=" + cfg.Resolution,
		envNekoUserPassword + "=" + userPassword,
		envNekoAdminPassword + "=" + adminPassword,
		// neko is not stripped by Traefik, so it must serve under its own prefix.
		envNekoPathPrefix + "=" + PathPrefix(username, "browser"),
		// Behind Traefik: trust its X-Forwarded-* headers.
		envNekoProxy + "=true",
		// The only person in the room gets control on the first click.
		envNekoHosting + "=true",
		// Otherwise <prefix>/metrics answers for any logged-in Berth user (verified).
		envNekoMetrics + "=false",
	}
	switch {
	case udpStart > 0 && udpEnd == udpStart:
		// One port: neko multiplexes every viewer of the workspace over it. This is
		// the default; it publishes one port per workspace and avoids the per-viewer
		// sockets a range uses (a closed tab holds its socket for 5-10 s, and sockets
		// were seen to stay bound until neko restarted).
		env = append(env, envNekoUDPMux+"="+itoa(udpStart))
	case udpStart > 0:
		// A range: each viewer takes one port from it.
		env = append(env, fmt.Sprintf("%s=%d-%d", envNekoEPR, udpStart, udpEnd))
	}
	if cfg.PublicIP != "" {
		env = append(env, envNekoNAT1TO1+"="+cfg.PublicIP)
	}
	return env
}

func itoa(n int) string { return strconv.Itoa(n) }
