// Package config loads Berth's configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the backend's runtime configuration. Every field maps to one
// BERTH_* environment variable documented in .env.example.
type Config struct {
	ListenAddr string     // BERTH_LISTEN_ADDR
	DBPath     string     // BERTH_DB_PATH
	LogLevel   slog.Level // BERTH_LOG_LEVEL

	SessionTTL   time.Duration // BERTH_SESSION_TTL
	CookieSecure bool          // BERTH_COOKIE_SECURE
	BcryptCost   int           // BERTH_BCRYPT_COST

	// Resource limits stamped onto a user's workspace row at registration.
	DefaultMemLimitMB int     // BERTH_DEFAULT_MEM_MB
	DefaultCPULimit   float64 // BERTH_DEFAULT_CPUS

	// Workspace containers.
	WorkspaceImage       string        // BERTH_WORKSPACE_IMAGE
	TraefikContainer     string        // BERTH_TRAEFIK_CONTAINER; "" disables attaching (set "none")
	TraefikEntrypoint    string        // BERTH_TRAEFIK_ENTRYPOINT
	UDPPortMin           int           // BERTH_UDP_PORT_MIN
	UDPPortMax           int           // BERTH_UDP_PORT_MAX
	UDPPortsPerWorkspace int           // BERTH_UDP_PORTS_PER_WORKSPACE
	Resolution           string        // BERTH_RESOLUTION, e.g. 1280x720@30
	PublicIP             string        // BERTH_PUBLIC_IP; "" lets neko choose
	ShmSizeMB            int           // BERTH_SHM_MB
	PidsLimit            int           // BERTH_PIDS_LIMIT
	StopTimeout          time.Duration // BERTH_STOP_TIMEOUT
	StartTimeout         time.Duration // BERTH_START_TIMEOUT
	// ProxyAttachInterval is how often Traefik is re-attached to the user
	// networks; it bounds how long workspaces are unreachable after Traefik is
	// recreated.
	ProxyAttachInterval time.Duration // BERTH_PROXY_ATTACH_INTERVAL

	// The reaper (internal/reaper).
	IdleTimeout    time.Duration // BERTH_IDLE_TIMEOUT: a running workspace unused this long is stopped
	ReclaimAfter   time.Duration // BERTH_RECLAIM_AFTER: a workspace stopped this long loses its container
	ReaperInterval time.Duration // BERTH_REAPER_INTERVAL: how often the reaper looks
	IdleCPUPercent float64       // BERTH_IDLE_CPU_PERCENT: average CPU at or above this counts as use (100 = one core); 0 ignores CPU
}

// noProxy is the BERTH_TRAEFIK_CONTAINER value that turns proxy attachment off.
// (An empty variable means "unset", i.e. the default, so disabling needs a word.)
const noProxy = "none"

