package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// traefik/dynamic.yml is configuration the labels depend on: Traefik resolves
// "berth-auth@file" and "berth-ws-headers@file" against it, and an unknown
// middleware name makes it drop the whole router (a 404 that tells an anonymous
// caller who has a workspace up). These tests read the real file.

func loadDynamicConfig(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "traefik", "dynamic.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseYAMLSubset(string(raw))
	if err != nil {
		t.Fatalf("traefik/dynamic.yml: %v", err)
	}
	return cfg
}

// get walks nested mappings by key and fails the test on a missing key.
func get(t *testing.T, root map[string]any, keys ...string) any {
	t.Helper()
	var v any = root
	for i, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("traefik/dynamic.yml: %s is not a mapping", strings.Join(keys[:i], "."))
		}
		next, ok := m[k]
		if !ok {
			t.Fatalf("traefik/dynamic.yml: %s is missing", strings.Join(keys[:i+1], "."))
		}
		v = next
	}
	return v
}

func TestDynamicConfigDefinesTheMiddlewaresTheLabelsUse(t *testing.T) {
	cfg := loadDynamicConfig(t)
	defined := get(t, cfg, "http", "middlewares").(map[string]any)

	for _, ref := range []string{AuthMiddleware, WSHeadersMiddleware} {
		name, ok := strings.CutSuffix(ref, "@file")
		if !ok {
			t.Fatalf("%q is not a file-provider reference", ref)
		}
		if _, ok := defined[name]; !ok {
			t.Errorf("middleware %s is used by the labels but not defined in traefik/dynamic.yml (defined: %v)", ref, keys(defined))
		}
	}
	// Every @file name any label mentions is defined.
	for k, v := range Labels("alice", "n", "web") {
		if !strings.HasSuffix(k, ".middlewares") {
			continue
		}
		for _, ref := range strings.Split(v, ",") {
			if name, ok := strings.CutSuffix(ref, "@file"); ok {
				if _, ok := defined[name]; !ok {
					t.Errorf("%s references %s, which dynamic.yml does not define", k, ref)
				}
			}
		}
	}
}

// berth-ws-headers is what keeps a same-site page from framing a victim's live
// terminal or remote browser, and a workspace from widening a service worker's
// scope. The values are the behaviour.
func TestWorkspaceResponseHeadersMiddleware(t *testing.T) {
	cfg := loadDynamicConfig(t)
	h := get(t, cfg, "http", "middlewares", "berth-ws-headers", "headers").(map[string]any)

	if got := h["customFrameOptionsValue"]; got != "SAMEORIGIN" {
		t.Errorf("customFrameOptionsValue = %v, want SAMEORIGIN (the SPA frames neko from the same origin; nobody else may)", got)
	}
	if got := h["contentSecurityPolicy"]; got != "frame-ancestors 'self'" {
		t.Errorf("contentSecurityPolicy = %v, want \"frame-ancestors 'self'\"", got)
	}
	custom, ok := h["customResponseHeaders"].(map[string]any)
	if !ok {
		t.Fatalf("customResponseHeaders = %v, want a mapping", h["customResponseHeaders"])
	}
	// An empty value is how Traefik deletes a response header; "/" or a missing
	// key would leave (or allow) a scope-widening header.
	if v, present := custom["Service-Worker-Allowed"]; !present || v != "" {
		t.Errorf("Service-Worker-Allowed = %q (present=%v), want present and empty (= deleted)", v, present)
	}
	for name := range h {
		switch name {
		case "customFrameOptionsValue", "contentSecurityPolicy", "customResponseHeaders":
		default:
			t.Errorf("unexpected setting %s in berth-ws-headers: update this test with a reason", name)
		}
	}
}

func TestForwardAuthMiddlewareSettings(t *testing.T) {
	cfg := loadDynamicConfig(t)
	fa := get(t, cfg, "http", "middlewares", "berth-auth", "forwardAuth").(map[string]any)
	if fa["trustForwardHeader"] != "false" {
		t.Errorf("trustForwardHeader = %v: a client must never choose the URI the check sees", fa["trustForwardHeader"])
	}
	if got, _ := fa["authRequestHeaders"].([]any); len(got) != 1 || got[0] != "Cookie" {
		t.Errorf("authRequestHeaders = %v, want only Cookie", fa["authRequestHeaders"])
	}
	if fa["address"] != "http://berth:8080/api/auth/check" {
		t.Errorf("address = %v", fa["address"])
	}
}

