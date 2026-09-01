package pkgcore

import (
	"errors"
	"fmt"
	"strings"
)

// Profile identifies the runtime profile the kernel and every module boot
// under. The same business code must behave identically in both profiles;
// only kernel wiring is allowed to branch on the value.
type Profile string

const (
	// ProfileDemo is the single-process profile backed by SQLite and
	// in-memory infrastructure implementations.
	ProfileDemo Profile = "demo"
	// ProfileProduction is the profile backed by PostgreSQL, Redis and the
	// other external infrastructure dependencies.
	ProfileProduction Profile = "production"
)

// ErrInvalidProfile is the sentinel wrapped by every error ParseProfile
// returns, so callers can classify the failure with errors.Is.
var ErrInvalidProfile = errors.New("pkgcore: invalid runtime profile")

// ParseProfile converts a configuration string into a Profile. Surrounding
// whitespace is trimmed and the value is matched case-insensitively, so
// "Production" and " production\n" both yield ProfileProduction. Any other
// value returns the zero Profile and an error wrapping ErrInvalidProfile.
func ParseProfile(s string) (Profile, error) {
	p := Profile(strings.ToLower(strings.TrimSpace(s)))
	if !p.Valid() {
		return "", fmt.Errorf("%w: %q (valid values are %q and %q)",
			ErrInvalidProfile, s, ProfileDemo, ProfileProduction)
	}
	return p, nil
}

// Valid reports whether p is one of the defined profiles. The comparison is
// exact: values that have not been normalised by ParseProfile, such as
// "Demo", are not valid.
func (p Profile) Valid() bool {
	switch p {
	case ProfileDemo, ProfileProduction:
		return true
	default:
		return false
	}
}
