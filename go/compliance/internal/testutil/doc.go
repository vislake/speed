// Package testutil holds compliance's shared test fixtures: FakeNote, a
// minimal SoftDeletable/TenantScoped model, its dbkit.Repository[T]-backed
// FakeRepository, and NewParticipant, which wraps a FakeRepository into a
// real pkgcore.RetentionParticipant -- the fake business-module
// participant the test suite registers to prove the retention-sweep,
// right-to-erasure and export-gathering orchestration end to end, without
// modifying any real business module (opting in is each owning module's
// own decision). FixedRegistrar is the hand-built
// pkgcore.RetentionRegistrar whose participants may carry the nil
// callbacks the real registrar refuses, driving the pass driver's
// defensive nil-callback skip.
//
// FakeNote mirrors go/dbkit/internal/testutil's SoftDeletableWidget
// fixture as closely as a different-module package can: a tenant-scoped,
// SoftDeletable model with a portable, dual-dialect-safe DDL string
// (fakeNoteTableSQL) instead of migration files, since this table is a
// test fixture, never part of a real deployment's schema. See dbkit's
// SoftDeletableWidgetTableSQL doc comment for the identical reasoning.
package testutil