// The catch-all for /ws/ answers every path whose router does not exist; it runs
// the same check and carries the same response headers as a real workspace router.
func TestFallbackRouterHasTheSameMiddlewares(t *testing.T) {
	cfg := loadDynamicConfig(t)
	r := get(t, cfg, "http", "routers", "berth-ws-fallback").(map[string]any)
	got, _ := r["middlewares"].([]any)
	want := []any{"berth-auth", "berth-ws-headers"}
	if !slices.Equal(got, want) {
		t.Errorf("berth-ws-fallback middlewares = %v, want %v (auth first, then the response headers)", got, want)
	}
	if r["rule"] != "PathPrefix(`/ws/`)" || r["priority"] != "2" {
		t.Errorf("berth-ws-fallback rule=%v priority=%v", r["rule"], r["priority"])
	}
	// The labels name the same middlewares, with the @file suffix.
	for _, name := range got {
		ref := name.(string) + "@file"
		if ref != AuthMiddleware && ref != WSHeadersMiddleware {
			t.Errorf("fallback uses %s, which the labels do not", ref)
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// parseYAMLSubset reads the small YAML dialect traefik/dynamic.yml is written
// in: block mappings, block sequences of scalars, plain and quoted scalars,
// full-line comments. It exists so the test needs no YAML dependency; anything
// outside the dialect is an error instead of a silent misparse. Scalars come
// back as strings (the test compares "false", "2", ""), sequences as []any and
// mappings as map[string]any.
func parseYAMLSubset(src string) (map[string]any, error) {
	type line struct {
		indent int
		text   string
		no     int
	}
	var lines []line
	for i, raw := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ContainsRune(raw, '\t') {
			return nil, fmt.Errorf("line %d: tab", i+1)
		}
		lines = append(lines, line{len(raw) - len(strings.TrimLeft(raw, " ")), trimmed, i + 1})
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty document")
	}

	scalar := func(s string) (string, error) {
		switch {
		case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
			return s[1 : len(s)-1], nil
		case len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'':
			return s[1 : len(s)-1], nil
		case strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'") || strings.HasPrefix(s, "[") ||
			strings.HasPrefix(s, "{") || strings.HasPrefix(s, "|") || strings.HasPrefix(s, ">") ||
			strings.HasPrefix(s, "&") || strings.HasPrefix(s, "*"):
			return "", fmt.Errorf("unsupported scalar %q", s)
		}
		if i := strings.Index(s, " #"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		return s, nil
	}

	var block func(indent int) (any, error)
	pos := 0
	block = func(indent int) (any, error) {
		if pos >= len(lines) || lines[pos].indent != indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", lines[min(pos, len(lines)-1)].no)
		}
		if strings.HasPrefix(lines[pos].text, "- ") || lines[pos].text == "-" {
			var list []any
			for pos < len(lines) && lines[pos].indent == indent && strings.HasPrefix(lines[pos].text, "-") {
				v, err := scalar(strings.TrimSpace(strings.TrimPrefix(lines[pos].text, "-")))
				if err != nil {
					return nil, fmt.Errorf("line %d: %w", lines[pos].no, err)
				}
				list = append(list, v)
				pos++
			}
			return list, nil
		}
		m := map[string]any{}
		for pos < len(lines) && lines[pos].indent == indent {
			l := lines[pos]
			key, rest, ok := strings.Cut(l.text, ":")
			if !ok || key == "" || strings.ContainsAny(key, " \"'") {
				return nil, fmt.Errorf("line %d: not a simple `key: value` line: %q", l.no, l.text)
			}
			if _, dup := m[key]; dup {
				return nil, fmt.Errorf("line %d: duplicate key %q", l.no, key)
			}
			pos++
			rest = strings.TrimSpace(rest)
			if rest != "" {
				v, err := scalar(rest)
				if err != nil {
					return nil, fmt.Errorf("line %d: %w", l.no, err)
				}
				m[key] = v
				continue
			}
			if pos < len(lines) && lines[pos].indent > indent {
				v, err := block(lines[pos].indent)
				if err != nil {
					return nil, err
				}
				m[key] = v
			} else {
				m[key] = ""
			}
		}
		return m, nil
	}

	v, err := block(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if pos != len(lines) {
		return nil, fmt.Errorf("line %d: unexpected content", lines[pos].no)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top level is not a mapping")
	}
	return m, nil
}

func TestParseYAMLSubset(t *testing.T) {
	src := "# comment\na:\n  b: \"x y\"\n  c: ''\n  d:\n    - one\n    - two # note\n  e: `q`\nf: 2\n"
	m, err := parseYAMLSubset(src)
	if err != nil {
		t.Fatal(err)
	}
	a := m["a"].(map[string]any)
	if a["b"] != "x y" || a["c"] != "" || a["e"] != "`q`" || m["f"] != "2" {
		t.Errorf("parsed %v", m)
	}
	if d := a["d"].([]any); len(d) != 2 || d[0] != "one" || d[1] != "two" {
		t.Errorf("list = %v", d)
	}
	for _, bad := range []string{"a: [1, 2]\n", "a: &x 1\n", "a:\n\tb: 1\n", "a: 1\na: 2\n", "just text\n", "  a: 1\nb: 2\n", ""} {
		if _, err := parseYAMLSubset(bad); err == nil {
			t.Errorf("parseYAMLSubset(%q) succeeded", bad)
		}
	}
}
