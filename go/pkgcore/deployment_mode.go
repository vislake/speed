package pkgcore

import (
	"errors"
	"fmt"
	"strings"
)

// DeploymentMode identifies the runtime deployment mode the kernel and every
// module boot under. The same business code must behave identically in both
// deployment modes; only kernel wiring is allowed to branch on the value.
type DeploymentMode string

const (
	// DeploymentModeStandalone is the single-process deployment mode backed
	// by SQLite and in-memory infrastructure implementations.
	DeploymentModeStandalone DeploymentMode = "standalone"
	// DeploymentModeDistributed is the deployment mode backed by PostgreSQL,
	// Redis and the other external infrastructure dependencies.
	DeploymentModeDistributed DeploymentMode = "distributed"
)

// ErrInvalidDeploymentMode is the sentinel wrapped by every error
// ParseDeploymentMode returns, so callers can classify the failure with
// errors.Is.
var ErrInvalidDeploymentMode = errors.New("pkgcore: invalid runtime deployment mode")

// ParseDeploymentMode converts a configuration string into a DeploymentMode.
// Surrounding whitespace is trimmed and the value is matched
// case-insensitively, so "Distributed" and " distributed\n" both yield
// DeploymentModeDistributed. Any other value returns the zero DeploymentMode
// and an error wrapping ErrInvalidDeploymentMode.
func ParseDeploymentMode(s string) (DeploymentMode, error) {
	m := DeploymentMode(strings.ToLower(strings.TrimSpace(s)))
	if !m.Valid() {
		return "", fmt.Errorf("%w: %q (valid values are %q and %q)",
			ErrInvalidDeploymentMode, s, DeploymentModeStandalone, DeploymentModeDistributed)
	}
	return m, nil
}

// Valid reports whether m is one of the defined deployment modes. The
// comparison is exact: values that have not been normalised by
// ParseDeploymentMode, such as "Standalone", are not valid.
func (m DeploymentMode) Valid() bool {
	switch m {
	case DeploymentModeStandalone, DeploymentModeDistributed:
		return true
	default:
		return false
	}
}
