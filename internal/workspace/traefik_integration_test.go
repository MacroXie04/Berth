//go:build integration

// Runs the real Traefik binary in front of stand-ins for ttyd and neko and the
// real Berth API, with the routers built from workspace.Labels and the middleware
// file traefik/dynamic.yml exactly as shipped (only the addresses of the
// backends are rewritten). It proves what the label strings and the YAML alone
// cannot: that Traefik accepts the middleware chain, runs it in the order that
// keeps ForwardAuth working, and sends and removes the headers.
//
//	BERTH_TEST_TRAEFIK=/path/to/traefik go test -tags integration -run Traefik ./internal/workspace
//
// Without BERTH_TEST_TRAEFIK the tests are skipped. Traefik listens on a free
// loopback port; no Docker daemon is involved.
package workspace_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/MacroXie04/Berth/internal/api"
	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/webui"
	"github.com/MacroXie04/Berth/internal/workspace"
)

const hostileSWA = "/"

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// upstream stands in for ttyd or neko. It echoes the path it saw, sends a
// hostile Service-Worker-Allowed on every response, permissive framing headers on
// /hostile, and echoes WebSocket frames on /ws.
func upstream(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			echoWebSocket(w, r)
			return
		}
		h := w.Header()
		h.Set("Service-Worker-Allowed", hostileSWA)
		h.Set("X-Seen-Path", r.URL.Path)
		h.Set("Content-Type", "text/html")
		if strings.HasSuffix(r.URL.Path, "/hostile") {
			h.Set("X-Frame-Options", "ALLOWALL")
			h.Set("Content-Security-Policy", "frame-ancestors *")
		}
		fmt.Fprintf(w, "<title>%s</title>%s", name, name)
	})
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func echoWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\nService-Worker-Allowed: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]), hostileSWA)
	rw.Flush()
	for {
		var head [2]byte
		if _, err := io.ReadFull(rw, head[:]); err != nil {
			return
		}
		n := int(head[1] & 0x7f)
		var mask [4]byte
		if head[1]&0x80 != 0 {
			if _, err := io.ReadFull(rw, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(rw, payload); err != nil {
			return
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		rw.Write(append([]byte{0x81, byte(n)}, payload...))
		rw.Flush()
	}
}

// wsProbe performs a WebSocket handshake through Traefik. On a 101 it sends one
// text frame and returns what comes back.
func wsProbe(t *testing.T, addr, path, cookie string) (status int, header http.Header, echo string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	var key [16]byte
	rand.Read(key[:])
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key[:]))
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Origin", "http://"+addr)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("websocket handshake on %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return resp.StatusCode, resp.Header, ""
	}
	msg := []byte("hello-through-traefik")
	var mask [4]byte
	rand.Read(mask[:])
	frame := append([]byte{0x81, 0x80 | byte(len(msg))}, mask[:]...)
	for i, b := range msg {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		t.Fatalf("websocket echo on %s: %v", path, err)
	}
	payload := make([]byte, head[1]&0x7f)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(payload)
}

// fileConfigFromLabels turns the Docker labels of one workspace into the same
// routers, middlewares and services in the file provider's format. Anything it
// does not understand is a test failure, so a new label cannot slip past unchecked.
func fileConfigFromLabels(t *testing.T, labels map[string]string, servicePorts map[string]string) map[string]any {
	t.Helper()
	routers, middlewares, services := map[string]any{}, map[string]any{}, map[string]any{}
	router := func(name string) map[string]any {
		if routers[name] == nil {
			routers[name] = map[string]any{}
		}
		return routers[name].(map[string]any)
	}
	for k, v := range labels {
		parts := strings.Split(k, ".")
		if len(parts) < 4 || parts[0] != "traefik" || parts[1] != "http" {
			continue // ownership labels, traefik.enable, traefik.docker.network
		}
		kind, name, rest := parts[2], parts[3], strings.Join(parts[4:], ".")
		switch {
		case kind == "routers" && rest == "entrypoints":
			router(name)["entryPoints"] = strings.Split(v, ",")
		case kind == "routers" && rest == "middlewares":
			router(name)["middlewares"] = strings.Split(v, ",")
		case kind == "routers" && (rest == "rule" || rest == "service"):
			router(name)[rest] = v
		case kind == "middlewares" && rest == "stripprefix.prefixes":
			middlewares[name] = map[string]any{"stripPrefix": map[string]any{"prefixes": strings.Split(v, ",")}}
		case kind == "services" && rest == "loadbalancer.server.port":
			url, ok := servicePorts[v]
			if !ok {
				t.Fatalf("label %s=%s: no stand-in for that port", k, v)
			}
			services[name] = map[string]any{"loadBalancer": map[string]any{"servers": []any{map[string]any{"url": url}}}}
		default:
			t.Fatalf("label %s=%s: not understood by this test's converter", k, v)
		}
	}
	return map[string]any{"http": map[string]any{"routers": routers, "middlewares": middlewares, "services": services}}
}

