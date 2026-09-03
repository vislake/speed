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

// RequiredCapabilities returns the Capability every seam a Kernel bootstraps
// under m must declare. DeploymentModeDistributed requires MultiReplicaSafe,
// because more than one replica may be running at once and every seam
// carries state the replicas must share; DeploymentModeStandalone requires
// nothing extra, because a single process has no other replica to share
// state with -- a standalone composition may still choose a
// MultiReplicaSafe, multi-replica-capable implementation (a single binary
// talking to real PostgreSQL, real Redis or real SMTP is ordinary
// small-customer production, not a misuse), RequiredCapabilities simply does
// not demand one.
//
// This is the direct replacement for the deployment-mode-keyed switch that
// used to live inside Kernel.Bootstrap's four ErrMissingDistributed* checks:
// with N implementations per seam, "the distributed deployment mode has no
// implementation to fall back on" no longer describes anything, so the mode
// contributes only a capability requirement, and Bootstrap compares it
// against whatever a Preset or a KernelOption resolved.
//
// An invalid DeploymentMode (not DeploymentModeStandalone or
// DeploymentModeDistributed) is not this method's concern to reject --
// Kernel.Bootstrap validates m.Valid() itself before ever asking for its
// required capabilities -- so RequiredCapabilities falls back to the
// standalone requirement, 0, for any value that is not
// DeploymentModeDistributed.
func (m DeploymentMode) RequiredCapabilities() Capability {
	if m == DeploymentModeDistributed {
		return MultiReplicaSafe
	}
	return 0
}
