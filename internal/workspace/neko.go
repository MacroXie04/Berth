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
	envNekoNAT1TO1       = "NEKO_WEBRTC_NAT1TO1"
	envNekoPathPrefix    = "NEKO_SERVER_PATH_PREFIX"
	envNekoProxy         = "NEKO_SERVER_PROXY"
	envNekoHosting       = "NEKO_SESSION_IMPLICIT_HOSTING"
	envNekoMetrics       = "NEKO_SERVER_METRICS"
)

// nekoEnv builds the KEY=value list for one workspace container.
//
// userPassword is what the web UI logs in with; the frontend gets it from
// GET /api/workspace. adminPassword is generated per container and handed to
// no one: the UI user is a regular neko user, and a separate admin secret keeps
// the two roles distinct.
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
	if udpStart > 0 {
		env = append(env, fmt.Sprintf("%s=%d-%d", envNekoEPR, udpStart, udpEnd))
	}
	if cfg.PublicIP != "" {
		env = append(env, envNekoNAT1TO1+"="+cfg.PublicIP)
	}
	return env
}

func itoa(n int) string { return strconv.Itoa(n) }
