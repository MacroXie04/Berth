package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/MacroXie04/Berth/internal/store"
)

// maxBodyBytes caps JSON request bodies; the largest legitimate one is a
// username and a 72-byte password.
const maxBodyBytes = 4 << 10

// writeJSON marshals before writing anything, so an encoding failure becomes an
// honest 500 instead of a success status followed by an empty body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON parses a strict JSON request body into dst. On failure it has
// already written the error response and returns false. Requiring
// Content-Type: application/json also means a cross-site HTML form cannot
// submit to these endpoints.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// userJSON is the public view of a user; it never includes the password hash.
type userJSON struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

func newUserJSON(u store.User) userJSON {
	return userJSON{ID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin}
}

// workspaceJSON is the public view of a workspace.
type workspaceJSON struct {
	State        store.State `json:"state"`
	MemLimitMB   int         `json:"mem_limit_mb"`
	CPULimit     float64     `json:"cpu_limit"`
	LastActiveAt *time.Time  `json:"last_active_at"`
}

func newWorkspaceJSON(w store.Workspace) workspaceJSON {
	return workspaceJSON{
		State:        w.State,
		MemLimitMB:   w.MemLimitMB,
		CPULimit:     w.CPULimit,
		LastActiveAt: w.LastActiveAt,
	}
}
