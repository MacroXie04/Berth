package webui

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	indexHTML = "<!doctype html><title>berth</title><div id=root></div>"
	secret    = "TOP-SECRET-OUTSIDE-THE-SPA"
)

// fixture is a built frontend as Vite lays it out, plus files that must never
// be served: dotfiles and a manifest in a dot directory.
func fixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                   {Data: []byte(indexHTML)},
		"favicon.ico":                  {Data: []byte("ico")},
		"favicon.svg":                  {Data: []byte("<svg/>")},
		"robots.txt":                   {Data: []byte("User-agent: *\n")},
		"site.webmanifest":             {Data: []byte("{}")},
		"LICENSE":                      {Data: []byte("no extension")},
		"notes.xyz":                    {Data: []byte("<script>alert(1)</script>")},
		"sniff.txt":                    {Data: []byte("<html><script>alert(1)</script></html>")},
		"assets/index-Abc12345.js":     {Data: []byte("console.log(1)")},
		"assets/index-Abc12345.css":    {Data: []byte("body{}")},
		"assets/index-Abc12345.js.map": {Data: []byte("{}")},
		"assets/logo-Zz99.svg":         {Data: []byte("<svg/>")},
		"assets/logo-Zz99.png":         {Data: []byte("\x89PNG")},
		"assets/inter-Qq77.woff2":      {Data: []byte("wOF2")},
		"assets/data-Qq77.json":        {Data: []byte("{}")},
		"assets/.hidden.js":            {Data: []byte("hidden")},
		".gitkeep":                     {Data: nil},
		".vite/manifest.json":          {Data: []byte("{}")},
	}
}

func newHandler(t *testing.T, fsys fstest.MapFS) http.Handler {
	t.Helper()
	h, err := New(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// wantHeaders are on every response the handler produces, whatever its status.
var wantHeaders = map[string]string{
	"X-Frame-Options":        "DENY",
	"Referrer-Policy":        "no-referrer",
	"X-Content-Type-Options": "nosniff",
}

func checkSecurityHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for k, v := range wantHeaders {
		if got := h.Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", what, k, got, v)
		}
	}
	csp := parsePolicy(t, h.Get("Content-Security-Policy"), ";", " ")
	for dir, want := range map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self'",
		"style-src":       "'self' 'unsafe-inline'",
		"img-src":         "'self' data:",
		"font-src":        "'self'",
		"connect-src":     "'self'",
		"frame-src":       "'self'",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"form-action":     "'self'",
		"frame-ancestors": "'none'",
	} {
		if got := csp[dir]; got != want {
			t.Errorf("%s: CSP %s = %q, want %q (header %q)", what, dir, got, want, h.Get("Content-Security-Policy"))
		}
	}
	if len(csp) != 11 {
		t.Errorf("%s: unexpected CSP directives: %q", what, h.Get("Content-Security-Policy"))
	}
	pp := parsePolicy(t, h.Get("Permissions-Policy"), ",", "=")
	for feat, want := range map[string]string{
		"clipboard-read": "()", "clipboard-write": "(self)", "fullscreen": "(self)", "autoplay": "(self)",
		"camera": "()", "microphone": "()", "geolocation": "()", "payment": "()", "usb": "()",
	} {
		if got := pp[feat]; got != want {
			t.Errorf("%s: Permissions-Policy %s = %q, want %q", what, feat, got, want)
		}
	}
}

// parsePolicy splits "a b; c d" (sep ";", kv " ") or "a=b, c=d" (sep ",", kv "=").
func parsePolicy(t *testing.T, v, sep, kv string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, part := range strings.Split(v, sep) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, _ := strings.Cut(part, kv)
		if _, dup := m[k]; dup {
			t.Errorf("policy %q repeats %q", v, k)
		}
		m[k] = strings.TrimSpace(val)
	}
	return m
}

