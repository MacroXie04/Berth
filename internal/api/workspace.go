package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
)

// WorkspaceService is what the HTTP layer needs from the workspace manager.
type WorkspaceService interface {
	Start(ctx context.Context, user store.User) (workspace.Status, error)
	Stop(ctx context.Context, user store.User) (workspace.Status, error)
	Status(ctx context.Context, user store.User) (workspace.Status, error)
	Heartbeat(ctx context.Context, user store.User) error
}

var _ WorkspaceService = (*workspace.Manager)(nil)

type usageJSON struct {
	CPUPercent    float64 `json:"cpu_percent"` // 100 = one full core
	MemBytes      uint64  `json:"mem_bytes"`
	MemLimitBytes uint64  `json:"mem_limit_bytes"`
	Pids          uint64  `json:"pids"`
}

type statusJSON struct {
	State         store.State `json:"state"`
	Health        string      `json:"health"` // none | starting | healthy | unhealthy
	MemLimitMB    int         `json:"mem_limit_mb"`
	CPULimit      float64     `json:"cpu_limit"`
	LastActiveAt  *time.Time  `json:"last_active_at"`
	LastStoppedAt *time.Time  `json:"last_stopped_at"`
	// Usage is present only while the container is running and a sample succeeded.
	Usage *usageJSON `json:"usage"`
	// NekoPassword is what the browser view logs in with. It goes only to the
	// authenticated owner, and only while the container exists.
	NekoPassword string `json:"neko_password,omitempty"`
}

// workspaceResponse is the body of every workspace endpoint that reports a
// status. It names the account the answer is about: the session cookie is shared
// by every tab of a browser, so a page that remembers who it believes is logged
// in can compare that with who the server just answered for, and notice a login
// or logout in another tab.
type workspaceResponse struct {
	Username  string     `json:"username"`
	Workspace statusJSON `json:"workspace"`
}

func newWorkspaceResponse(user store.User, s workspace.Status) workspaceResponse {
	return workspaceResponse{Username: user.Username, Workspace: newStatusJSON(s)}
}

func newStatusJSON(s workspace.Status) statusJSON {
	ws := s.Workspace
	out := statusJSON{
		State:         ws.State,
		Health:        "none",
		MemLimitMB:    ws.MemLimitMB,
		CPULimit:      ws.CPULimit,
		LastActiveAt:  ws.LastActiveAt,
		LastStoppedAt: ws.LastStoppedAt,
	}
	if s.Health != docker.HealthNone {
		out.Health = string(s.Health)
	}
	if s.Stats != nil {
		out.Usage = &usageJSON{
			CPUPercent:    s.Stats.CPUPercent,
			MemBytes:      s.Stats.MemUsageBytes,
			MemLimitBytes: s.Stats.MemLimitBytes,
			Pids:          s.Stats.Pids,
		}
	}
	if ws.State == store.StateStarting || ws.State == store.StateRunning {
		out.NekoPassword = ws.NekoPassword
	}
	return out
}

// workspaceError maps a manager error to a response. Details go to the log, not
// to the client: they name containers, networks and paths of the host.
func (s *Server) workspaceError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, workspace.ErrNoCapacity):
		writeError(w, http.StatusServiceUnavailable, "the server has no capacity for another workspace")
	case errors.Is(err, workspace.ErrBusy):
		writeError(w, http.StatusConflict, "the workspace is busy starting or stopping; try again in a moment")
	case docker.IsUnavailable(err):
		s.Log.Error(what, "err", err)
		writeError(w, http.StatusServiceUnavailable, "the container service is unavailable")
	default:
		s.Log.Error(what, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// requireWorkspaces answers 503 when the server runs without a container
// backend (for example the API started without access to Docker).
func (s *Server) requireWorkspaces(w http.ResponseWriter) bool {
	if s.Workspaces == nil {
		writeError(w, http.StatusServiceUnavailable, "the container service is not configured")
		return false
	}
	return true
}

func (s *Server) handleWorkspaceStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w) {
		return
	}
	user := userFrom(r.Context())
	st, err := s.Workspaces.Status(r.Context(), user)
	if err != nil {
		s.workspaceError(w, "workspace status", err)
		return
	}
	writeJSON(w, http.StatusOK, newWorkspaceResponse(user, st))
}

// handleWorkspaceStart powers the workspace on and returns as soon as the
// container is started; the client polls GET /api/workspace until it is running.
func (s *Server) handleWorkspaceStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w) {
		return
	}
	user := userFrom(r.Context())
	st, err := s.Workspaces.Start(r.Context(), user)
	if err != nil {
		s.workspaceError(w, "workspace start", err)
		return
	}
	s.Log.Info("workspace start", "user", user.Username, "state", st.Workspace.State)
	writeJSON(w, http.StatusOK, newWorkspaceResponse(user, st))
}

func (s *Server) handleWorkspaceStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w) {
		return
	}
	user := userFrom(r.Context())
	st, err := s.Workspaces.Stop(r.Context(), user)
	if err != nil {
		s.workspaceError(w, "workspace stop", err)
		return
	}
	s.Log.Info("workspace stop", "user", user.Username, "state", st.Workspace.State)
	writeJSON(w, http.StatusOK, newWorkspaceResponse(user, st))
}

func (s *Server) handleWorkspaceHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w) {
		return
	}
	if err := s.Workspaces.Heartbeat(r.Context(), userFrom(r.Context())); err != nil {
		s.Log.Error("workspace heartbeat", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
