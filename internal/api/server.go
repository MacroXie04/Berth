// Package api implements Berth's HTTP API on chi.
package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/store"
)

// Deps are the collaborators and settings a Server needs.
type Deps struct {
	Store  store.Store
	Hasher *auth.Hasher
	Log    *slog.Logger
	Now    func() time.Time // defaults to time.Now; injectable for tests
	// Workspaces runs the container lifecycle. Nil means no container backend:
	// the workspace endpoints answer 503 and everything else still works.
	Workspaces WorkspaceService
	// Web serves the single-page app for every GET/HEAD request that no route
	// claims and that is not under /api, /healthz or /ws (internal/webui.Embedded
	// in production). Nil means no frontend: those paths answer the JSON 404.
	Web http.Handler

	SessionTTL        time.Duration
	CookieSecure      bool
	DefaultMemLimitMB int
	DefaultCPULimit   float64
}

// Server serves the HTTP API.
type Server struct {
	Deps
}

// New returns a Server, filling in defaults for optional dependencies.
func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Server{Deps: d}
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(requestID)
	r.Use(s.accessLog)
	r.Use(s.recoverer)

	// The app is the fallback for a GET/HEAD that no route claimed (see notFound),
	// so it can never shadow a route: /api, /healthz and the 405s of both behave
	// as they always did.
	r.NotFound(s.notFound)
	r.MethodNotAllowed(methodNotAllowed)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api", func(r chi.Router) {
		r.Use(apiHeaders)

		r.Route("/auth", func(r chi.Router) {
			r.Post("/register", s.handleRegister)
			r.Post("/login", s.handleLogin)
			r.With(requireJSON).Post("/logout", s.handleLogout)
			r.Get("/check", s.handleCheck)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Use(requireJSON) // after the login check: a stranger still gets 401
			r.Get("/me", s.handleMe)

			r.Route("/workspace", func(r chi.Router) {
				r.Get("/", s.handleWorkspaceStatus)
				r.Post("/start", s.handleWorkspaceStart)
				r.Post("/stop", s.handleWorkspaceStop)
				r.Post("/heartbeat", s.handleWorkspaceHeartbeat)
			})
		})
	})

	return r
}

func jsonNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// notFound answers a request no route claimed: the single-page app for a
// GET/HEAD (its client-side routes, assets and index), otherwise the JSON 404.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if s.Web != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !backendPath(r.URL.Path) {
		s.Web.ServeHTTP(w, r)
		return
	}
	jsonNotFound(w, r)
}

// backendPath reports whether p, as the server decoded it, lies in a namespace
// that is never the app's: /api, /healthz and /ws (Traefik sends an owner's
// request for a workspace that is not up here, to be answered with a 404). The
// /api router inherits notFound, so this is also what keeps an unknown
// /api/... a JSON 404; it covers the spellings the router does not map to those
// paths either ("/%61pi/me", "/api%2Fme", "//api/me", "/API/me").
func backendPath(p string) bool {
	first, _, _ := strings.Cut(strings.TrimLeft(p, "/"), "/")
	switch strings.ToLower(first) {
	case "api", "healthz", "ws":
		return true
	}
	return false
}

// NewHTTPServer returns the production http.Server around h. The API proxies
// and streams nothing, so every phase gets a short deadline, and request
// headers are capped well below net/http's 1 MiB default. writeTimeout must
// outlast the slowest handler, which is a workspace start or stop.
func NewHTTPServer(addr string, h http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}
