package testkit

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// TenantCtx returns a background context carrying tenant -- the context a
// host hands a tenant-scoped call after tenancy.Middleware has resolved the
// tenant, without spinning up the middleware itself. The tenant travels on
// the context alone, which is the only way any repository ever learns which
// tenant it is acting for.
func TenantCtx(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}
