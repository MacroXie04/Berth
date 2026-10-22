// Package config loads Berth's configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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
}

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
