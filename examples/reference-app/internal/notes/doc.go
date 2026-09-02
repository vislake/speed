// Package notes is examples/reference-app's placeholder tenant-scoped
// business module: a minimal "Note" resource (id, tenant_id, text,
// created_at) demonstrating real, end-to-end usage of the whole speed
// module stack -- pkgcore.Module wiring (routes, permissions, events,
// audit actions), dbkit.Repository[T] tenant isolation, and
// tenancy.Middleware -- with no dental/business-specific content of any
// kind. It stands in for the real reference-app modules that land in
// later milestones once authn, org and the other M1+ modules exist; see
// examples/reference-app's own doc.go for the full picture and
// cmd/server/main.go for how this module is wired into a running server.
package notes
