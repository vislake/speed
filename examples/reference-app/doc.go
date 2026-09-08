// Package referenceapp is speed's mandatory first consumer: an AI smile
// simulation platform example that exercises the delivered modules end to
// end the way an external consumer would. internal/notes is a small,
// complete, tenant-scoped business module exercising the full
// pkgcore.Module contract -- routes, permissions, events, audit actions,
// migrations, locales, and an OpenAPI fragment -- and cmd/server/main.go
// wires it, together with the org, authn, rbac, storage, notification,
// sharing, integration, billing, ai-gateway, compliance and admin modules
// and this package's own demo surfaces, into a real, runnable HTTP server
// behind the composed authn + tenancy middleware chain. See
// internal/notes' own doc.go and README.md (in this directory) for how to
// run it and what it demonstrates.
package referenceapp
