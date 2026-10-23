// Package webui serves the built single-page app (web/, built by Vite into
// dist/) from the backend binary, so one process and one image carry both the
// API and the UI.
//
// The handler is deliberately dull. It indexes the files once at construction
// and a request can only ever select one of those names, so no spelling of a
// path (dot segments, encoded slashes, backslashes, NULs, doubled slashes) can
// reach a file that was not indexed; such paths are refused before the lookup
// as well. Dotfiles (.gitkeep, .vite/) are never indexed. Everything it sends
// carries the security headers below.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// dist is the Vite build output. dist/.gitkeep is committed so a fresh clone
// compiles before the frontend was ever built; "all:" is what lets the embed
// accept a directory whose only content is a dotfile.
//
//go:embed all:dist
var dist embed.FS

// Embedded returns the handler for the frontend compiled into the binary.
func Embedded() (http.Handler, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	return New(sub)
}

// Security headers on every response of this handler.
const (
	// A page served by Berth loads only its own scripts, styles and fonts.
	//
	// connect-src is 'self' alone: CSP3 defines 'self' to cover ws:/wss: for the
	// page's own host and port, and Chromium 141 does so (checked against the real
	// handler: a same-origin WebSocket opens over http/ws and https/wss, one to
	// another port of the same host is refused). Browsers that predate that rule
	// (older Safari and Firefox) would refuse the terminal's WebSocket; if that
	// ever shows up, set connectSrc to "'self' ws: wss:" (which also lets script
	// open sockets to any host, the one thing the strict form prevents).
	connectSrc = "'self'"

	// Scripts stay strict ('self' only: no inline script, no eval), which is what
	// blocks cross-site scripting. Styles allow 'unsafe-inline' because xterm.js
	// creates <style> elements and style attributes for its colours and selection
	// highlight; blocking them leaves an unreadable terminal (measured), and the
	// page renders no HTML from the server, so injected CSS has no way in.
	csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"font-src 'self'; connect-src " + connectSrc + "; frame-src 'self'; object-src 'none'; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

	// What the neko iframe (same origin, its own document) uses: fullscreen,
	// autoplaying video and copying out of the remote browser. A policy on the
	// parent also caps every frame below it, so these are allowed for 'self' only
	// and everything a workspace has no business with is off for everybody. The
	// microphone is off: neko's "share my microphone" button does not work through
	// this page. clipboard-read is off on purpose: with it neko copies whatever the
	// user last copied locally into the workspace whenever the mouse enters the
	// video (after a permission prompt); Ctrl+V into the remote browser still works.
	permissionsPolicy = "clipboard-read=(), clipboard-write=(self), fullscreen=(self), autoplay=(self), " +
		"camera=(), microphone=(), geolocation=(), payment=(), usb=()"
)

const (
	cacheNoCache   = "no-cache"
	cacheImmutable = "public, max-age=31536000, immutable"
)

// The first path segment of anything that belongs to the backend or to a
// workspace. The router never sends these here; this is the second lock, for a
// spelling the router did not recognise (for example "/%61pi/auth/check").
var reserved = [...]string{"api", "ws", "healthz"}

// contentTypes is explicit instead of mime.TypeByExtension, which reads the
// host's mime.types and so answers differently in the minimal runtime image.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".webmanifest": "application/manifest+json; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".wasm":        "application/wasm",
}

type fileInfo struct {
	ctype     string
	etag      string
	immutable bool
}

type handler struct {
	fsys  fs.FS
	files map[string]fileInfo // slash-separated name without leading slash
	dirs  map[string]bool
	index bool // index.html exists
}

