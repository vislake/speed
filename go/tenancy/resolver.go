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
// DomainResolver's Host-based lookup. See
// docs/internal/04-data-and-tenancy.md for the full trust-boundary
// rationale.
//
// This module does not implement a Resolver for authenticated requests:
// verifying a token's signature, managing keys and validating claims is
// authn's responsibility. The module dependency graph runs authn ->
// tenancy, not the other way around, so putting that logic here would force
// a cycle once authn exists. Once it does, authn will supply its own type
// implementing this interface, reading the tenant from the already-verified
// token claims.
//
// Resolve returns a non-nil error when the tenant cannot be determined. An
// implementation must never invent or default to a tenant just to avoid
// returning one -- DomainResolver's fallback to a configured default tenant
// is a deliberate, documented exception scoped to the unauthenticated case,
// not a precedent for other implementations to follow.
type Resolver interface {
	Resolve(r *http.Request) (pkgcore.TenantID, error)
}

// DomainResolver maps a request's Host header to a tenant via a caller-supplied
// lookup, for UNAUTHENTICATED requests only (e.g. rendering the correct brand on
// a login page). It grants no data access -- it only decides what to display
// before anyone has proven who they are.
//
// DomainResolver's own logic is deliberately simple: Host -> lookup ->
// tenant. Deciding whether a given Host is a tenant's own custom domain or a
// subdomain of the platform's domain -- and stripping a subdomain label when
// it is one -- is entirely lookup's business. DomainResolver neither parses
// nor normalizes the Host header itself beyond what net/http already does
// when it populates (*http.Request).Host.
type DomainResolver struct {
	lookup        func(host string) (pkgcore.TenantID, bool)
	defaultTenant pkgcore.TenantID
}

// NewDomainResolver returns a DomainResolver that resolves a request's
// tenant by calling lookup with (*http.Request).Host. Whenever lookup
// reports no match -- including when lookup is nil, or when it reports a
// match with an empty TenantID -- Resolve falls back to defaultTenant
// rather than failing the request: per
// docs/internal/04-data-and-tenancy.md, a request that cannot be matched to
// a brand must still be able to render a login page.
func NewDomainResolver(lookup func(host string) (pkgcore.TenantID, bool), defaultTenant pkgcore.TenantID) *DomainResolver {
	return &DomainResolver{lookup: lookup, defaultTenant: defaultTenant}
}

// Resolve implements Resolver. It never returns a non-nil error: a Host
// that lookup does not recognize resolves to the default tenant configured
// with NewDomainResolver instead of failing the request.
func (d *DomainResolver) Resolve(r *http.Request) (pkgcore.TenantID, error) {
	if d.lookup != nil {
		if id, ok := d.lookup(r.Host); ok && id != "" {
			return id, nil
		}
	}
	return d.defaultTenant, nil
}

// compile-time check that *DomainResolver satisfies Resolver.
var _ Resolver = (*DomainResolver)(nil)
