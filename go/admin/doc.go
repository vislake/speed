// Package admin provides the operations-console backend, the module at the
// top of the dependency graph that assembles the platform's
// operator-facing surfaces. It ships the tenant ledger (admin_tenants) with
// suspension enforcement through tenancy's TenantStatusResolver seam, the
// impersonation pipeline, cross-tenant user search, an audit-query HTTP
// surface with an asynchronous export leg, role management over
// rbac.Service, a cross-tenant usage/billing dashboard, and notification
// send-record search.
//
// The HTTP surface is the module's own OpenAPI fragment (api/openapi.yaml),
// implemented by handler.go behind the generated api.ServerInterface.
package admin
