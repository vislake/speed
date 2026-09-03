package config

import "testing"

// Tests for validateScope, the write-side gate that accepts the two tiers
// that can carry a row and reports the error a write to any other scope
// must fail with.

func TestValidateScope_AcceptsRowCarryingScopes(t *testing.T) {
	for _, scope := range []Scope{ScopeSystem, ScopeTenant} {
		if err := validateScope(scope); err != nil {
			t.Fatalf("validateScope(%q) returned an error: %v", scope, err)
		}
	}
}

func TestValidateScope_ReportsReservedUserTierAsReserved(t *testing.T) {
	// ScopeUser is deliberately unimplemented; a caller naming it deserves
	// to learn it is reserved, not that it was mistyped.
	err := validateScope(ScopeUser)
	assertCode(t, err, ErrUserScopeUnavailable)
}

func TestValidateScope_RejectsEverythingElse(t *testing.T) {
	for _, scope := range []Scope{"", "bogus", "TENANT", Scope("config")} {
		err := validateScope(scope)
		assertCode(t, err, ErrInvalidScope)
	}
}
