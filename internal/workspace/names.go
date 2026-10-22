// Package workspace owns the container lifecycle: naming, create/start/stop/
// remove, and Traefik labels. Milestone 1 only needs the naming scheme.
package workspace

// Resource names derived from a username. Usernames are validated by
// auth.ValidateUsername before they ever reach these helpers.
const (
	VolumePrefix    = "berth-data-"
	ContainerPrefix = "berth-ws-"
	NetworkPrefix   = "berth-net-"

	// ProxyNetwork is the compose network Traefik and the backend share.
	// Workspaces must NOT join it: members of one bridge reach each other
	// directly, bypassing ForwardAuth (see "Open decisions" D1 in CLAUDE.md).
	ProxyNetwork = "berth-proxy"
)

// VolumeName is the data volume mounted at /home/user. It is never deleted.
func VolumeName(username string) string { return VolumePrefix + username }

// ContainerName is the workspace container's name.
func ContainerName(username string) string { return ContainerPrefix + username }

// NetworkName is the user's private network.
func NetworkName(username string) string { return NetworkPrefix + username }
