// Package referenceapp is the M0-stage skeleton of speed's mandatory first
// consumer, examples/reference-app. Today it proves that the kernel
// bootstrap wiring resolves and builds end-to-end as an external consumer
// of pkgcore, dbkit, and tenancy: internal/notes is a small, complete,
// tenant-scoped business module (a placeholder "Note" resource) exercising
// the full pkgcore.Module contract -- routes, permissions, events, audit
// actions, migrations, locales, and an OpenAPI fragment -- and
// cmd/server/main.go wires it into a real, runnable HTTP server behind
// tenancy.Middleware. See internal/notes' own doc.go and README.md (in
// this directory) for how to run it and what it demonstrates.
//
// It is not yet the AI dental smile-simulation platform described in
// docs/internal/14-reference-app.md -- that full scope needs authn, org,
// storage, ai-gateway, billing, and other modules that do not exist yet --
// and will grow into that platform incrementally as each M1+ module
// lands, replacing internal/notes' placeholder content along the way.
package referenceapp
