package workspace

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/docker"
)

func TestLabelsForAUser(t *testing.T) {
	l := Labels("alice", "berth-net-alice", "web")
	want := map[string]string{
		docker.LabelManaged: "true",
		docker.LabelUser:    "alice",

		"traefik.enable":         "true",
		"traefik.docker.network": "berth-net-alice",

		"traefik.http.routers.alice-term.entrypoints":                    "web",
		"traefik.http.routers.alice-term.rule":                           "PathPrefix(`/ws/alice/terminal`)",
		"traefik.http.routers.alice-term.middlewares":                    "berth-auth@file,berth-ws-headers@file,alice-term-strip",
		"traefik.http.routers.alice-term.service":                        "alice-term",
		"traefik.http.middlewares.alice-term-strip.stripprefix.prefixes": "/ws/alice/terminal",
		"traefik.http.services.alice-term.loadbalancer.server.port":      "7681",

		"traefik.http.routers.alice-browser.entrypoints":               "web",
		"traefik.http.routers.alice-browser.rule":                      "PathPrefix(`/ws/alice/browser`)",
		"traefik.http.routers.alice-browser.middlewares":               "berth-auth@file,berth-ws-headers@file",
		"traefik.http.routers.alice-browser.service":                   "alice-browser",
		"traefik.http.services.alice-browser.loadbalancer.server.port": "8080",
	}
	for k, v := range want {
		if got, ok := l[k]; !ok || got != v {
			t.Errorf("label %s = %q (present=%v), want %q", k, got, ok, v)
		}
	}
	for k := range l {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected label %s=%q", k, l[k])
		}
	}
}

// The order of `middlewares` is load-bearing: ForwardAuth sees the URL as it is
// when it runs, so it must run before StripPrefix, and the response-header
// middleware sits between them (after the auth check, before the URL is
// rewritten).
func TestMiddlewareOrder(t *testing.T) {
	l := Labels("alice", "berth-net-alice", "web")
	// The literal middleware names, not the constants: they are what dynamic.yml
	// defines and what Traefik resolves.
	chain := strings.Split(l["traefik.http.routers.alice-term.middlewares"], ",")
	if len(chain) != 3 || chain[0] != "berth-auth@file" || chain[1] != "berth-ws-headers@file" || chain[2] != "alice-term-strip" {
		t.Fatalf("terminal middleware chain = %v, want [berth-auth@file berth-ws-headers@file alice-term-strip]", chain)
	}
	chain = strings.Split(l["traefik.http.routers.alice-browser.middlewares"], ",")
	if len(chain) != 2 || chain[0] != "berth-auth@file" || chain[1] != "berth-ws-headers@file" {
		t.Fatalf("browser middleware chain = %v, want [berth-auth@file berth-ws-headers@file]", chain)
	}
	if AuthMiddleware != "berth-auth@file" || WSHeadersMiddleware != "berth-ws-headers@file" {
		t.Errorf("middleware constants = %q, %q", AuthMiddleware, WSHeadersMiddleware)
	}

	// Every router that is declared has middlewares declared, starts with the
	// auth check and carries the response headers: neither may exist without them.
	routers := map[string]bool{}
	for k := range l {
		if rest, ok := strings.CutPrefix(k, "traefik.http.routers."); ok {
			routers[strings.SplitN(rest, ".", 2)[0]] = true
		}
	}
	if len(routers) != 2 {
		t.Errorf("routers = %v, want the terminal and the browser router", routers)
	}
	for r := range routers {
		mw, ok := l["traefik.http.routers."+r+".middlewares"]
		if !ok {
			t.Errorf("router %s has no middlewares", r)
			continue
		}
		list := strings.Split(mw, ",")
		if list[0] != AuthMiddleware {
			t.Errorf("router %s middlewares %q do not start with the auth middleware", r, mw)
		}
		if !slices.Contains(list, WSHeadersMiddleware) {
			t.Errorf("router %s middlewares %q lack the response-header middleware", r, mw)
		}
		// The strip middleware, where there is one, comes last: it rewrites the URL.
		for i, name := range list {
			if strings.HasSuffix(name, "-strip") && i != len(list)-1 {
				t.Errorf("router %s: %s is not the last middleware: %q", r, name, mw)
			}
		}
	}
}

// Two users must never share a router, service or middleware name, and neither
// may collide with the names the compose file defines.
func TestLabelNamesNeverCollideAcrossUsers(t *testing.T) {
	names := []string{"alice", "bob", "a-b", "a", "a-term", "a-browser", "berth", "berth-api", "berth-auth", "x-term-strip"}
	owner := map[string]string{} // object name -> user
	reserved := map[string]bool{"berth-api": true, "berth-auth": true}
	for _, u := range names {
		if !auth.ValidUsername(u) && len(u) >= auth.MinUsernameLen {
			t.Fatalf("test username %q is not valid", u)
		}
		for k := range Labels(u, NetworkName(u), "web") {
			parts := strings.Split(k, ".")
			if len(parts) < 4 || parts[0] != "traefik" || parts[1] != "http" {
				continue
			}
			kind, name := parts[2], parts[3] // routers|services|middlewares + the object's name
			id := kind + "/" + name
			if prev, ok := owner[id]; ok && prev != u {
				t.Errorf("%s is defined by both %q and %q", id, prev, u)
			}
			owner[id] = u
			if reserved[name] {
				t.Errorf("user %q defines %s, which the compose file owns", u, id)
			}
		}
	}
}

