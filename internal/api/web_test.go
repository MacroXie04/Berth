package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/MacroXie04/Berth/internal/webui"
)

// stubWeb stands in for the single-page app and records every path it was
// asked for. It has no path checks of its own, so a request that reaches it is
// a request the router handed to the app.
type stubWeb struct {
	mu    sync.Mutex
	paths []string
}

func (s *stubWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	w.Header().Set("X-Web", "1")
	io.WriteString(w, "WEB")
}

func (s *stubWeb) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func withWeb(h http.Handler) func(*Deps) { return func(d *Deps) { d.Web = h } }

func isJSON(r resp) bool {
	return strings.HasPrefix(r.Header().Get("Content-Type"), "application/json")
}

func TestWebServesGetAndHeadThatNoRouteClaims(t *testing.T) {
	web := &stubWeb{}
	e := newEnv(t, withWeb(web))
	for _, tc := range []struct{ method, path string }{
		{"GET", "/"}, {"HEAD", "/"}, {"GET", "/workspace"}, {"GET", "/admin/users"}, {"GET", "/assets/index-1.js"},
		{"GET", "/favicon.ico"}, {"GET", "/nope"}, {"GET", "/apix"}, {"GET", "/wsx"}, {"GET", "/healthzz"}, {"GET", "/workspace?x=1"},
	} {
		r := e.do(tc.method, tc.path, "", nil)
		if r.Code != 200 || r.Header().Get("X-Web") != "1" {
			t.Errorf("%s %s: %d, not served by the app", tc.method, tc.path, r.Code)
		}
		if r.Header().Get("X-Request-Id") == "" {
			t.Errorf("%s %s: the app's responses must carry the request id", tc.method, tc.path)
		}
	}
}

// The app must never answer for the API, the health probe or the workspaces,
// in any spelling: a request the router did not map to a route lands in the
// fallback, so the fallback has to refuse these itself. The stub has no guard of
// its own, so any hit is a leak.
func TestWebNeverShadowsTheBackend(t *testing.T) {
	web := &stubWeb{}
	e := newEnv(t, withWeb(web))
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api", 404}, {"GET", "/api/", 404}, {"GET", "/api/nope", 404}, {"GET", "/api/nope.js", 404},
		{"GET", "/api/auth/login", 405}, {"HEAD", "/api/me", 405}, {"GET", "/api/me", 401}, {"GET", "/api/auth/check", 401},
		{"GET", "/api/workspace/", 401},
		{"GET", "/healthz/", 404}, {"GET", "/healthz/x", 404}, {"HEAD", "/healthz", 405}, {"POST", "/healthz", 405},
		{"GET", "/ws", 404}, {"GET", "/ws/", 404}, {"GET", "/ws/alice/terminal/", 404}, {"HEAD", "/ws/alice/browser/", 404},
		// spellings the router does not map to /api, /healthz or /ws
		{"GET", "/API/me", 404}, {"GET", "/Api", 404}, {"GET", "/WS/alice/terminal/", 404}, {"GET", "/HEALTHZ", 404},
		{"GET", "/%61pi/auth/check", 404}, {"GET", "/%61pi/me", 404}, {"GET", "/api%2Fauth%2Fcheck", 404}, {"GET", "/api%2fme", 404},
		{"GET", "/%77s/alice/terminal/", 404}, {"GET", "/%68ealthz", 404},
		{"GET", "//api/auth/check", 404}, {"GET", "///api/me", 404}, {"GET", "//ws/alice/terminal/", 404}, {"GET", "//healthz", 404},
	} {
		r := e.do(tc.method, tc.path, "", nil)
		if r.Code != tc.status {
			t.Errorf("%s %s: %d, want %d (%s)", tc.method, tc.path, r.Code, tc.status, r.Body)
		}
		if !isJSON(r) {
			t.Errorf("%s %s: Content-Type %q, want JSON", tc.method, tc.path, r.Header().Get("Content-Type"))
		}
	}
	if calls := web.calls(); len(calls) != 0 {
		t.Errorf("the app was asked for backend paths: %v", calls)
	}
}

// Only a GET or HEAD can be an app navigation; anything else that no route
// claims keeps the JSON 404 it always had.
func TestWebIgnoresOtherMethods(t *testing.T) {
	web := &stubWeb{}
	e := newEnv(t, withWeb(web))
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		for _, p := range []string{"/", "/workspace", "/assets/index-1.js"} {
			r := e.do(method, p, "", nil)
			if r.Code != 404 || !isJSON(r) {
				t.Errorf("%s %s: %d %q", method, p, r.Code, r.Header().Get("Content-Type"))
			}
		}
	}
	if calls := web.calls(); len(calls) != 0 {
		t.Errorf("the app was asked to handle %v", calls)
	}
}

// Without a frontend (the default, and every other test) the 404s are JSON.
func TestNoWebKeepsJSON404(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/workspace", "/assets/index-1.js", "/favicon.ico"} {
		r := e.do("GET", p, "", nil)
		if r.Code != 404 || !isJSON(r) || r.json()["error"] != "not found" {
			t.Errorf("GET %s: %d %q %s", p, r.Code, r.Header().Get("Content-Type"), r.Body)
		}
	}
}

