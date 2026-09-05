package tenancy

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// TenantStatus is the closed vocabulary a TenantStatusResolver reports.
type TenantStatus string

const (
	// TenantStatusActive is a tenant Middleware must let requests through
	// for -- the only status a TenantStatusResolver reports for a tenant
	// nobody has suspended.
	TenantStatusActive TenantStatus = "active"

	// TenantStatusSuspended is a tenant Middleware refuses every request
	// against (see ErrTenantSuspended) once a TenantStatusResolver is
	// wired with WithTenantStatusResolver.
	TenantStatusSuspended TenantStatus = "suspended"
)

// TenantStatusResolver is an OPTIONAL, structurally-typed seam a host may
// give Middleware (via WithTenantStatusResolver) to make a tenant's
// suspended status actually refuse requests, rather than merely being a
// fact recorded somewhere no request pipeline consults.
//
// This package declares the interface and the one call site that consults
// it; it implements nothing itself and imports nothing that would. Any
// host wanting real tenant suspension implements this against whatever it
// uses to track tenant status -- go/admin's own tenant ledger (D3/D4 in
// docs/internal/23-admin.md) is this seam's first real implementer, kept
// entirely on admin's side of the boundary, the same "structurally typed,
// no import in either direction" shape org.FeatureGate and
// rbac.SubtreeResolver already use.
//
// Status is consulted by Middleware only for a request whose tenant it
// already resolved successfully -- never for an allowlisted request that
// proceeded with no tenant in context at all, since there is nothing to
// look up a status for. A Status call that returns a non-nil error fails
// the request closed with ErrTenantStatusUnavailable: an unreachable
// status source must never be treated as "assume active and let the
// request through", the identical fail-closed discipline Resolver itself
// already follows for tenant resolution.
type TenantStatusResolver interface {
	// Status returns tenant's current status. ctx carries the tenant
	// Middleware already resolved (pkgcore.TenantFromContext reports it),
	// so an implementation needing to read its own store's tenant-scoped
	// data may do so consistently with what the rest of the request will
	// see.
	Status(ctx context.Context, tenant pkgcore.TenantID) (TenantStatus, error)
}

// WithTenantStatusResolver wires r into Middleware so a resolved tenant's
// status is consulted on every request, refusing one against a suspended
// tenant with ErrTenantSuspended.
//
// This option is entirely additive and OFF by default: a host that never
// calls WithTenantStatusResolver gets exactly today's Middleware
// behavior, unchanged in every respect -- every request that resolves a
// tenant proceeds, regardless of that tenant's status anywhere else in
// the system, because nothing consults one. This is what lets the seam
// ship with no back-compat concern: an existing host recompiled against
// this version of tenancy, with no code change at all, behaves
// identically to before.
func WithTenantStatusResolver(r TenantStatusResolver) MiddlewareOption {
	return func(c *middlewareConfig) { c.statusResolver = r }
}