var (
	// Container names, image references and entrypoints are interpolated into
	// Docker calls and Traefik label keys, so they get a strict shape.
	nameRE       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	entrypointRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	imageRE      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,255}$`)
	resolutionRE = regexp.MustCompile(`^[1-9][0-9]{2,4}x[1-9][0-9]{2,4}@[1-9][0-9]{0,2}$`)
)

// Bounds for BERTH_BCRYPT_COST: below 10 is too cheap to brute-force-resist,
// above 15 lets a handful of requests pin every CPU.
const (
	MinBcryptCost = 10
	MaxBcryptCost = 15
)

// Upper bounds for the default workspace limits. They exist to reject typos and
// non-numbers (strconv accepts "NaN" and "Inf"), not to size a machine.
const (
	MaxMemLimitMB = 1 << 20 // 1 TiB
	MaxCPULimit   = 1024
)

// Bounds for the reaper's settings. The age limit exists to reject typos (and
// keep a duration comfortably inside what the database and time arithmetic hold),
// not to size a deployment.
const (
	MaxReaperAge      = 3650 * 24 * time.Hour
	MaxIdleCPUPercent = 1000
)

// Load reads the configuration through getenv (os.Getenv in production) and
// reports every invalid value at once rather than stopping at the first.
func Load(getenv func(string) string) (Config, error) {
	l := loader{getenv: getenv}

	cfg := Config{
		ListenAddr:        l.str("BERTH_LISTEN_ADDR", ":8080"),
		DBPath:            l.str("BERTH_DB_PATH", "./data/berth.db"),
		LogLevel:          l.logLevel("BERTH_LOG_LEVEL", slog.LevelInfo),
		SessionTTL:        l.duration("BERTH_SESSION_TTL", 7*24*time.Hour),
		CookieSecure:      l.boolean("BERTH_COOKIE_SECURE", false),
		BcryptCost:        l.integer("BERTH_BCRYPT_COST", 12),
		DefaultMemLimitMB: l.integer("BERTH_DEFAULT_MEM_MB", 2048),
		DefaultCPULimit:   l.float("BERTH_DEFAULT_CPUS", 2.0),

		WorkspaceImage:       l.str("BERTH_WORKSPACE_IMAGE", "berth/workspace:latest"),
		TraefikContainer:     l.str("BERTH_TRAEFIK_CONTAINER", "berth-traefik"),
		TraefikEntrypoint:    l.str("BERTH_TRAEFIK_ENTRYPOINT", "web"),
		UDPPortMin:           l.integer("BERTH_UDP_PORT_MIN", 30000),
		UDPPortMax:           l.integer("BERTH_UDP_PORT_MAX", 30999),
		UDPPortsPerWorkspace: l.integer("BERTH_UDP_PORTS_PER_WORKSPACE", 1),
		Resolution:           l.str("BERTH_RESOLUTION", "1280x720@30"),
		PublicIP:             l.str("BERTH_PUBLIC_IP", ""),
		ShmSizeMB:            l.integer("BERTH_SHM_MB", 2048),
		PidsLimit:            l.integer("BERTH_PIDS_LIMIT", 2048),
		StopTimeout:          l.duration("BERTH_STOP_TIMEOUT", 15*time.Second),
		StartTimeout:         l.duration("BERTH_START_TIMEOUT", 45*time.Second),
		ProxyAttachInterval:  l.duration("BERTH_PROXY_ATTACH_INTERVAL", 10*time.Second),

		IdleTimeout:    l.duration("BERTH_IDLE_TIMEOUT", 30*time.Minute),
		ReclaimAfter:   l.duration("BERTH_RECLAIM_AFTER", 7*24*time.Hour),
		ReaperInterval: l.duration("BERTH_REAPER_INTERVAL", time.Minute),
		IdleCPUPercent: l.float("BERTH_IDLE_CPU_PERCENT", 5),
	}
	if cfg.TraefikContainer == noProxy {
		cfg.TraefikContainer = ""
	}

	if cfg.SessionTTL <= 0 {
		l.fail("BERTH_SESSION_TTL must be positive")
	}
	if cfg.BcryptCost < MinBcryptCost || cfg.BcryptCost > MaxBcryptCost {
		l.fail("BERTH_BCRYPT_COST must be between %d and %d", MinBcryptCost, MaxBcryptCost)
	}
	if cfg.DefaultMemLimitMB <= 0 || cfg.DefaultMemLimitMB > MaxMemLimitMB {
		l.fail("BERTH_DEFAULT_MEM_MB must be between 1 and %d", MaxMemLimitMB)
	}
	// !(x > 0) is deliberate: it also rejects NaN, for which every comparison is
	// false. Infinities fall to the bounds (-Inf is not > 0; +Inf is > MaxCPULimit).
	if !(cfg.DefaultCPULimit > 0) || cfg.DefaultCPULimit > MaxCPULimit {
		l.fail("BERTH_DEFAULT_CPUS must be a number greater than 0 and at most %d", MaxCPULimit)
	}
	if cfg.ListenAddr == "" || cfg.DBPath == "" {
		l.fail("BERTH_LISTEN_ADDR and BERTH_DB_PATH must not be empty")
	}

	if !imageRE.MatchString(cfg.WorkspaceImage) {
		l.fail("BERTH_WORKSPACE_IMAGE %q is not a valid image reference", cfg.WorkspaceImage)
	}
	if cfg.TraefikContainer != "" && !nameRE.MatchString(cfg.TraefikContainer) {
		l.fail("BERTH_TRAEFIK_CONTAINER %q is not a valid container name (or %q to disable)", cfg.TraefikContainer, noProxy)
	}
	if !entrypointRE.MatchString(cfg.TraefikEntrypoint) {
		l.fail("BERTH_TRAEFIK_ENTRYPOINT %q is not a valid entrypoint name", cfg.TraefikEntrypoint)
	}
	if !resolutionRE.MatchString(cfg.Resolution) {
		l.fail("BERTH_RESOLUTION %q must look like 1280x720@30", cfg.Resolution)
	}
	if cfg.PublicIP != "" {
		if _, err := netip.ParseAddr(cfg.PublicIP); err != nil {
			l.fail("BERTH_PUBLIC_IP %q is not an IP address", cfg.PublicIP)
		}
	}
	if cfg.UDPPortMin < 1024 || cfg.UDPPortMax > 65535 || cfg.UDPPortsPerWorkspace < 1 ||
		cfg.UDPPortMax-cfg.UDPPortMin+1 < cfg.UDPPortsPerWorkspace {
		l.fail("BERTH_UDP_PORT_MIN/MAX must be within 1024-65535 and hold at least BERTH_UDP_PORTS_PER_WORKSPACE (>= 1) ports")
	}
	if cfg.ShmSizeMB < 64 || cfg.ShmSizeMB > MaxMemLimitMB {
		l.fail("BERTH_SHM_MB must be between 64 and %d", MaxMemLimitMB)
	}
	if cfg.PidsLimit < 64 || cfg.PidsLimit > 1<<20 {
		l.fail("BERTH_PIDS_LIMIT must be between 64 and %d", 1<<20)
	}
	if cfg.StopTimeout < time.Second || cfg.StopTimeout > 10*time.Minute {
		l.fail("BERTH_STOP_TIMEOUT must be between 1s and 10m")
	}
	if cfg.StartTimeout < 5*time.Second || cfg.StartTimeout > 10*time.Minute {
		l.fail("BERTH_START_TIMEOUT must be between 5s and 10m")
	}
	if cfg.ProxyAttachInterval < 2*time.Second || cfg.ProxyAttachInterval > 10*time.Minute {
		l.fail("BERTH_PROXY_ATTACH_INTERVAL must be between 2s and 10m")
	}

	if cfg.IdleTimeout < time.Minute || cfg.IdleTimeout > MaxReaperAge {
		l.fail("BERTH_IDLE_TIMEOUT must be between 1m and %s", MaxReaperAge)
	}
	if cfg.ReclaimAfter < time.Minute || cfg.ReclaimAfter > MaxReaperAge {
		l.fail("BERTH_RECLAIM_AFTER must be between 1m and %s", MaxReaperAge)
	}
	if cfg.ReaperInterval < 5*time.Second || cfg.ReaperInterval > time.Hour {
		l.fail("BERTH_REAPER_INTERVAL must be between 5s and 1h")
	}
	// !(x >= 0) also rejects NaN.
	if !(cfg.IdleCPUPercent >= 0) || cfg.IdleCPUPercent > MaxIdleCPUPercent {
		l.fail("BERTH_IDLE_CPU_PERCENT must be a number between 0 and %d", MaxIdleCPUPercent)
	}

	if err := errors.Join(l.errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// loader reads typed values and accumulates parse errors.
type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

// lookup returns the trimmed value, or "" when unset or blank.
func (l *loader) lookup(key string) string {
	return strings.TrimSpace(l.getenv(key))
}

func (l *loader) str(key, def string) string {
	if v := l.lookup(key); v != "" {
		return v
	}
	return def
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v := l.lookup(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail("%s: %q is not a duration (e.g. 30m, 168h)", key, v)
		return def
	}
	return d
}

func (l *loader) boolean(key string, def bool) bool {
	v := l.lookup(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail("%s: %q is not a boolean", key, v)
		return def
	}
	return b
}

func (l *loader) integer(key string, def int) int {
	v := l.lookup(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail("%s: %q is not an integer", key, v)
		return def
	}
	return n
}

func (l *loader) float(key string, def float64) float64 {
	v := l.lookup(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.fail("%s: %q is not a number", key, v)
		return def
	}
	return f
}

func (l *loader) logLevel(key string, def slog.Level) slog.Level {
	v := l.lookup(key)
	if v == "" {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail("%s: %q is not one of debug, info, warn, error", key, v)
		return def
	}
	return lvl
}