func TestServesFilesWithTypesAndCaching(t *testing.T) {
	h := newHandler(t, fixture())
	cases := []struct {
		path, ctype, cache string
	}{
		{"/index.html", "text/html; charset=utf-8", "no-cache"},
		{"/assets/index-Abc12345.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/assets/index-Abc12345.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/assets/index-Abc12345.js.map", "application/json; charset=utf-8", "public, max-age=31536000, immutable"},
		{"/assets/logo-Zz99.svg", "image/svg+xml", "public, max-age=31536000, immutable"},
		{"/assets/logo-Zz99.png", "image/png", "public, max-age=31536000, immutable"},
		{"/assets/inter-Qq77.woff2", "font/woff2", "public, max-age=31536000, immutable"},
		{"/assets/data-Qq77.json", "application/json; charset=utf-8", "public, max-age=31536000, immutable"},
		// Not content-hashed, so they must revalidate.
		{"/favicon.ico", "image/x-icon", "no-cache"},
		{"/favicon.svg", "image/svg+xml", "no-cache"},
		{"/robots.txt", "text/plain; charset=utf-8", "no-cache"},
		{"/site.webmanifest", "application/manifest+json; charset=utf-8", "no-cache"},
		// Unknown types are opaque, never guessed from the content.
		{"/notes.xyz", "application/octet-stream", "no-cache"},
		{"/LICENSE", "application/octet-stream", "no-cache"},
		{"/sniff.txt", "text/plain; charset=utf-8", "no-cache"},
	}
	for _, tc := range cases {
		rec := do(h, "GET", tc.path)
		if rec.Code != 200 {
			t.Errorf("GET %s: %d", tc.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != tc.ctype {
			t.Errorf("GET %s: Content-Type %q, want %q", tc.path, got, tc.ctype)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("GET %s: Cache-Control %q, want %q", tc.path, got, tc.cache)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("GET %s: no ETag", tc.path)
		}
		checkSecurityHeaders(t, "GET "+tc.path, rec.Header())
	}
	if body := do(h, "GET", "/assets/index-Abc12345.js").Body.String(); body != "console.log(1)" {
		t.Errorf("asset body %q", body)
	}
}

func TestSPAFallback(t *testing.T) {
	h := newHandler(t, fixture())
	for _, p := range []string{
		"/", "/workspace", "/workspace/", "/admin/users", "/admin/users/", "/login", "/some/deep/link/here",
		"/wsx", "/apix", "/healthzz", // look like the reserved names but are different segments
		"/v1.2/route", // a dot in a directory name is not a file extension
		"/a%20b",
		"/index.html?x=1", "/workspace?token=abc#frag",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			rec := do(h, method, p)
			if rec.Code != 200 {
				t.Errorf("%s %s: %d", method, p, rec.Code)
				continue
			}
			if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Header().Get("Cache-Control") != "no-cache" {
				t.Errorf("%s %s: headers %v", method, p, rec.Header())
			}
			if method == "GET" && rec.Body.String() != indexHTML {
				t.Errorf("GET %s: body %q", p, rec.Body.String())
			}
			if method == "HEAD" && rec.Body.Len() != 0 {
				t.Errorf("HEAD %s: body %q", p, rec.Body.String())
			}
			if rec.Header().Get("Content-Length") != strconv.Itoa(len(indexHTML)) {
				t.Errorf("%s %s: Content-Length %q", method, p, rec.Header().Get("Content-Length"))
			}
			checkSecurityHeaders(t, method+" "+p, rec.Header())
		}
	}
}

// A missing file with an extension is a missing file, never the app shell: an
// HTML answer to a script request turns a stale deploy into a syntax error.
func TestMissingFilesAndDirectoriesAre404(t *testing.T) {
	h := newHandler(t, fixture())
	for _, p := range []string{
		"/assets/missing.js", "/assets/index-Abc12345.js.bak", "/favicon.png", "/missing.css", "/x/y/z.png",
		"/assets", "/assets/", // a directory never lists, and is not a route either
		"/index.html/", "/assets/index-Abc12345.js/", // a trailing slash makes a file URL a different thing
		"/assets/INDEX-ABC12345.JS", // paths are case sensitive
	} {
		for _, method := range []string{"GET", "HEAD"} {
			rec := do(h, method, p)
			if rec.Code != 404 {
				t.Errorf("%s %s: %d, want 404 (body %q)", method, p, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "<") || strings.Contains(rec.Body.String(), "index-Abc12345") {
				t.Errorf("%s %s: body leaks content: %q", method, p, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-cache" {
				t.Errorf("%s %s: a 404 must not be cached: %q", method, p, rec.Header().Get("Cache-Control"))
			}
			checkSecurityHeaders(t, method+" "+p, rec.Header())
		}
	}
}

// The backend's and the workspaces' namespaces are never the app, whatever the
// spelling; the router normally keeps them out, this is the second lock.
func TestReservedNamespacesAre404NotIndex(t *testing.T) {
	h := newHandler(t, fixture())
	for _, p := range []string{
		"/api", "/api/", "/api/me", "/api/auth/check", "/api/nope.js", "/API/me", "/Api", "/aPi/auth/check",
		"/ws", "/ws/", "/ws/alice/terminal/", "/WS/alice/browser/", "/ws/alice/browser/index.html",
		"/healthz", "/healthz/", "/HEALTHZ", "/healthz/x",
		"/%61pi/auth/check", "/%61PI/me", "/%77s/alice/terminal/", "/%68ealthz",
		"/api%2Fauth%2Fcheck", "/api%2fme", "/ws%2Falice%2Fterminal",
	} {
		rec := do(h, "GET", p)
		if rec.Code != 404 || strings.Contains(rec.Body.String(), "berth") {
			t.Errorf("GET %s: %d %q, want a plain 404", p, rec.Code, rec.Body.String())
		}
	}
}

func TestMethods(t *testing.T) {
	h := newHandler(t, fixture())
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "PROPFIND"} {
		for _, p := range []string{"/", "/workspace", "/assets/index-Abc12345.js", "/missing.js", "/api/me", "/ws/alice/terminal/"} {
			rec := do(h, method, p)
			if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: %d Allow=%q", method, p, rec.Code, rec.Header().Get("Allow"))
			}
			if strings.Contains(rec.Body.String(), "berth") || strings.Contains(rec.Body.String(), "console") {
				t.Errorf("%s %s: body %q", method, p, rec.Body.String())
			}
			checkSecurityHeaders(t, method+" "+p, rec.Header())
		}
	}
}

func TestConditionalRequests(t *testing.T) {
	h := newHandler(t, fixture())
	for _, p := range []string{"/", "/assets/index-Abc12345.js"} {
		first := do(h, "GET", p)
		etag := first.Header().Get("ETag")
		if etag == "" || !strings.HasPrefix(etag, `"`) {
			t.Fatalf("GET %s: ETag %q", p, etag)
		}
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("If-None-Match", etag)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 304 || rec.Body.Len() != 0 {
			t.Errorf("GET %s with its ETag: %d %q", p, rec.Code, rec.Body.String())
		}
		checkSecurityHeaders(t, "304 "+p, rec.Header())
		if rec.Header().Get("Cache-Control") != first.Header().Get("Cache-Control") {
			t.Errorf("304 for %s lost Cache-Control: %q", p, rec.Header().Get("Cache-Control"))
		}
		req.Header.Set("If-None-Match", `"other"`)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("GET %s with a stale ETag: %d", p, rec.Code)
		}
	}
	// Different content, different validator.
	a := do(h, "GET", "/assets/index-Abc12345.js").Header().Get("ETag")
	b := do(h, "GET", "/assets/index-Abc12345.css").Header().Get("ETag")
	if a == b {
		t.Errorf("distinct files share an ETag: %s", a)
	}
}