type edge struct {
	t       *testing.T
	addr    string
	cookies map[string]string // username -> "berth_ws=<token>"
}

type reply struct {
	status int
	header http.Header
	body   string
}

func (e *edge) get(path, user string) reply {
	e.t.Helper()
	req, err := http.NewRequest("GET", "http://"+e.addr+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("Cookie", e.cookies[user])
	}
	return e.do(req)
}

func (e *edge) do(req *http.Request) reply {
	e.t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, string(body)}
}

func (e *edge) post(path, body string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("POST", "http://"+e.addr+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func startTraefik(t *testing.T) *edge {
	t.Helper()
	bin := os.Getenv("BERTH_TEST_TRAEFIK")
	if bin == "" {
		t.Skip("set BERTH_TEST_TRAEFIK to a traefik v3 binary to run this test")
	}
	dir := t.TempDir()
	edgeAddr := freeAddr(t)

	// Backends. The API is the real one, over a temporary database.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "berth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hasher, err := auth.NewHasher(bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	apiSrv := httptest.NewServer(api.New(api.Deps{
		Store: st, Hasher: hasher, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		SessionTTL: time.Hour, DefaultMemLimitMB: 2048, DefaultCPULimit: 2,
	}).Handler())
	t.Cleanup(apiSrv.Close)
	ttyd, neko := httptest.NewServer(upstream("TTYD")), httptest.NewServer(upstream("NEKO"))
	t.Cleanup(ttyd.Close)
	t.Cleanup(neko.Close)
	spaHandler, err := webui.New(fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>SPA</title>")}})
	if err != nil {
		t.Fatal(err)
	}
	spa := httptest.NewServer(spaHandler)
	t.Cleanup(spa.Close)

	// Dynamic configuration: the shipped middleware file with only the backend's
	// address replaced, plus the workspace routers from the real labels and the
	// two routers docker-compose.yml puts on the berth service.
	dyn, err := os.ReadFile(filepath.Join("..", "..", "traefik", "dynamic.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dyn, []byte("http://berth:8080")) {
		t.Fatal("traefik/dynamic.yml no longer names http://berth:8080: update this test")
	}
	dyn = bytes.ReplaceAll(dyn, []byte("http://berth:8080"), []byte(apiSrv.URL))
	dynDir := filepath.Join(dir, "dyn")
	if err := os.MkdirAll(dynDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dynDir, "dynamic.yml"), dyn, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := fileConfigFromLabels(t, workspace.Labels("alice", workspace.NetworkName("alice"), "web"), map[string]string{
		fmt.Sprint(workspace.TerminalPort): ttyd.URL,
		fmt.Sprint(workspace.BrowserPort):  neko.URL,
	})
	h := cfg["http"].(map[string]any)
	h["routers"].(map[string]any)["berth-api"] = map[string]any{
		"entryPoints": []string{"web"}, "rule": "PathPrefix(`/api`) && !Path(`/api/auth/check`)", "service": "berth-api"}
	h["routers"].(map[string]any)["berth-web"] = map[string]any{
		"entryPoints": []string{"web"}, "priority": 1, "service": "berth-spa",
		"rule": "PathPrefix(`/`) && !PathPrefix(`/api`) && !PathPrefix(`/healthz`) && !PathPrefix(`/ws`)"}
	svc := func(url string) map[string]any {
		return map[string]any{"loadBalancer": map[string]any{"servers": []any{map[string]any{"url": url}}}}
	}
	h["services"].(map[string]any)["berth-api"] = svc(apiSrv.URL)
	h["services"].(map[string]any)["berth-spa"] = svc(spa.URL)
	raw, _ := json.MarshalIndent(cfg, "", " ") // JSON is YAML
	if err := os.WriteFile(filepath.Join(dynDir, "workspaces.yml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	logFile, err := os.Create(filepath.Join(dir, "traefik.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin,
		"--entrypoints.web.address="+edgeAddr,
		"--entrypoints.web.http.encodedcharacters.allowencodedslash=false",
		"--entrypoints.web.http.encodedcharacters.allowencodedbackslash=false",
		"--entrypoints.web.http.encodedcharacters.allowencodednullcharacter=false",
		"--entrypoints.web.http.encodedcharacters.allowencodedsemicolon=false",
		"--entrypoints.web.http.aliasheadersstrategy=delete",
		"--providers.file.directory="+dynDir, "--providers.file.watch=false",
		"--log.level=DEBUG", "--global.checknewversion=false", "--global.sendanonymoususage=false")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(logFile.Name())
			if len(b) > 6000 {
				b = b[len(b)-6000:]
			}
			t.Logf("traefik log (tail):\n%s", b)
		}
	})

	e := &edge{t: t, addr: edgeAddr, cookies: map[string]string{}}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if resp, err := http.Get("http://" + edgeAddr + "/api/me"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("traefik did not come up")
		}
		time.Sleep(100 * time.Millisecond)
	}

	for _, u := range []struct{ name, pw string }{{"alice", "correct horse"}, {"bob", "battery staple"}} {
		body := fmt.Sprintf(`{"username":%q,"password":%q}`, u.name, u.pw)
		for _, step := range []string{"register", "login"} {
			resp := e.post("/api/auth/"+step, body)
			resp.Body.Close()
			if resp.StatusCode != 200 && resp.StatusCode != 201 {
				t.Fatalf("%s %s: %d", step, u.name, resp.StatusCode)
			}
			for _, c := range resp.Cookies() {
				if c.Name == "berth_ws" {
					e.cookies[u.name] = "berth_ws=" + c.Value
				}
			}
		}
		if e.cookies[u.name] == "" {
			t.Fatalf("login of %s set no berth_ws cookie", u.name)
		}
	}
	return e
}

