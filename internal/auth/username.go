// Package auth holds the pure authentication primitives: username and
// password rules, bcrypt hashing, and session tokens. It has no HTTP or
// database dependencies.
package auth

import (
	"errors"
	"regexp"
)

// Username bounds. A username is interpolated into Docker container, volume
// and network names, Traefik router names, and URL paths, so the accepted
// alphabet is deliberately tiny: lowercase letters, digits and single inner
// hyphens, starting with a letter. No dots (they break Traefik label keys),
// no underscores (invalid in DNS names), no uppercase (case-folding clashes).
const (
	MinUsernameLen = 3
	MaxUsernameLen = 32
)

var usernameRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ErrInvalidUsername describes the username rules for display to users.
var ErrInvalidUsername = errors.New(
	"username must be 3-32 characters: lowercase letters, digits and single hyphens, starting with a letter")

// ValidUsername reports whether s is an acceptable username.
func ValidUsername(s string) bool {
	return len(s) >= MinUsernameLen && len(s) <= MaxUsernameLen && usernameRE.MatchString(s)
}

// ValidateUsername returns ErrInvalidUsername unless s is acceptable.
func ValidateUsername(s string) error {
	if !ValidUsername(s) {
		return ErrInvalidUsername
	}
	return nil
}