// A 206 would be compressed by Traefik's compress middleware (br, zstd) while its
// Content-Range counts identity bytes, which corrupts a resumed download. The
// handler never answers one: every request for a file is a 200 or a 304.
func TestRangeRequestsAreAnsweredWithTheWholeFile(t *testing.T) {
	h := newHandler(t, fixture())
	files := map[string]string{
		"/assets/index-Abc12345.js":  "console.log(1)",
		"/assets/index-Abc12345.css": "body{}",
		"/index.html":                indexHTML,
		"/":                          indexHTML, // the app shell
		"/workspace/deep/link":       indexHTML, // a client-side route, same file
		"/favicon.ico":               "ico",
	}
	ranges := []string{"bytes=0-3", "bytes=1-", "bytes=-4", "bytes=0-0", "bytes=0-1,3-4", "bytes=999999-", "bytes=0-99999", "items=0-3", "bytes=garbage"}
	for p, body := range files {
		etag := do(h, "GET", p).Header().Get("ETag")
		for _, rg := range ranges {
			for _, ifRange := range []string{"", etag, `"stale"`} {
				req := httptest.NewRequest("GET", p, nil)
				req.Header.Set("Range", rg)
				if ifRange != "" {
					req.Header.Set("If-Range", ifRange)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				what := "GET " + p + " Range: " + rg + " If-Range: " + ifRange
				if rec.Code != 200 {
					t.Errorf("%s: %d, want 200", what, rec.Code)
				}
				if rec.Body.String() != body {
					t.Errorf("%s: body %q, want the whole file %q", what, rec.Body.String(), body)
				}
				if cr := rec.Header().Get("Content-Range"); cr != "" {
					t.Errorf("%s: Content-Range %q", what, cr)
				}
				if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
					t.Errorf("%s: Content-Type %q", what, ct)
				}
				if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
					t.Errorf("%s: Content-Length %q, want %d", what, cl, len(body))
				}
				if ar := rec.Header().Get("Accept-Ranges"); ar != "none" {
					t.Errorf("%s: Accept-Ranges %q, want none (ranges are not served)", what, ar)
				}
				if rec.Header().Get("ETag") != etag {
					t.Errorf("%s: ETag %q, want %q", what, rec.Header().Get("ETag"), etag)
				}
				checkSecurityHeaders(t, what, rec.Header())
				// The handler works on a copy: the caller's request is untouched.
				if req.Header.Get("Range") != rg || req.Header.Get("If-Range") != ifRange {
					t.Errorf("%s: the caller's request headers were modified", what)
				}
			}
		}
	}

	// Without a Range header too: no ranges are advertised.
	if ar := do(h, "GET", "/assets/index-Abc12345.js").Header().Get("Accept-Ranges"); ar != "none" {
		t.Errorf("Accept-Ranges %q without a Range request, want none", ar)
	}

	// HEAD keeps working: the whole file's headers and no body.
	for _, rg := range []string{"", "bytes=0-3"} {
		req := httptest.NewRequest("HEAD", "/assets/index-Abc12345.js", nil)
		if rg != "" {
			req.Header.Set("Range", rg)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Range") != "" ||
			rec.Header().Get("Content-Length") != strconv.Itoa(len("console.log(1)")) {
			t.Errorf("HEAD with Range %q: %d body=%q Content-Range=%q Content-Length=%q",
				rg, rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"), rec.Header().Get("Content-Length"))
		}
	}

	// Validators still win: a matching If-None-Match is a 304 even with a Range.
	for _, p := range []string{"/", "/assets/index-Abc12345.js"} {
		etag := do(h, "GET", p).Header().Get("ETag")
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("If-None-Match", etag)
		req.Header.Set("Range", "bytes=0-3")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 304 || rec.Body.Len() != 0 || rec.Header().Get("Content-Range") != "" {
			t.Errorf("GET %s with If-None-Match and Range: %d %q", p, rec.Code, rec.Body.String())
		}
		checkSecurityHeaders(t, "304+Range "+p, rec.Header())
	}

	// Over a real connection, the way Traefik talks to the backend.
	srv := httptest.NewServer(h)
	defer srv.Close()
	creq, _ := http.NewRequest("GET", srv.URL+"/assets/index-Abc12345.js", nil)
	creq.Header.Set("Range", "bytes=5-")
	creq.Header.Set("Accept-Encoding", "br")
	resp, err := http.DefaultClient.Do(creq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(got) != "console.log(1)" || resp.Header.Get("Content-Range") != "" {
		t.Errorf("ranged request over HTTP: %d %q Content-Range=%q", resp.StatusCode, got, resp.Header.Get("Content-Range"))
	}
}

// Without a build the API still runs; the app shell answers 503 and says why.
func TestFrontendNotBuilt(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"only the placeholder": {".gitkeep": {}},
		"empty":                {},
		"assets but no index":  {"assets/a-1.js": {Data: []byte("x")}},
	} {
		h := newHandler(t, fsys)
		for _, p := range []string{"/", "/workspace", "/admin/users", "/index.html"} {
			for _, method := range []string{"GET", "HEAD"} {
				rec := do(h, method, p)
				want := 503
				if p == "/index.html" {
					want = 404 // a named file that does not exist
				}
				if rec.Code != want {
					t.Errorf("%s: %s %s: %d, want %d", name, method, p, rec.Code, want)
				}
				if want == 503 && method == "GET" && rec.Body.String() != "frontend not built\n" {
					t.Errorf("%s: GET %s: body %q", name, p, rec.Body.String())
				}
				if method == "HEAD" && rec.Body.Len() != 0 {
					t.Errorf("%s: HEAD %s: body %q", name, p, rec.Body.String())
				}
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
					t.Errorf("%s: %s %s: Content-Type %q", name, method, p, ct)
				}
				checkSecurityHeaders(t, name+" "+method+" "+p, rec.Header())
			}
		}
		if rec := do(h, "GET", "/assets/missing.js"); rec.Code != 404 {
			t.Errorf("%s: missing asset: %d", name, rec.Code)
		}
	}
	h := newHandler(t, fstest.MapFS{"assets/a-1.js": {Data: []byte("x")}})
	if rec := do(h, "GET", "/assets/a-1.js"); rec.Code != 200 {
		t.Errorf("an existing asset is still served without an index: %d", rec.Code)
	}
}