func TestTraefikWorkspaceRoutes(t *testing.T) {
	e := startTraefik(t)
	const frameAncestors = "frame-ancestors 'self'"

	// An allowed response carries the headers, minus the hostile one, and each
	// upstream saw the path it expects (only the terminal's prefix is stripped).
	for _, tc := range []struct{ name, path, upstream, seen string }{
		{"terminal", "/ws/alice/terminal/", "TTYD", "/"},
		{"terminal asset", "/ws/alice/terminal/some/file.js", "TTYD", "/some/file.js"},
		{"browser", "/ws/alice/browser/", "NEKO", "/ws/alice/browser/"},
		{"browser asset", "/ws/alice/browser/static/app.js", "NEKO", "/ws/alice/browser/static/app.js"},
	} {
		t.Run("headers/"+tc.name, func(t *testing.T) {
			r := e.get(tc.path, "alice")
			if r.status != 200 || !strings.Contains(r.body, tc.upstream) {
				t.Fatalf("status %d body %q", r.status, r.body)
			}
			if got := r.header.Values("X-Frame-Options"); len(got) != 1 || got[0] != "SAMEORIGIN" {
				t.Errorf("X-Frame-Options = %q, want exactly SAMEORIGIN", got)
			}
			if got := r.header.Values("Content-Security-Policy"); len(got) != 1 || got[0] != frameAncestors {
				t.Errorf("Content-Security-Policy = %q, want exactly %q", got, frameAncestors)
			}
			if got := r.header.Values("Service-Worker-Allowed"); len(got) != 0 {
				t.Errorf("Service-Worker-Allowed = %q: the upstream's header must be removed", got)
			}
			if got := r.header.Get("X-Seen-Path"); got != tc.seen {
				t.Errorf("upstream saw %q, want %q", got, tc.seen)
			}
		})
	}

	// An upstream that asks for permissive framing gets overridden, not merged.
	for _, path := range []string{"/ws/alice/terminal/hostile", "/ws/alice/browser/hostile"} {
		r := e.get(path, "alice")
		if r.status != 200 {
			t.Fatalf("%s: %d", path, r.status)
		}
		if got := r.header.Values("X-Frame-Options"); len(got) != 1 || got[0] != "SAMEORIGIN" {
			t.Errorf("%s: X-Frame-Options = %q, want exactly SAMEORIGIN", path, got)
		}
		if got := r.header.Values("Content-Security-Policy"); len(got) != 1 || got[0] != frameAncestors {
			t.Errorf("%s: Content-Security-Policy = %q, want exactly %q", path, got, frameAncestors)
		}
		if got := r.header.Values("Service-Worker-Allowed"); len(got) != 0 {
			t.Errorf("%s: Service-Worker-Allowed = %q", path, got)
		}
	}

	// The auth check runs first, on the URL as the client sent it: if StripPrefix
	// (or anything else) ran before it, the owner would get a 403.
	for _, tc := range []struct {
		name, path, user string
		want             int
	}{
		{"terminal, anonymous", "/ws/alice/terminal/", "", 401},
		{"terminal, someone else", "/ws/alice/terminal/", "bob", 403},
		{"terminal, owner", "/ws/alice/terminal/", "alice", 200},
		{"browser, anonymous", "/ws/alice/browser/", "", 401},
		{"browser, someone else", "/ws/alice/browser/", "bob", 403},
		{"browser, owner", "/ws/alice/browser/", "alice", 200},
		// No router exists for these: the catch-all runs the same check.
		{"no workspace, anonymous", "/ws/carol/terminal/", "", 401},
		{"no workspace, other user", "/ws/carol/terminal/", "alice", 403},
		{"bob's workspace is down, anonymous", "/ws/bob/browser/", "", 401},
		{"bob's workspace is down, alice", "/ws/bob/browser/", "alice", 403},
		{"bob's workspace is down, bob", "/ws/bob/browser/", "bob", 404},
		{"unknown section, owner", "/ws/alice/other/", "alice", 403},
		{"encoded slash", "/ws/alice/terminal/..%2f..%2fbob%2fterminal/", "alice", 400},
	} {
		if r := e.get(tc.path, tc.user); r.status != tc.want {
			t.Errorf("%s: GET %s as %q = %d (%s), want %d", tc.name, tc.path, tc.user, r.status, strings.TrimSpace(r.body), tc.want)
		}
	}

	// The catch-all's own answer (the backend's 404 for an owner whose workspace
	// is down) carries the same response headers.
	r := e.get("/ws/bob/terminal/", "bob")
	if r.status != 404 || strings.Join(r.header.Values("X-Frame-Options"), "|") != "SAMEORIGIN" ||
		strings.Join(r.header.Values("Content-Security-Policy"), "|") != frameAncestors {
		t.Errorf("catch-all 404: %d XFO=%q CSP=%q", r.status, r.header.Values("X-Frame-Options"), r.header.Values("Content-Security-Policy"))
	}

	// ForwardAuth's own 401/403 are untouched by the chain behind it.
	for _, tc := range []struct {
		path, user string
		want       int
	}{{"/ws/alice/terminal/", "", 401}, {"/ws/alice/browser/", "bob", 403}, {"/ws/carol/terminal/", "", 401}} {
		r := e.get(tc.path, tc.user)
		if r.status != tc.want || !strings.Contains(r.body, `"error"`) || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s as %q: %d %q Cache-Control=%q, want the backend's %d JSON error with no-store",
				tc.path, tc.user, r.status, r.body, r.header.Get("Cache-Control"), tc.want)
		}
	}

	// WebSockets still upgrade through auth, headers and strip.
	status, _, echo := wsProbe(t, e.addr, "/ws/alice/terminal/ws", e.cookies["alice"])
	if status != 101 || echo != "hello-through-traefik" {
		t.Errorf("terminal WebSocket as owner: %d %q, want 101 and the echo", status, echo)
	}
	if status, _, _ := wsProbe(t, e.addr, "/ws/alice/terminal/ws", ""); status != 401 {
		t.Errorf("terminal WebSocket without a cookie: %d, want 401", status)
	}
	if status, _, _ := wsProbe(t, e.addr, "/ws/alice/terminal/ws", e.cookies["bob"]); status != 403 {
		t.Errorf("terminal WebSocket as someone else: %d, want 403", status)
	}
	if status, _, echo := wsProbe(t, e.addr, "/ws/alice/browser/ws", e.cookies["alice"]); status != 101 || echo != "hello-through-traefik" {
		t.Errorf("browser WebSocket as owner: %d %q", status, echo)
	}

	// The app is not touched: its own headers stand, and it is not frameable.
	spa := e.get("/", "")
	if spa.status != 200 || spa.header.Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(spa.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("SPA: %d XFO=%q CSP=%q", spa.status, spa.header.Get("X-Frame-Options"), spa.header.Get("Content-Security-Policy"))
	}
}
