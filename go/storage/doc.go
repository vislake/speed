// Package storage implements the storage module of the speed platform: the
// metadata that describes one tenant's stored media objects, the internal
// keys their bytes live under, and the module wiring that declares the
// whole surface to the platform registry.
//
// The module deliberately tracks metadata, not bytes. The bytes of an
// uploaded object live in the host's ObjectStore, reached through the
// pkgcore.ObjectStore seam, at a key this module builds and validates
// (key.go) from the tenant id, the object id and a fixed suffix: original
// content at <tenant>/<object>/original, generated derivatives at
// <tenant>/<object>/derivatives/<kind>. Such keys are never exposed
// through an API -- they name where the platform put a tenant's bytes, and
// revealing them would leak both the tenant's storage layout and the
// object ids they embed. Objects are stored under an id, and every other
// consumer names them by that id.
//
// One row in the objects table (model.go) holds the whole story of one
// object: what the uploader declared before any bytes arrived (size, media
// type, checksum), what the pipeline established once the bytes were in
// (finalized size, detected MIME type, SHA-256 digest, pixel dimensions),
// and where the object stands in its lifecycle (the ObjectState constants).
// Derived renditions -- thumbnails today, other kinds as needs grow --
// get one row each in object_derivatives, with their own keys. Both tables
// are tenant-scoped and reachable only through the module's repositories
// (repository.go), which embed dbkit's Repository base and so inherit its
// three isolation layers; cursor-paged object listing uses the
// (tenant_id, created_at) index the migrations ship.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// Register contributes the module's permissions (storage:read,
// storage:write), its audit actions (storage.object.create/.complete/
// .delete) and its two event types (storage.object.completed and
// .deleted), and validates the one required host seam: a jobs.Queue
// (WithQueue), refused with ErrQueueRequired when absent. Locale files
// under locales/ describe every error code in both supported languages;
// the versioned, dual-dialect migrations under migrations/ create the two
// tables from zero on SQLite and PostgreSQL alike.
//
// This round ships that metadata plane only. What it does not ship yet,
// in the order the roadmap's storage round delivers: the object service
// that drives uploads through their lifecycle states (declaring a
// completed upload, enforcing the policy bounds the With* options carry,
// moving rows to their finalized or expired states), the spec-first HTTP
// surface under api/ with the handler behind it (OpenAPISpec() returns
// nil until then, and Register mounts no routes), and the queue task that
// finalizes upload bytes and generates derivatives on the wired queue.
// Until those land, a host can store, list, read and delete object and
// derivative metadata rows, and can rely on the isolation, key and error
// contracts they were built on.
package storage
