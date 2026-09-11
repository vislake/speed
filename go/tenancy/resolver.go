package tenancy

import (
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
)

// Resolver determines which tenant a request belongs to.
//
// Middleware consults exactly one Resolver per request and trusts its
// answer completely, so every implementation must derive the tenant from a
// source the server itself controls -- never from anything the client
// supplied on the request being resolved. For an authenticated request that
// source is the access token's claims; for an unauthenticated request it is
// DomainResolver's Host-based lookup.
//
// This module does not implement a Resolver for authenticated requests:
// verifying a token's signature, managing keys and validating claims is
// authn's responsibility. The module dependency graph runs authn ->
// tenancy, not the other way around, so an authenticated-request Resolver
// would force an import cycle if it lived here; authn supplies its own,
// reading the tenant from the already-verified token claims and composing
// with this package through tenancy.Middleware.
//
// Resolve returns a non-nil error when the tenant cannot be determined. An
// implementation must never invent or default to a tenant just to avoid
// returning one -- DomainResolver's fallback to a configured default tenant
// is a deliberate, documented exception scoped to the unauthenticated case,
// not a precedent for other implementations to follow.
type Resolver interface {
	Resolve(r *http.Request) (pkgcore.TenantID, error)
}
