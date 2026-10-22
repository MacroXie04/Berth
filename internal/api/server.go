// Package api implements Berth's HTTP API on chi.
package api

import (
	"log/slog"
	"net/http"
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

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api", func(r chi.Router) {
		r.Use(apiHeaders)

		r.Route("/auth", func(r chi.Router) {
			r.Post("/register", s.handleRegister)
			r.Post("/login", s.handleLogin)
			r.Post("/logout", s.handleLogout)
			r.Get("/check", s.handleCheck)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Get("/me", s.handleMe)
		})
	})
	return r
}

// NewHTTPServer returns the production http.Server around h. The API proxies
// and streams nothing, so every phase gets a short deadline, and request
// headers are capped well below net/http's 1 MiB default.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}