func TestLabelsCarryOnlyThisUsersPaths(t *testing.T) {
	for _, u := range []string{"alice", "bob"} {
		for k, v := range Labels(u, NetworkName(u), "web") {
			for _, other := range []string{"alice", "bob"} {
				if other != u && strings.Contains(v, other) && !strings.Contains(v, u) {
					t.Errorf("%s's label %s=%q mentions %s", u, k, v, other)
				}
			}
			if strings.Contains(k, ".rule") && !strings.Contains(v, "/ws/"+u+"/") {
				t.Errorf("rule %s=%q is not scoped to /ws/%s/", k, v, u)
			}
		}
	}
}

func TestEntrypointIsConfigurable(t *testing.T) {
	l := Labels("alice", "n", "websecure")
	if l["traefik.http.routers.alice-term.entrypoints"] != "websecure" || l["traefik.http.routers.alice-browser.entrypoints"] != "websecure" {
		t.Fatalf("entrypoints: %v", l)
	}
}

func TestNekoEnv(t *testing.T) {
	cfg := testConfig()
	cfg.PublicIP = "203.0.113.7"
	env := nekoEnv(cfg, "alice", "userpw", "adminpw", 30008, 30015)
	sort.Strings(env)
	want := []string{
		"NEKO_DESKTOP_SCREEN=1280x720@30",
		"NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD=adminpw",
		"NEKO_MEMBER_MULTIUSER_USER_PASSWORD=userpw",
		"NEKO_SERVER_METRICS=false",
		"NEKO_SERVER_PATH_PREFIX=/ws/alice/browser",
		"NEKO_SERVER_PROXY=true",
		"NEKO_SESSION_IMPLICIT_HOSTING=true",
		"NEKO_WEBRTC_EPR=30008-30015",
		"NEKO_WEBRTC_NAT1TO1=203.0.113.7",
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env:\n%s\nwant:\n%s", strings.Join(env, "\n"), strings.Join(want, "\n"))
	}
	// Without a public IP the NAT setting is left to neko; without a port block
	// no range is advertised.
	cfg.PublicIP = ""
	if v := envValue(nekoEnv(cfg, "alice", "u", "a", 0, 0), envNekoNAT1TO1); v != "" {
		t.Errorf("NAT1TO1 set without a public IP: %q", v)
	}
	for _, key := range []string{envNekoEPR, envNekoUDPMux} {
		if v := envValue(nekoEnv(cfg, "alice", "u", "a", 0, 0), key); v != "" {
			t.Errorf("%s set without a port block: %q", key, v)
		}
	}
}

// One port per workspace (the default) is multiplexed over, not a one-port range.
// The assertions use the literal environment strings, not the constants: the
// names are neko's, and a typo in a constant would otherwise go unnoticed while
// neko ignored the setting and used unpublished ephemeral UDP ports (video that
// never connects for a browser off the host).
func TestNekoEnvOnePortUsesTheMultiplexer(t *testing.T) {
	cfg := testConfig()
	env := nekoEnv(cfg, "alice", "userpw", "adminpw", 30005, 30005)
	sort.Strings(env)
	want := []string{
		"NEKO_DESKTOP_SCREEN=1280x720@30",
		"NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD=adminpw",
		"NEKO_MEMBER_MULTIUSER_USER_PASSWORD=userpw",
		"NEKO_SERVER_METRICS=false",
		"NEKO_SERVER_PATH_PREFIX=/ws/alice/browser",
		"NEKO_SERVER_PROXY=true",
		"NEKO_SESSION_IMPLICIT_HOSTING=true",
		"NEKO_WEBRTC_UDPMUX=30005",
	}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env:\n%s\nwant:\n%s", strings.Join(env, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range env {
		if strings.HasPrefix(e, "NEKO_WEBRTC_EPR") {
			t.Errorf("%s: a single port must use the multiplexer, not a range", e)
		}
	}

	// With a public IP the NAT setting joins it, under neko's literal name.
	cfg.PublicIP = "203.0.113.7"
	env = nekoEnv(cfg, "alice", "u", "a", 30005, 30005)
	if !slices.Contains(env, "NEKO_WEBRTC_UDPMUX=30005") || !slices.Contains(env, "NEKO_WEBRTC_NAT1TO1=203.0.113.7") {
		t.Errorf("env %v lacks NEKO_WEBRTC_UDPMUX=30005 or NEKO_WEBRTC_NAT1TO1=203.0.113.7", env)
	}

	// Two ports or more: a range, no multiplexer.
	env = nekoEnv(cfg, "alice", "u", "a", 30005, 30006)
	if !slices.Contains(env, "NEKO_WEBRTC_EPR=30005-30006") {
		t.Errorf("env %v lacks NEKO_WEBRTC_EPR=30005-30006", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "NEKO_WEBRTC_UDPMUX") {
			t.Errorf("%s set for a range", e)
		}
	}

	// No block (the zero values): neither setting.
	for _, e := range nekoEnv(cfg, "alice", "u", "a", 0, 0) {
		if strings.HasPrefix(e, "NEKO_WEBRTC_UDPMUX") || strings.HasPrefix(e, "NEKO_WEBRTC_EPR") {
			t.Errorf("%s set without a port block", e)
		}
	}
}
