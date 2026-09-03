// Package authn is speed's authentication module: it owns who a caller is,
// never what that caller may do.
//
// The split matters because it is a hard architectural boundary rather than a
// naming preference. authn holds passwords, sessions, tokens, social and
// enterprise identity bindings, and second factors; it assembles the
// authenticated identity and hands it downstream. Authorization -- roles,
// permissions, policy evaluation -- belongs to rbac, which must never import
// this package: rbac only ever sees the two fields it needs, and the
// authenticating side is what puts them together. See the module dependency
// direction in docs/internal/01-architecture.md.
//
// # What this package produces
//
// The output of authentication is a [Principal]: the user, the tenant whose
// data the request is acting inside, the session the request belongs to, and
// the authentication methods that session was established with. A Principal
// deliberately carries no roles and no permissions.
//
// # Data domain
//
// Every table this module owns except the tenant SSO configuration is
// identity data, not tenant data (docs/internal/04-data-and-tenancy.md): a
// person can belong to several tenants, so users, their sessions, their
// refresh tokens and their login history cannot be scoped to one. That is
// why none of those models implements [github.com/vislake/speed/go/dbkit.TenantScoped]
// and why their repositories hold a plain *gorm.DB rather than embedding
// dbkit.Repository[T] -- see repository.go's file comment for the full
// justification, and go/authn/AGENTS.md for the rule a reviewer should apply.
//
// # Sessions, tenants and tokens
//
// A session belongs to a user, not to a tenant. One login creates one
// session, and within it the user switches tenants freely: switching issues a
// new access token carrying the new tenant while reusing the same session and
// the same refresh token. Access tokens are short-lived and carry the current
// tenant, so a request's tenant context comes from the token itself and the
// server does not query the database to establish it. Refresh tokens are
// long-lived, opaque, stored hashed, bound to a session, and rotated on every
// use with replay detection -- see session.go.
package authn
