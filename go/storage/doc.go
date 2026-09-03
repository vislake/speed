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
// The module ships the runtime that drives those rows through their
// lifecycle, not the metadata plane alone. ObjectService (object.go)
// drives uploads through their states: Create declares an upload and opens
// its window, Upload streams the bytes into the host's ObjectStore,
// Complete runs the revalidation pipeline over them and finalizes the row,
// and Get, OpenContent and List serve completed objects -- all enforcing
// the policy bounds the With* options carry. DeriveService (derive.go)
// turns a completed image object's bytes into its thumbnail derivative:
// the queued work Complete enqueues and the handler Register registers
// claims from the host's queue. LifecycleService (cleanup.go) ends object
// life: Delete removes an object and everything it names, Sweep runs one
// tenant's periodic cleanup, EnqueueExpirySweep schedules it. And Register
// mounts the module's HTTP surface: the spec-first api/ fragment's seven
// operations, served by the generated Handler at /api/v1/storage, with
// OpenAPISpec returning the fragment itself, embedded in the module
// binary.
package storage