// Everything the backend answered before the app existed it answers the same
// way now: status, headers (bar the per-request ID) and body.
func TestRoutesAreUnchangedByTheApp(t *testing.T) {
	plain := newEnv(t)
	with := newEnv(t, withWeb(&stubWeb{}))
	_, wsPlain := plain.registerAndLoginBoth("alice", "correct horse")
	_, wsWith := with.registerAndLoginBoth("alice", "correct horse")
	wsCookie := map[*env]*http.Cookie{plain: wsPlain, with: wsWith}

	for _, tc := range []struct {
		method, path string
		hdr          map[string]string
		ws           bool
	}{
		{"GET", "/healthz", nil, false},
		{"HEAD", "/healthz", nil, false},
		{"POST", "/healthz", nil, false},
		{"GET", "/api/auth/check", nil, false},
		{"GET", "/api/auth/check", map[string]string{"X-Forwarded-Uri": "/ws/alice/terminal/"}, false},
		{"GET", "/api/auth/check", map[string]string{"X-Forwarded-Uri": "/ws/alice/terminal/"}, true},
		{"GET", "/api/auth/check", map[string]string{"X-Forwarded-Uri": "/ws/bob/terminal/"}, true},
		{"GET", "/api/auth/check", map[string]string{"X-Forwarded-Uri": "/workspace"}, true},
		{"GET", "/api/me", nil, false},
		{"GET", "/api/nope", nil, false},
		{"POST", "/api/nope", nil, false},
		{"GET", "/api/auth/login", nil, false},
	} {
		var got [2]resp
		for i, e := range []*env{plain, with} {
			var cookies []*http.Cookie
			if tc.ws {
				cookies = append(cookies, wsCookie[e])
			}
			got[i] = e.do(tc.method, tc.path, "", tc.hdr, cookies...)
		}
		if got[0].Code != got[1].Code || got[0].Body.String() != got[1].Body.String() {
			t.Errorf("%s %s %v ws=%v: %d %q without the app, %d %q with it",
				tc.method, tc.path, tc.hdr, tc.ws, got[0].Code, got[0].Body, got[1].Code, got[1].Body)
		}
		for _, k := range []string{"Content-Type", "Cache-Control", "X-Content-Type-Options", "Allow", "X-Web"} {
			if got[0].Header().Get(k) != got[1].Header().Get(k) {
				t.Errorf("%s %s: header %s differs: %q vs %q", tc.method, tc.path, k, got[0].Header().Get(k), got[1].Header().Get(k))
			}
		}
	}
}

func spaFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":               {Data: []byte("<!doctype html><title>berth</title>")},
		"assets/index-Abc12345.js": {Data: []byte("console.log(1)")},
		".gitkeep":                 {},
	}
}

// The real single-page-app handler behind the real router.
func TestRealWebAppBehindTheRouter(t *testing.T) {
	w, err := webui.New(spaFS())
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, withWeb(w))

	r := e.do("GET", "/workspace", "", nil)
	if r.Code != 200 || r.Body.String() != "<!doctype html><title>berth</title>" ||
		r.Header().Get("Cache-Control") != "no-cache" || r.Header().Get("Content-Security-Policy") == "" ||
		r.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("deep link: %d %q %v", r.Code, r.Body, r.Header())
	}
	r = e.do("GET", "/assets/index-Abc12345.js", "", nil)
	if r.Code != 200 || r.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		r.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("asset: %d %v", r.Code, r.Header())
	}
	if r := e.do("GET", "/assets/missing.js", "", nil); r.Code != 404 || strings.Contains(r.Body.String(), "berth") {
		t.Errorf("missing asset: %d %q", r.Code, r.Body)
	}
	if r := e.do("GET", "/.gitkeep", "", nil); r.Code != 404 {
		t.Errorf("placeholder: %d", r.Code)
	}

	// Backend paths never become the app, and keep the API's own headers and JSON.
	for _, p := range []string{"/api/nope", "/api", "/%61pi/me", "/ws/alice/terminal/", "/healthz/x"} {
		r := e.do("GET", p, "", nil)
		if r.Code != 404 || !isJSON(r) || strings.Contains(r.Body.String(), "berth") || r.Header().Get("Content-Security-Policy") != "" {
			t.Errorf("GET %s: %d %q %q", p, r.Code, r.Header().Get("Content-Type"), r.Body)
		}
	}
	if r := e.do("GET", "/api/nope", "", nil); r.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("unknown API path lost its no-store header: %v", r.Header())
	}
	if r := e.do("GET", "/healthz", "", nil); r.Code != 200 || r.json()["status"] != "ok" || r.Header().Get("Content-Security-Policy") != "" {
		t.Errorf("healthz: %d %s", r.Code, r.Body)
	}
	if r := e.do("GET", "/api/auth/check", "", nil); r.Code != 401 {
		t.Errorf("check: %d", r.Code)
	}
}

// The access log keeps recording only the path the client asked for, never the
// query and never a path the app rewrote internally (an unknown route is served
// as index.html).
func TestAccessLogForTheApp(t *testing.T) {
	w, err := webui.New(spaFS())
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	e := newEnv(t, withWeb(w), func(d *Deps) { d.Log = slog.New(slog.NewTextHandler(&logs, nil)) })
	e.do("GET", "/workspace?token=hunter2&x=1", "", nil)
	line := logs.String()
	if !strings.Contains(line, "path=/workspace ") || !strings.Contains(line, "status=200") || !strings.Contains(line, "method=GET") {
		t.Errorf("access log: %s", line)
	}
	for _, bad := range []string{"hunter2", "token", "index.html", "?"} {
		if strings.Contains(line, bad) {
			t.Errorf("access log contains %q: %s", bad, line)
		}
	}
}

func TestStubWebIsAHandler(t *testing.T) {
	// Guards the test double itself: a handler that is not reached must not be
	// mistaken for one that refused.
	rec := httptest.NewRecorder()
	(&stubWeb{}).ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Header().Get("X-Web") != "1" || rec.Body.String() != "WEB" {
		t.Fatalf("stub does not mark its responses: %v %q", rec.Header(), rec.Body)
	}
}
