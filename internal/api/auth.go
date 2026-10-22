package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

const sessionCookieName = "berth_session"

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
	now := s.Now()
	expires := now.Add(s.SessionTTL)
	if err := s.Store.CreateSession(r.Context(), store.Session{
		TokenHash: tokenHash,
		UserID:    user.ID,
		CreatedAt: now,
		ExpiresAt: expires,
	}); err != nil {
		s.Log.Error("create session", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := s.Store.DeleteExpiredSessions(r.Context(), now); err != nil {
		s.Log.Warn("purge expired sessions", "err", err) // housekeeping only
	}

	s.setSessionCookie(w, token, expires)
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
	s.clearSessionCookie(w)
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

// setSessionCookie issues the session cookie: HttpOnly so scripts cannot read
// it, SameSite=Lax so other sites cannot make the browser send it on
// cross-site POSTs, Secure when the deployment terminates TLS.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(expires.Sub(s.Now()).Seconds()),
		HttpOnly: true,
		Secure:   s.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}
