// Package rbac is speed's role-based access control engine: the module
// that answers "may this subject perform this action on this resource,
// and over which slice of the organization tree".
//
// It is a native engine over dbkit-backed, tenant-scoped tables, not a
// Casbin deployment. Casbin's policy store model was rejected because
// casbin_rule has no tenant_id column at all -- the tenant lives inside a
// policy string -- which would opt the single most security-critical table
// in the product out of all three tenant-isolation layers (the GORM
// plugin's injected filter, dbkit.Repository[T], and PostgreSQL row-level
// security), and makes tenancytest.AssertIsolated impossible to run
// against it. The model's semantics are preserved:
// domain equals tenant, resource:action permission naming, a "system"
// pseudo-tenant for platform-operations grants, and materialized-path
// prefix matching for subtree scope.
//
// The defining boundary rule of this module: rbac must never depend on
// authn. Authorization knows only Subject{TenantID, UserID}; whoever
// authenticates assembles the Subject and calls in. The two facts rbac
// needs from the outside -- who the subject is, and where an organization
// node sits in the tree -- both arrive through interfaces declared here
// and implemented by the host, so neither authn nor org is ever imported.
package rbac