// New returns the handler for the files in fsys, which must hold index.html at
// its root once the frontend is built. The file list is read once, here; files
// added later are not served. A missing index.html is not an error: the handler
// then answers 503 for the app shell until the next start.
func New(fsys fs.FS) (http.Handler, error) {
	h := &handler{fsys: fsys, files: map[string]fileInfo{}, dirs: map[string]bool{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if hidden(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			h.dirs[name] = true
		case d.Type().IsRegular(): // symlinks and devices are not served
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			h.files[name] = fileInfo{
				ctype: contentType(name),
				etag:  `"` + hex.EncodeToString(sum[:16]) + `"`,
				// Vite content-hashes everything it emits under assets/, so those
				// names change whenever their bytes do. Nothing else may be cached
				// without revalidation.
				immutable: strings.HasPrefix(name, "assets/"),
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index frontend files: %w", err)
	}
	_, h.index = h.files["index.html"]
	return h, nil
}

// hidden reports whether any element of the slash-separated name is a dotfile.
func hidden(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func contentType(name string) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", csp)
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Permissions-Policy", permissionsPolicy)
	hd.Set("Cache-Control", cacheNoCache) // files under assets/ override this

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hd.Set("Allow", "GET, HEAD")
		plain(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	name, trailingSlash, ok := requestName(r.URL.Path, r.URL.RawPath)
	if !ok {
		plain(w, r, http.StatusNotFound, "not found")
		return
	}

	if !trailingSlash {
		if f, ok := h.files[name]; ok {
			h.serveFile(w, r, name, f)
			return
		}
	}
	// Anything that is not a file: a directory never lists, an unknown name with
	// an extension is a missing asset (and must not be answered with HTML, which
	// would turn a stale script URL into a confusing syntax error), and every
	// other path is a client-side route of the app.
	if h.dirs[name] || path.Ext(name) != "" {
		plain(w, r, http.StatusNotFound, "not found")
		return
	}
	if !h.index {
		plain(w, r, http.StatusServiceUnavailable, "frontend not built")
		return
	}
	h.serveFile(w, r, "index.html", h.files["index.html"])
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string, f fileInfo) {
	hd := w.Header()
	hd.Set("Content-Type", f.ctype) // set, so ServeContent never sniffs
	hd.Set("ETag", f.etag)
	if f.immutable {
		hd.Set("Cache-Control", cacheImmutable)
	}
	fh, err := h.fsys.Open(name)
	if err != nil {
		hd.Del("ETag")
		hd.Set("Cache-Control", cacheNoCache)
		plain(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	defer fh.Close()
	rs, ok := fh.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(fh)
		if err != nil {
			hd.Del("ETag")
			hd.Set("Cache-Control", cacheNoCache)
			plain(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		rs = bytes.NewReader(data)
	}
	// No modification time: the ETag does the validation.
	http.ServeContent(w, r, name, time.Time{}, rs)
}

func plain(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, msg+"\n")
	}
}

// requestName turns the decoded request path into an index key ("" is the
// root). It refuses (ok=false) everything that is not a plain, canonical,
// forward-slash path to something the app owns:
//
//   - an encoded slash or backslash in the raw path (a second spelling of a
//     separator), any backslash, NUL or other control character, invalid UTF-8;
//   - empty (double slash), "." and ".." segments, and dotfile segments;
//   - the backend's and the workspaces' namespaces (/api, /ws, /healthz), whatever
//     their case.
//
// A single trailing slash is allowed and reported, because it marks a route, not
// a file.
func requestName(p, raw string) (name string, trailingSlash, ok bool) {
	if p == "" || p[0] != '/' || !utf8.ValidString(p) {
		return "", false, false
	}
	if lr := strings.ToLower(raw); strings.Contains(lr, "%2f") || strings.Contains(lr, "%5c") {
		return "", false, false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return "", false, false
		}
	}
	rest := p[1:]
	if rest == "" {
		return "", false, true
	}
	if strings.HasSuffix(rest, "/") {
		trailingSlash = true
		rest = rest[:len(rest)-1]
	}
	segs := strings.Split(rest, "/")
	for _, s := range segs {
		if s == "" || strings.HasPrefix(s, ".") { // double slash, ".", "..", dotfile
			return "", false, false
		}
	}
	first := strings.ToLower(segs[0])
	for _, r := range reserved {
		if first == r {
			return "", false, false
		}
	}
	return rest, trailingSlash, true
}
