package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// TenantID is the identifier of a tenant. It is the single unit of data
// isolation: tenant-scoped storage refuses to run without one.
type TenantID string

// ctxKey is a private context key type. Using a dedicated unexported type
// keeps these values from colliding with keys set by other packages, which a
// bare string or int key cannot guarantee.
type ctxKey int

const (
	// ctxKeyTenant addresses the TenantID carried by a context.
	ctxKeyTenant ctxKey = iota
	// ctxKeySystemReason addresses the SystemReason carried by a context.
	ctxKeySystemReason
)

// ErrNoTenant is returned by MustTenantFromContext when the context carries
// no usable tenant. Callers classify it with errors.Is.
var ErrNoTenant = errors.New("pkgcore: no tenant in context; tenant-scoped access requires a context built with WithTenant")

// ErrSystemActorRequired is returned by WithSystemContext when the supplied
// SystemReason has an empty Actor. Every use of the escape hatch must be
// attributable to someone.
var ErrSystemActorRequired = errors.New("pkgcore: system context requires a non-empty Actor")

// ErrSystemPurposeNotRegistered is returned by WithSystemContext when the
// supplied purpose was never declared through RegisterSystemPurpose.
var ErrSystemPurposeNotRegistered = errors.New("pkgcore: system purpose is not registered")

// WithTenant returns a copy of ctx carrying id. An empty id is stored as
// given but is never reported as a tenant by the readers below, so a caller
// that passes one fails closed instead of operating on an unscoped context.
func WithTenant(ctx context.Context, id TenantID) context.Context {
	return context.WithValue(ctx, ctxKeyTenant, id)
}

// TenantFromContext returns the TenantID carried by ctx. The second result is
// false when no tenant was set, or when the stored tenant is empty.
func TenantFromContext(ctx context.Context) (TenantID, bool) {
	id, ok := ctx.Value(ctxKeyTenant).(TenantID)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// MustTenantFromContext returns the TenantID carried by ctx, or an error
// wrapping ErrNoTenant when there is none. Despite the Must prefix it never
// panics: it is the fail-closed primitive data-access code calls to refuse to
// run against an unscoped context, and a refusal there must stay recoverable.
func MustTenantFromContext(ctx context.Context) (TenantID, error) {
	id, ok := TenantFromContext(ctx)
	if !ok {
		return "", ErrNoTenant
	}
	return id, nil
}

// SystemPurpose names why tenant filtering is being bypassed. It is a closed
// enumeration rather than free text: a purpose has to be declared through
// RegisterSystemPurpose before WithSystemContext will accept it.
type SystemPurpose string

var (
	systemPurposeMu          sync.Mutex
	registeredSystemPurposes = map[SystemPurpose]bool{}
)

// RegisterSystemPurpose records p as a purpose WithSystemContext will accept.
// Modules call it once, from their own registration, to declare the system
// purposes they use. Registering the same purpose again is a no-op, and the
// empty purpose is ignored so it can never be granted.
func RegisterSystemPurpose(p SystemPurpose) {
	if p == "" {
		return
	}
	systemPurposeMu.Lock()
	defer systemPurposeMu.Unlock()
	registeredSystemPurposes[p] = true
}

// systemPurposeRegistered reports whether p was declared through
// RegisterSystemPurpose.
func systemPurposeRegistered(p SystemPurpose) bool {
	systemPurposeMu.Lock()
	defer systemPurposeMu.Unlock()
	return registeredSystemPurposes[p]
}

// SystemReason records who is bypassing tenant filtering and why. Ticket is
// optional and links the bypass to the request that authorised it.
type SystemReason struct {
	Actor   string
	Purpose SystemPurpose
	Ticket  string
}

// WithSystemContext returns a copy of ctx carrying reason, granting the
// tenant-filtering escape hatch. It fails when Actor is empty or when Purpose
// was never registered through RegisterSystemPurpose.
//
// A system context is orthogonal to a tenant context: it sets no tenant, and
// a caller acting as a system actor inside one tenant's data combines it with
// WithTenant. On failure ctx is returned unchanged, so a caller that ignores
// the error is left without the escape hatch rather than with a nil context.
//
// The escape hatch bypasses tenant filtering only. It never bypasses
// authorization.
func WithSystemContext(ctx context.Context, reason SystemReason) (context.Context, error) {
	if reason.Actor == "" {
		return ctx, fmt.Errorf("%w (purpose %q)", ErrSystemActorRequired, reason.Purpose)
	}
	if !systemPurposeRegistered(reason.Purpose) {
		return ctx, fmt.Errorf("%w: %q; declare it with RegisterSystemPurpose",
			ErrSystemPurposeNotRegistered, reason.Purpose)
	}
	return context.WithValue(ctx, ctxKeySystemReason, reason), nil
}

// SystemReasonFromContext returns the SystemReason carried by ctx. The second
// result is false when ctx is not a system context.
func SystemReasonFromContext(ctx context.Context) (SystemReason, bool) {
	reason, ok := ctx.Value(ctxKeySystemReason).(SystemReason)
	if !ok {
		return SystemReason{}, false
	}
	return reason, true
}
