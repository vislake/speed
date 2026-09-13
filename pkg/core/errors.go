package core

import "errors"

// The sentinel errors the lifecycle driver returns. Callers tell the classes
// apart with errors.Is; the error text carries what is needed to locate the
// cause and states the fixing action.
var (
	// ErrMissingProvider reports that a required capability has no provider
	// that can be taken up.
	ErrMissingProvider = errors.New("core: capability has no available provider")
	// ErrAmbiguousProvider reports that a single-valued lookup found more
	// than one constructed provider.
	ErrAmbiguousProvider = errors.New("core: capability has more than one constructed provider")
	// ErrExclusiveViolated reports that a capability kept more than one
	// enabled provider through resolution while at least one of them claims
	// exclusivity.
	ErrExclusiveViolated = errors.New("core: exclusive capability has more than one enabled provider")
	// ErrMissingReason reports a module that disabled itself without saying
	// why.
	ErrMissingReason = errors.New("core: module disabled itself without a reason")
	// ErrDependencyCycle reports a cycle in the graph the Requires
	// declarations form.
	ErrDependencyCycle = errors.New("core: module dependency graph has a cycle")
	// ErrUndeliveredCapability reports a module whose product does not
	// implement a capability it declared in Provides.
	ErrUndeliveredCapability = errors.New("core: module did not deliver a declared capability")
)
