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
	// ErrStateConflict is returned by TransitionWorkspace when the workspace
	// exists but is not in any of the expected states: another request got
	// there first.
	ErrStateConflict = errors.New("store: workspace state conflict")
	// ErrNoPortsFree is returned by AllocateUDPRange when the configured range
	// has no free block left.
	ErrNoPortsFree = errors.New("store: no free UDP port range")
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

	// UDPPortStart..UDPPortEnd (inclusive) is the container's published WebRTC
	// range; both are zero while no container holds one.
	UDPPortStart int
	UDPPortEnd   int
	// NekoPassword configures the neko web UI; empty until the first container
	// is created.
	NekoPassword string
}

// Session is a login session. TokenHash and WSTokenHash are the hex sha256 of
// the two cookie tokens (API scope and workspace scope).
type Session struct {
	TokenHash   string
	WSTokenHash string
	UserID      int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
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
	// WorkspaceSessionUser is SessionUser for the workspace-scoped token.
	WorkspaceSessionUser(ctx context.Context, wsTokenHash string, now time.Time) (User, error)
	// DeleteSession revokes the session holding this API token (both tokens die).
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)

	// TransitionWorkspace atomically moves the workspace to state `to` if and
	// only if it is currently in one of `from` (compare-and-swap), applying the
	// options in the same statement. It returns ErrStateConflict when the
	// workspace is in another state and ErrNotFound when it does not exist.
	// from == [to] is allowed: it just updates fields.
	TransitionWorkspace(ctx context.Context, userID int64, from []State, to State, now time.Time,
		opts ...TransitionOption) (Workspace, error)
	// AllocateUDPRange gives the workspace a block of `size` ports inside
	// [min, max] that overlaps no other workspace's block, or returns the block
	// it already holds. ErrNoPortsFree when none fits.
	AllocateUDPRange(ctx context.Context, userID int64, min, max, size int, now time.Time) (start, end int, err error)
	// TouchLastActive records activity for a starting or running workspace.
	TouchLastActive(ctx context.Context, userID int64, now time.Time) error

	Close() error
}
