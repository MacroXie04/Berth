package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		ListenAddr:        ":8080",
		DBPath:            "./data/berth.db",
		LogLevel:          slog.LevelInfo,
		SessionTTL:        7 * 24 * time.Hour,
		CookieSecure:      false,
		BcryptCost:        12,
		DefaultMemLimitMB: 2048,
		DefaultCPULimit:   2.0,
	}
	if cfg != want {
		t.Fatalf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"BERTH_LISTEN_ADDR":    "127.0.0.1:9000",
		"BERTH_DB_PATH":        "/data/b.db",
		"BERTH_LOG_LEVEL":      "debug",
		"BERTH_SESSION_TTL":    "36h",
		"BERTH_COOKIE_SECURE":  "true",
		"BERTH_BCRYPT_COST":    "10",
		"BERTH_DEFAULT_MEM_MB": "4096",
		"BERTH_DEFAULT_CPUS":   "1.5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" || cfg.DBPath != "/data/b.db" || cfg.LogLevel != slog.LevelDebug ||
		cfg.SessionTTL != 36*time.Hour || !cfg.CookieSecure || cfg.BcryptCost != 10 ||
		cfg.DefaultMemLimitMB != 4096 || cfg.DefaultCPULimit != 1.5 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestBlankAndWhitespaceMeanUnset(t *testing.T) {
	cfg, err := Load(env(map[string]string{"BERTH_LISTEN_ADDR": "   ", "BERTH_SESSION_TTL": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" || cfg.SessionTTL != 7*24*time.Hour {
		t.Fatalf("blank values should fall back to defaults: %+v", cfg)
	}
}

func TestInvalidValuesAreAllReported(t *testing.T) {
	_, err := Load(env(map[string]string{
		"BERTH_SESSION_TTL":    "soon",
		"BERTH_COOKIE_SECURE":  "maybe",
		"BERTH_BCRYPT_COST":    "twelve",
		"BERTH_DEFAULT_MEM_MB": "lots",
		"BERTH_DEFAULT_CPUS":   "fast",
		"BERTH_LOG_LEVEL":      "loud",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{
		"BERTH_SESSION_TTL", "BERTH_COOKIE_SECURE", "BERTH_BCRYPT_COST",
		"BERTH_DEFAULT_MEM_MB", "BERTH_DEFAULT_CPUS", "BERTH_LOG_LEVEL",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s:\n%v", key, err)
		}
	}
}

func TestBounds(t *testing.T) {
	cases := map[string]map[string]string{
		"zero ttl":      {"BERTH_SESSION_TTL": "0s"},
		"negative ttl":  {"BERTH_SESSION_TTL": "-1h"},
		"cost too low":  {"BERTH_BCRYPT_COST": "9"},
		"cost too high": {"BERTH_BCRYPT_COST": "16"},
		"zero memory":   {"BERTH_DEFAULT_MEM_MB": "0"},
		"negative cpus": {"BERTH_DEFAULT_CPUS": "-1"},
		"zero cpus":     {"BERTH_DEFAULT_CPUS": "0"},
		"NaN cpus":      {"BERTH_DEFAULT_CPUS": "NaN"},
		"Inf cpus":      {"BERTH_DEFAULT_CPUS": "Inf"},
		"+Inf cpus":     {"BERTH_DEFAULT_CPUS": "+Inf"},
		"-Inf cpus":     {"BERTH_DEFAULT_CPUS": "-Inf"},
		"overflow cpus": {"BERTH_DEFAULT_CPUS": "1e999"},
		"huge cpus":     {"BERTH_DEFAULT_CPUS": "1025"},
		"huge memory":   {"BERTH_DEFAULT_MEM_MB": "1048577"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(env(e)); err == nil {
				t.Fatalf("expected error for %v", e)
			}
		})
	}
}

func TestBoundaryValuesAreAccepted(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"BERTH_BCRYPT_COST":    "15",
		"BERTH_DEFAULT_MEM_MB": "1048576",
		"BERTH_DEFAULT_CPUS":   "1024",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BcryptCost != 15 || cfg.DefaultMemLimitMB != MaxMemLimitMB || cfg.DefaultCPULimit != MaxCPULimit {
		t.Fatalf("upper bounds not accepted as-is: %+v", cfg)
	}
	if cfg, err := Load(env(map[string]string{"BERTH_BCRYPT_COST": "10", "BERTH_DEFAULT_MEM_MB": "1", "BERTH_DEFAULT_CPUS": "0.01"})); err != nil || cfg.BcryptCost != 10 {
		t.Fatalf("lower bounds rejected: %+v, %v", cfg, err)
	}
}
