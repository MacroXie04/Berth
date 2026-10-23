package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/store"
)

type ctxKey struct{ name string }

var userCtxKey = ctxKey{"user"}

// userFrom returns the authenticated user placed in ctx by requireUser.
func userFrom(ctx context.Context) store.User {
	u, _ := ctx.Value(userCtxKey).(store.User)
	return u
}

// requestID gives every request a fresh random ID, exposed as the X-Request-Id
// response header and to the access log. An inbound X-Request-Id is ignored:
// chi's stock middleware would trust it and log it verbatim, letting any client
// write megabytes into the log per request.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [6]byte
		_, _ = rand.Read(b[:]) // crypto/rand does not fail on supported platforms
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), middleware.RequestIDKey, id)))
	})
}

// apiHeaders marks every API response as uncacheable and un-sniffable: it may
// carry per-user state or an auth decision.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// accessLog logs one line per request. Only the path is logged, never the
// query string, headers or cookies. High-volume plumbing endpoints (health
// probes and the per-request ForwardAuth check) log at debug.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/api/auth/check" {
			level = slog.LevelDebug
		}
		s.Log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration", time.Since(start).Round(time.Microsecond),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}

// recoverer turns a handler panic into a logged 500 instead of a dropped
// connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec) // net/http's own signal to abort the response
			}
			s.Log.Error("panic in handler", "panic", rec, "path", r.URL.Path,
				"request_id", middleware.GetReqID(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

// errNoSession means the request carries no valid, unexpired session. It is
// distinct from an infrastructure error so a database hiccup is a 500, not a
// spurious logout.
var errNoSession = errors.New("no valid session")

// sessionUser resolves the request's API session cookie to a user.
func (s *Server) sessionUser(r *http.Request) (store.User, error) {
	return s.cookieUser(r, sessionCookieName, s.Store.SessionUser)
}

// workspaceUser resolves the workspace-scoped cookie, which only the ForwardAuth
// check accepts.
func (s *Server) workspaceUser(r *http.Request) (store.User, error) {
	return s.cookieUser(r, wsCookieName, s.Store.WorkspaceSessionUser)
}

func (s *Server) cookieUser(r *http.Request, cookie string,
	lookup func(context.Context, string, time.Time) (store.User, error)) (store.User, error) {
	c, err := r.Cookie(cookie)
	if err != nil || c.Value == "" {
		return store.User{}, errNoSession
	}
	u, err := lookup(r.Context(), auth.HashToken(c.Value), s.Now())
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, errNoSession
	}
	return u, err
}

// requireUser rejects requests without a valid session and stores the user in
// the request context.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.sessionUser(r)
		switch {
		case errors.Is(err, errNoSession):
			writeError(w, http.StatusUnauthorized, "not logged in")
		case err != nil:
			s.Log.Error("session lookup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		default:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
		}
	})
}
