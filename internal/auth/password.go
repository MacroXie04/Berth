package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// Password bounds. bcrypt only uses the first 72 bytes of its input, so longer
// passwords would be silently truncated; they are rejected instead.
const (
	MinPasswordLen   = 8
	MaxPasswordBytes = 72
)

// ErrInvalidPassword describes the password rules for display to users.
var ErrInvalidPassword = errors.New("password must be 8-72 bytes long")

// ValidatePassword enforces the length rules for new passwords.
func ValidatePassword(pw string) error {
	if len(pw) < MinPasswordLen || len(pw) > MaxPasswordBytes {
		return ErrInvalidPassword
	}
	return nil
}

// Hasher hashes and verifies passwords with bcrypt at a fixed cost.
type Hasher struct {
	cost  int
	dummy []byte // a valid hash at this cost, for equalising login timing
}

// NewHasher returns a Hasher at the given bcrypt cost. It performs one hash up
// front (the cost of a single login attempt) to prepare the dummy hash.
func NewHasher(cost int) (*Hasher, error) {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return nil, fmt.Errorf("auth: bcrypt cost %d out of range [%d, %d]", cost, bcrypt.MinCost, bcrypt.MaxCost)
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("berth-dummy-password"), cost)
	if err != nil {
		return nil, fmt.Errorf("auth: prepare dummy hash: %w", err)
	}
	return &Hasher{cost: cost, dummy: dummy}, nil
}

// Hash returns the bcrypt hash of pw. pw must already satisfy ValidatePassword.
func (h *Hasher) Hash(pw string) (string, error) {
	out, err := bcrypt.GenerateFromPassword([]byte(pw), h.cost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(out), nil
}

// Verify reports whether pw matches hash. Passwords longer than MaxPasswordBytes
// never match: bcrypt would truncate them and accept a wrong suffix. They still
// pay for a full compare, so response time does not distinguish an existing
// account from an unknown one (VerifyDummy truncates the same way).
func (h *Hasher) Verify(hash, pw string) bool {
	if len(pw) > MaxPasswordBytes {
		_ = bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw[:MaxPasswordBytes]))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// VerifyDummy burns the same CPU time as Verify against a real hash. Login
// calls it for unknown usernames so response time does not reveal whether an
// account exists.
func (h *Hasher) VerifyDummy(pw string) {
	if len(pw) > MaxPasswordBytes {
		pw = pw[:MaxPasswordBytes]
	}
	_ = bcrypt.CompareHashAndPassword(h.dummy, []byte(pw))
}
