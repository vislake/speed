package config

// Scope names one tier of the three-tier configuration hierarchy
// docs/internal/11-cross-cutting.md's dynamic-config section defines:
// system (platform-wide) → tenant (per-tenant override) → user (per-user,
// reserved for a future milestone). Rows in the configs table carry one
// explicit Scope each, and reads resolve from the narrowest scope the
// context entitles the caller to down to the schema default.
type Scope string

const (
	// ScopeSystem is the platform-wide tier: every tenant reads a system row
	// unless a tenant row overrides the key. Writing one requires an audited
	// system context (a context built with pkgcore.WithSystemContext or
	// tenancy.WithSystemContext), so no tenant-scoped request can widen a
	// platform setting by accident.
	ScopeSystem Scope = "system"

	// ScopeTenant is the per-tenant override tier. Reading falls back from a
	// tenant row to the system row to the schema default; writing one takes
	// the owning tenant from the context, never from a caller-supplied
	// identifier, and requires the context to carry that tenant.
	ScopeTenant Scope = "tenant"

	// ScopeUser is the per-user tier docs/internal/11-cross-cutting.md
	// reserves. It is deliberately unimplemented in
	// this milestone: no call may write a user row, and the scope is only
	// declared so that schema and store speak the same vocabulary the design
	// does. Any Set attempt reports ErrUserScopeUnavailable rather than being
	// silently treated as a coarser scope.
	ScopeUser Scope = "user"
)

// validateScope accepts the scopes that can carry a row and reports the
// error a write to any other scope must fail with. ScopeUser is its own
// error -- a caller naming the reserved tier deserves to know it is reserved,
// not that it was mistyped -- and everything else is invalid outright.
func validateScope(scope Scope) error {
	switch scope {
	case ScopeSystem, ScopeTenant:
		return nil
	case ScopeUser:
		return ErrUserScopeUnavailable
	default:
		return ErrInvalidScope
	}
}