// The index is a list of names taken at construction. Dotfiles never make it
// in, so a Vite manifest or the placeholder can not be requested by any path.
func TestIndexSkipsDotfilesAndNonRegularFiles(t *testing.T) {
	h := newHandler(t, fixture()).(*handler)
	for name := range h.files {
		if hidden(name) {
			t.Errorf("dotfile %q is indexed", name)
		}
	}
	for name := range h.dirs {
		if hidden(name) {
			t.Errorf("dot directory %q is indexed", name)
		}
	}
	if !h.dirs["assets"] || !h.index {
		t.Errorf("dirs=%v index=%v", h.dirs, h.index)
	}
	for _, p := range []string{"/.gitkeep", "/.vite/manifest.json", "/.vite", "/.vite/", "/assets/.hidden.js", "/.well-known/security", "/a/.b/c"} {
		if rec := do(h, "GET", p); rec.Code != 404 {
			t.Errorf("GET %s: %d, want 404 (dotfiles are not part of the app)", p, rec.Code)
		}
	}
}

// outsideFixture is a real directory tree: dist/ is the app, secret.txt lies
// next to it, and dist/link.txt is a symlink to it.
func outsideFixture(t *testing.T) http.Handler {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"secret.txt":                    secret,
		"dist/index.html":               indexHTML,
		"dist/assets/index-Abc12345.js": "console.log(1)",
		"dist/.env":                     secret,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(dist, "link.txt")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.Symlink(root, filepath.Join(dist, "up")); err != nil {
		t.Fatal(err)
	}
	h, err := New(os.DirFS(dist))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// rawGet sends a request line verbatim, so no client library gets to clean or
// re-encode the target. It returns the status and the body.
func rawGet(t *testing.T, addr, method, target string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, method+" "+target+" HTTP/1.1\r\nHost: berth\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, "(no response: " + err.Error() + ")"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Every spelling of "a path above the app" or "a path that is not canonical",
// sent as raw request lines over a real socket, next to a real secret file.
func TestPathTraversalIsBoring(t *testing.T) {
	srv := httptest.NewServer(outsideFixture(t))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	// Each must be refused, never answered with the secret and never with the
	// app shell (that would make a bad path look like a route).
	refused := []string{
		"/../secret.txt", "/../../secret.txt", "/assets/../../secret.txt", "/assets/../../../../etc/passwd",
		"/..%2fsecret.txt", "/..%2Fsecret.txt", "/assets/..%2f..%2fsecret.txt", "/assets%2f..%2f..%2fsecret.txt",
		"/%2e%2e/secret.txt", "/%2E%2E/secret.txt", "/.%2e/secret.txt", "/%2e./secret.txt", "/assets/%2e%2e/%2e%2e/secret.txt",
		"/%2e%2e%2fsecret.txt", "/%252e%252e/secret.txt",
		"/..%5csecret.txt", "/%5c..%5csecret.txt", "/assets%5c..%5c..%5csecret.txt", "/..\\secret.txt", "/assets\\index-Abc12345.js",
		"//secret.txt", "///etc/passwd", "//assets/index-Abc12345.js", "/assets//index-Abc12345.js", "/assets///index-Abc12345.js",
		"/./index.html", "/assets/./index-Abc12345.js", "/assets/index-Abc12345.js/.",
		"/%00", "/index.html%00", "/assets/index-Abc12345.js%00.png", "/secret.txt%00.js", "/assets/%00/../../secret.txt",
		"/%ff", "/assets/%c0%ae%c0%ae/secret.txt", "/%c0%af", "/index%0d%0aSet-Cookie:x=1", "/index.html%0a",
		"/secret.txt", "/dist/../secret.txt", "/link.txt", "/up/secret.txt", "/up/dist/index.html", "/.env", "/up/../secret.txt",
		// no-extension variants must not turn into the app shell either
		"/../workspace", "/assets/../workspace", "/%2e%2e/workspace", "/workspace/../admin", "/./workspace", "/a//b",
		"/workspace%2fadmin", "/workspace%5cadmin", "/workspace\\admin", "/%2e", "/%2e%2e", "/..", "/.", "/assets/..",
		// the namespaces that are never the app
		"/api", "/api/auth/check", "/api/..", "/ws/../api/auth/check", "/ws/alice/terminal/", "/%2e%2e/api/auth/check", "//api/auth/check",
		"/healthz", "/healthz/",
	}
	for _, target := range refused {
		for _, method := range []string{"GET", "HEAD"} {
			code, body := rawGet(t, addr, method, target)
			if code == 0 { // the HTTP server itself refused to parse it; also fine
				continue
			}
			if code != 404 && code != 400 {
				t.Errorf("%s %s: %d, want 404 (or 400 from net/http)", method, target, code)
			}
			if strings.Contains(body, secret) || strings.Contains(body, "berth") || strings.Contains(body, "console.log") || strings.Contains(body, "root:") {
				t.Errorf("%s %s: body %q", method, target, body)
			}
		}
	}

	// The same targets through the request URL as a handler sees it (httptest
	// parses what net/http would), and straight at the handler.
	h := outsideFixture(t)
	for _, target := range refused {
		req, err := http.NewRequest("GET", "http://berth"+target, nil)
		if err != nil {
			continue // not even a valid URL
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("handler GET %q (Path=%q RawPath=%q): %d", target, req.URL.Path, req.URL.RawPath, rec.Code)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("handler GET %q leaked the secret", target)
		}
	}

	// And the sane neighbours still work.
	for target, want := range map[string]int{
		"/": 200, "/workspace": 200, "/assets/index-Abc12345.js": 200, "/index.html": 200, "/assets/%69ndex-Abc12345.js": 200,
	} {
		if code, _ := rawGet(t, addr, "GET", target); code != want {
			t.Errorf("GET %s: %d, want %d", target, code, want)
		}
	}
}

func TestEmbeddedPlaceholderBuilds(t *testing.T) {
	h, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	// Whether or not the frontend was built before this test ran, the
	// placeholder is never served and the handler does not fail.
	if rec := do(h, "GET", "/.gitkeep"); rec.Code != 404 {
		t.Errorf("/.gitkeep: %d", rec.Code)
	}
	if rec := do(h, "GET", "/"); rec.Code != 200 && rec.Code != 503 {
		t.Errorf("/: %d", rec.Code)
	}
	if rec := do(h, "GET", "/assets/definitely-missing.js"); rec.Code != 404 {
		t.Errorf("missing asset: %d", rec.Code)
	}
}

func TestRequestName(t *testing.T) {
	type res struct {
		name     string
		trailing bool
		ok       bool
	}
	cases := map[string]res{
		"/":            {"", false, true},
		"/workspace":   {"workspace", false, true},
		"/workspace/":  {"workspace", true, true},
		"/assets/a.js": {"assets/a.js", false, true},
		"/a b":         {"a b", false, true},
		"/wsx":         {"wsx", false, true},
		"":             {},
		"workspace":    {},
		"//":           {},
		"//a":          {},
		"/a//b":        {},
		"/a/./b":       {},
		"/a/../b":      {},
		"/.":           {},
		"/..":          {},
		"/.git":        {},
		"/a/.b":        {},
		"/a\\b":        {},
		"/a\x00b":      {},
		"/a\nb":        {},
		"/a\x7fb":      {},
		"/\xff":        {},
		"/api":         {},
		"/api/x":       {},
		"/Api/x":       {},
		"/ws":          {},
		"/WS/x":        {},
		"/healthz":     {},
		"/HealthZ/x":   {},
	}
	for p, want := range cases {
		name, trailing, ok := requestName(p, "")
		if (res{name, trailing, ok}) != want {
			t.Errorf("requestName(%q) = %q, %v, %v; want %+v", p, name, trailing, ok, want)
		}
	}
	for _, raw := range []string{"/a%2Fb", "/a%2fb", "/a%5Cb", "/a%5cb"} {
		if _, _, ok := requestName("/a/b", raw); ok {
			t.Errorf("raw path %q accepted", raw)
		}
	}
	if _, _, ok := requestName("/a b", "/a%20b"); !ok {
		t.Error("an encoded space is a plain character")
	}
}
