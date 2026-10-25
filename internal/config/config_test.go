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

		WorkspaceImage:       "berth/workspace:latest",
		TraefikContainer:     "berth-traefik",
		TraefikEntrypoint:    "web",
		UDPPortMin:           30000,
		UDPPortMax:           30999,
		UDPPortsPerWorkspace: 1,
		Resolution:           "1280x720@30",
		ShmSizeMB:            2048,
		PidsLimit:            2048,
		StopTimeout:          15 * time.Second,
		StartTimeout:         45 * time.Second,
		ProxyAttachInterval:  10 * time.Second,

		IdleTimeout:    30 * time.Minute,
		ReclaimAfter:   7 * 24 * time.Hour,
		ReaperInterval: time.Minute,
		IdleCPUPercent: 5,
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

func TestWorkspaceOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"BERTH_WORKSPACE_IMAGE":         "registry.example.com/team/ws:1.2.3",
		"BERTH_TRAEFIK_CONTAINER":       "proxy_1",
		"BERTH_TRAEFIK_ENTRYPOINT":      "websecure",
		"BERTH_UDP_PORT_MIN":            "40000",
		"BERTH_UDP_PORT_MAX":            "40063",
		"BERTH_UDP_PORTS_PER_WORKSPACE": "16",
		"BERTH_RESOLUTION":              "1920x1080@60",
		"BERTH_PUBLIC_IP":               "203.0.113.7",
		"BERTH_SHM_MB":                  "1024",
		"BERTH_PIDS_LIMIT":              "4096",
		"BERTH_STOP_TIMEOUT":            "30s",
		"BERTH_START_TIMEOUT":           "2m",
		"BERTH_PROXY_ATTACH_INTERVAL":   "30s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceImage != "registry.example.com/team/ws:1.2.3" || cfg.TraefikContainer != "proxy_1" ||
		cfg.TraefikEntrypoint != "websecure" || cfg.UDPPortMin != 40000 || cfg.UDPPortMax != 40063 ||
		cfg.UDPPortsPerWorkspace != 16 || cfg.Resolution != "1920x1080@60" || cfg.PublicIP != "203.0.113.7" ||
		cfg.ShmSizeMB != 1024 || cfg.PidsLimit != 4096 || cfg.StopTimeout != 30*time.Second || cfg.StartTimeout != 2*time.Minute ||
		cfg.ProxyAttachInterval != 30*time.Second {
		t.Fatalf("workspace overrides not applied: %+v", cfg)
	}
	if cfg, err := Load(env(map[string]string{"BERTH_PUBLIC_IP": "2001:db8::1"})); err != nil || cfg.PublicIP != "2001:db8::1" {
		t.Fatalf("IPv6 public IP rejected: %v", err)
	}
}

func TestProxyAttachmentCanBeDisabled(t *testing.T) {
	cfg, err := Load(env(map[string]string{"BERTH_TRAEFIK_CONTAINER": "none"}))
	if err != nil || cfg.TraefikContainer != "" {
		t.Fatalf("got %q, %v; want attachment disabled", cfg.TraefikContainer, err)
	}
}

