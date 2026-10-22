// Package store persists Berth's state. All SQL lives behind the Store
// interface so another database can be swapped in later.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned when a looked-up row does not exist (or, for a
	// session, has expired).
	ErrNotFound = errors.New("store: not found")
	// ErrUsernameTaken is returned by CreateUser on a duplicate username.
	ErrUsernameTaken = errors.New("store: username already taken")
)

// State is a workspace's position in the lifecycle state machine.
type State string

const (
	StateOffline  State = "offline"  // no container; only the data volume remains
	StateStarting State = "starting" // container being created/started
	StateRunning  State = "running"
	StateStopped  State = "stopped" // container kept, not running
)

// User is an account.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

// Workspace is a user's (single) workspace. A nil time means "never".
type Workspace struct {
	UserID        int64
	ContainerID   string // empty when the container was removed
	State         State
	VolumeName    string
	MemLimitMB    int
	CPULimit      float64
	LastActiveAt  *time.Time
	LastStoppedAt *time.Time
	UpdatedAt     time.Time
}

// Session is a login session. TokenHash is the hex sha256 of the cookie token.
type Session struct {
	TokenHash string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewUser carries what registration needs to create a user and its workspace.
type NewUser struct {
	Username     string
	PasswordHash string
	VolumeName   string
	MemLimitMB   int
	CPULimit     float64
}

// Store is the repository interface the rest of the backend depends on.
type Store interface {
	// CreateUser atomically inserts the user and its offline workspace row.
	// The first user ever created becomes admin. Returns ErrUsernameTaken on
	// a duplicate username.
	CreateUser(ctx context.Context, nu NewUser, now time.Time) (User, Workspace, error)
	UserByUsername(ctx context.Context, username string) (User, error)
	UserByID(ctx context.Context, id int64) (User, error)
	WorkspaceByUserID(ctx context.Context, userID int64) (Workspace, error)

	CreateSession(ctx context.Context, s Session) error
	// SessionUser returns the user owning the session, or ErrNotFound when
	// the session is unknown or expired at now.
	SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)

	Close() error
}
