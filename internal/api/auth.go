package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

// A login sets two independent cookies, each scoped to the paths that need it:
//
//	berth_session  Path=/api  the API credential (login state for /api/*)
//	berth_ws       Path=/ws   proves identity to the ForwardAuth check only
//
// Traefik forwards the browser's Cookie header to the workspace container, where
// code running as the user can read it. Scoping the tokens means the container
// only ever sees berth_ws, which cannot be used against the API, while the API
// token never leaves /api: workspace code cannot steal it.
//
// That is not the same as workspace code being unable to use it. A page served
// by a workspace (hostile code can replace ttyd or neko on their ports) shares
// the Berth origin, so script in it can call /api with the visitor's own
// cookie. Closing that needs the workspaces on a different origin (CLAUDE.md,
// decision D4c).
const (
	sessionCookieName = "berth_session"
	sessionCookiePath = "/api"
	wsCookieName      = "berth_ws"
	wsCookiePath      = "/ws"
)

// errBadLogin is the single message for every login failure, so the response
// never says whether the username or the password was wrong.
const errBadLogin = "invalid username or password"

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := auth.ValidateUsername(req.Username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := s.Hasher.Hash(req.Password)
	if err != nil {
		s.Log.Error("hash password", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	user, ws, err := s.Store.CreateUser(r.Context(), store.NewUser{
		Username:     req.Username,
		PasswordHash: hash,
		VolumeName:   workspace.VolumeName(req.Username),
		MemLimitMB:   s.DefaultMemLimitMB,
		CPULimit:     s.DefaultCPULimit,
	}, s.Now())
	switch {
	case errors.Is(err, store.ErrUsernameTaken):
		writeError(w, http.StatusConflict, "username already taken")
		return
	case err != nil:
		s.Log.Error("create user", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Log.Info("user registered", "username", user.Username, "admin", user.IsAdmin)
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":      newUserJSON(user),
		"workspace": newWorkspaceJSON(ws),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if !decodeJSON(w, r, &req) {
		return
	}

	user, err := s.Store.UserByUsername(r.Context(), req.Username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.Hasher.VerifyDummy(req.Password) // equalise timing with the real path
		writeError(w, http.StatusUnauthorized, errBadLogin)
		return
	case err != nil:
		s.Log.Error("lookup user", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !s.Hasher.Verify(user.PasswordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, errBadLogin)
		return
	}

	// Rotate: always mint a fresh token, and retire the one the browser sent.
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if err := s.Store.DeleteSession(r.Context(), auth.HashToken(c.Value)); err != nil {
			s.Log.Warn("delete previous session", "err", err)
		}
	}
	token, tokenHash, err := auth.NewSessionToken()
	if err != nil {
		s.Log.Error("new session token", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	wsToken, wsTokenHash, err := auth.NewSessionToken()
	if err != nil {
		s.Log.Error("new workspace token", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := s.Now()
	expires := now.Add(s.SessionTTL)
	if err := s.Store.CreateSession(r.Context(), store.Session{
		TokenHash:   tokenHash,
		WSTokenHash: wsTokenHash,
		UserID:      user.ID,
		CreatedAt:   now,
		ExpiresAt:   expires,
	}); err != nil {
		s.Log.Error("create session", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := s.Store.DeleteExpiredSessions(r.Context(), now); err != nil {
		s.Log.Warn("purge expired sessions", "err", err) // housekeeping only
	}

	s.setSessionCookies(w, token, wsToken, expires)
	writeJSON(w, http.StatusOK, map[string]any{"user": newUserJSON(user)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		if err := s.Store.DeleteSession(r.Context(), auth.HashToken(c.Value)); err != nil {
			s.Log.Error("delete session", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	s.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	ws, err := s.Store.WorkspaceByUserID(r.Context(), user.ID)
	if err != nil {
		// Every user has a workspace row from registration; absence is a bug.
		s.Log.Error("load workspace", "user", user.Username, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":      newUserJSON(user),
		"workspace": newWorkspaceJSON(ws),
	})
}

// sessionCookie builds one of the two session cookies: HttpOnly so scripts
// cannot read it, SameSite=Lax so other sites cannot make the browser send it on
// cross-site POSTs, Secure when the deployment terminates TLS.
func (s *Server) sessionCookie(name, path, value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Server) setSessionCookies(w http.ResponseWriter, apiToken, wsToken string, expires time.Time) {
	maxAge := int(expires.Sub(s.Now()).Seconds())
	http.SetCookie(w, s.sessionCookie(sessionCookieName, sessionCookiePath, apiToken, expires, maxAge))
	http.SetCookie(w, s.sessionCookie(wsCookieName, wsCookiePath, wsToken, expires, maxAge))
}

// clearSessionCookies expires both cookies. A clearing cookie only replaces the
// stored one if Name and Path match, so the paths must mirror setSessionCookies.
func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	http.SetCookie(w, s.sessionCookie(sessionCookieName, sessionCookiePath, "", time.Time{}, -1))
	http.SetCookie(w, s.sessionCookie(wsCookieName, wsCookiePath, "", time.Time{}, -1))
}
