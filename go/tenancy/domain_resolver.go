package tenancy

import (
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
)

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
// rather than failing the request: a request that cannot be matched to a
// brand must still be able to render a login page.
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