// Names, entrypoints and images are interpolated into Docker calls and Traefik
// label keys, so metacharacters must be refused at startup.
func TestWorkspaceValuesAreValidated(t *testing.T) {
	bad := map[string]map[string]string{
		"image with space":          {"BERTH_WORKSPACE_IMAGE": "a b"},
		"image with backtick":       {"BERTH_WORKSPACE_IMAGE": "x`y"},
		"image with comma":          {"BERTH_WORKSPACE_IMAGE": "a,b"},
		"container with space":      {"BERTH_TRAEFIK_CONTAINER": "a b"},
		"container with slash":      {"BERTH_TRAEFIK_CONTAINER": "../x"},
		"container starting dash":   {"BERTH_TRAEFIK_CONTAINER": "-x"},
		"entrypoint with comma":     {"BERTH_TRAEFIK_ENTRYPOINT": "web,admin"},
		"entrypoint with backtick":  {"BERTH_TRAEFIK_ENTRYPOINT": "we`b"},
		"entrypoint with dot":       {"BERTH_TRAEFIK_ENTRYPOINT": "web.x"},
		"resolution no fps":         {"BERTH_RESOLUTION": "1280x720"},
		"resolution garbage":        {"BERTH_RESOLUTION": "big"},
		"resolution zero fps":       {"BERTH_RESOLUTION": "1280x720@0"},
		"resolution leading zero":   {"BERTH_RESOLUTION": "01280x720@30"},
		"public ip not an ip":       {"BERTH_PUBLIC_IP": "example.com"},
		"public ip with port":       {"BERTH_PUBLIC_IP": "1.2.3.4:80"},
		"udp min below 1024":        {"BERTH_UDP_PORT_MIN": "80"},
		"udp max above 65535":       {"BERTH_UDP_PORT_MAX": "70000"},
		"udp range too small":       {"BERTH_UDP_PORT_MIN": "30000", "BERTH_UDP_PORT_MAX": "30003", "BERTH_UDP_PORTS_PER_WORKSPACE": "8"},
		"udp inverted":              {"BERTH_UDP_PORT_MIN": "31000", "BERTH_UDP_PORT_MAX": "30000"},
		"zero ports per workspace":  {"BERTH_UDP_PORTS_PER_WORKSPACE": "0"},
		"shm too small":             {"BERTH_SHM_MB": "10"},
		"pids too small":            {"BERTH_PIDS_LIMIT": "1"},
		"pids absurd":               {"BERTH_PIDS_LIMIT": "99999999"},
		"stop timeout too short":    {"BERTH_STOP_TIMEOUT": "100ms"},
		"stop timeout too long":     {"BERTH_STOP_TIMEOUT": "1h"},
		"start timeout too short":   {"BERTH_START_TIMEOUT": "1s"},
		"start timeout not a time":  {"BERTH_START_TIMEOUT": "soon"},
		"attach interval too short": {"BERTH_PROXY_ATTACH_INTERVAL": "500ms"},
		"attach interval too long":  {"BERTH_PROXY_ATTACH_INTERVAL": "1h"},

		"idle timeout under a minute":  {"BERTH_IDLE_TIMEOUT": "59s"},
		"idle timeout zero":            {"BERTH_IDLE_TIMEOUT": "0s"},
		"idle timeout negative":        {"BERTH_IDLE_TIMEOUT": "-30m"},
		"idle timeout absurd":          {"BERTH_IDLE_TIMEOUT": "100000h"},
		"idle timeout not a duration":  {"BERTH_IDLE_TIMEOUT": "30"},
		"reclaim under a minute":       {"BERTH_RECLAIM_AFTER": "30s"},
		"reclaim negative":             {"BERTH_RECLAIM_AFTER": "-1h"},
		"reclaim absurd":               {"BERTH_RECLAIM_AFTER": "100000h"},
		"reaper interval under 5s":     {"BERTH_REAPER_INTERVAL": "4s"},
		"reaper interval over an hour": {"BERTH_REAPER_INTERVAL": "61m"},
		"idle cpu negative":            {"BERTH_IDLE_CPU_PERCENT": "-1"},
		"idle cpu NaN":                 {"BERTH_IDLE_CPU_PERCENT": "NaN"},
		"idle cpu Inf":                 {"BERTH_IDLE_CPU_PERCENT": "Inf"},
		"idle cpu over 1000":           {"BERTH_IDLE_CPU_PERCENT": "1001"},
		"idle cpu not a number":        {"BERTH_IDLE_CPU_PERCENT": "five"},
	}
	for name, e := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(env(e)); err == nil {
				t.Fatalf("expected an error for %v", e)
			}
		})
	}
	// The limits themselves are accepted.
	ok := map[string]string{
		"BERTH_UDP_PORT_MIN": "1024", "BERTH_UDP_PORT_MAX": "65535", "BERTH_UDP_PORTS_PER_WORKSPACE": "64512",
		"BERTH_SHM_MB": "64", "BERTH_PIDS_LIMIT": "64",
		"BERTH_STOP_TIMEOUT": "1s", "BERTH_START_TIMEOUT": "5s", "BERTH_PROXY_ATTACH_INTERVAL": "2s",
	}
	if _, err := Load(env(ok)); err != nil {
		t.Fatalf("boundary values rejected: %v", err)
	}
}

func TestReaperSettings(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"BERTH_IDLE_TIMEOUT":     "1m",
		"BERTH_RECLAIM_AFTER":    "90s",
		"BERTH_REAPER_INTERVAL":  "5s",
		"BERTH_IDLE_CPU_PERCENT": "12.5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleTimeout != time.Minute || cfg.ReclaimAfter != 90*time.Second || cfg.ReaperInterval != 5*time.Second ||
		cfg.IdleCPUPercent != 12.5 {
		t.Fatalf("reaper settings not applied: %+v", cfg)
	}
	// The limits themselves are accepted: an hour between looks, ten years of
	// patience, the CPU check switched off or at its maximum.
	for _, e := range []map[string]string{
		{"BERTH_IDLE_TIMEOUT": "87600h", "BERTH_RECLAIM_AFTER": "87600h", "BERTH_REAPER_INTERVAL": "1h", "BERTH_IDLE_CPU_PERCENT": "0"},
		{"BERTH_IDLE_CPU_PERCENT": "1000"},
	} {
		if _, err := Load(env(e)); err != nil {
			t.Errorf("boundary values %v rejected: %v", e, err)
		}
	}
	if cfg, err := Load(env(map[string]string{"BERTH_IDLE_CPU_PERCENT": "0"})); err != nil || cfg.IdleCPUPercent != 0 {
		t.Fatalf("0 must disable the CPU check, not fall back to the default: %+v, %v", cfg, err)
	}
}
