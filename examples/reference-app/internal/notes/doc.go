// Package notes is examples/reference-app's tenant-scoped business
// module: a minimal "Note" resource (id, tenant_id, text, created_at)
// demonstrating real, end-to-end usage of the whole speed module stack --
// module wiring (routes, permissions, events, audit actions),
// dbkit.Repository[T] tenant isolation, and tenancy.Middleware -- with no
// dental/business-specific content of any kind. It is the reference
// app's demonstrable content surface: other app surfaces and modules
// (the audit persister, the notification dispatch trigger, the retention
// and erasure participants) consume notes' rows and events. See
// examples/reference-app's own doc.go for the full picture and
// cmd/server/main.go for how this module is wired into a running server.
package notes
