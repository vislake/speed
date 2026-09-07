// Package cases is the reference app's Case/Patient domain layer (product
// round P2b) -- the tenant-scoped record structure the P3 web UI will sit
// on. It sits deliberately ABOVE the photo, never beside it: the P2a round
// established that a smile simulation is generated from one go/storage
// photo object and durably indexed per photo (internal/smilesim's
// per-photo enumeration, the P3 gallery's data source); this round adds the
// layer that groups photos with each other and with the patient they belong
// to -- a Case (a treatment scenario a clinic staff member works, in this
// dental smile-simulation product's vocabulary) carrying the clinic-given
// patient record and the case's photos, with each photo's simulations
// remaining exactly where P2a put them.
//
// # Shape decision: one cases table with the patient embedded; one
// case_photos table per photo (no patients table, no EMR)
//
// The P3 UI's queries -- a clinic-wide case list, a case detail showing
// its photos and their simulations, and the before/after pairing where
// one photo's simulations are the "after" candidates for that "before" --
// are all keyed by (tenant) or (tenant, case). The clinic-wide list was
// the block-A product decision: the P2b round shipped the list scoped to
// the creating user ("my cases"), and the acceptance review found that
// wrong for the product -- a case a receptionist opens must be visible to
// the dentist who sees the patient next, so only the tenant boundary
// (never the creator column) may hide a case from a colleague in the
// same clinic. Nothing P3 renders is keyed by
// patient: no per-patient page, no "all cases of this patient" list, no
// patient deduplication or merge. A separate patients table earns its keep
// only once a patient can own several cases AND the product lists or
// dedupes by patient -- neither of which this round has a consumer for --
// so the patient is embedded in the cases row as the two fields a clinic
// intake form provides (a display name and an optional clinic-given
// reference), exactly the minimal "no PHI beyond what a demo needs" shape
// the round brief draws. A future round that needs a real patient entity
// extracts it from these two columns; nothing about this shape blocks that.
//
// Photos are a child table (case_photos), not a JSON array column on the
// case row, for three reasons: photos accrete over a case's life and each
// attachment must be its own independent tenant-scoped write (a JSON
// array's read-modify-write would silently lose a concurrent sibling
// attachment), the client's attachment order is worth a deterministic
// column (position) rather than timestamp luck, and a per-photo row is
// where P3's later per-photo metadata (an angle label, a capture date)
// will live without a schema re-model. One row per (tenant, case, photo),
// enforced by a unique index; a photo of one case may not also be attached
// to another case of the same tenant (Service refuses with a coded
// conflict), which keeps the before/after pairing unambiguous -- a photo's
// per-photo simulation list shows in exactly one case's detail.
//
// The case layer deliberately does NOT join the simulation layer: the
// detail this round's HTTP surface serves returns the case with its photos,
// and each photo's simulations stay on the P2a enumeration route
// (GET /api/v1/smile-simulation/photos/{photoObjectID}/simulations). A P3
// case-detail view composes the two -- one case fetch, one enumeration
// fetch per photo -- and that split is a choice, not an accident: the
// enumeration is the live per-photo data source whose entries change as
// jobs finish and new generations are requested, so per-photo lists want
// their own fetch and cache granularity, and a simulation completing must
// not invalidate the case record itself. The case package therefore has no
// dependency on smilesim, go/jobs or go/ai-gateway at all.
//
// # Surface decision: a spec fragment, extended by the block-A web round
//
// Product round P3a promoted the case surface from hand-mounted routes to
// the fragment this package's api/ directory ships (see the fragment's
// own header for the promotion's rationale), and the block-A web round
// widened it from three operations to five: the clinic-wide list flip
// (Service.List below), plus the photo upload and photo-content
// operations the browser surface needs. Upload (POST /api/v1/cases/
// photos/upload) is the one-shot form of go/storage's three-step upload
// protocol the cases UI runs before a create: the handler performs
// create/upload/complete itself over base64-encoded bytes -- JSON is the
// only transport the app's generated frontend surface carries, so the
// bytes travel encoded -- and returns the object id the create then
// references. Content (GET /api/v1/cases/{caseId}/photos/
// {photoObjectID}/content) serves one attached photo's stored bytes the
// same way, for the case view to render. The full surface, all five
// operations declared in the fragment and implemented by cmd/server's
// handlers behind the generated ServerInterface: POST /api/v1/cases
// (create: patient name, optional patient reference, optional initial
// photo object ids), GET /api/v1/cases (the clinic-wide list -- no
// creator attribution needed, any authenticated member of the tenant
// may read it), GET /api/v1/cases/{id} (the case's detail with its
// photos), the photo upload and the photo content reads above.
// "Close/delete case" is deliberately NOT in this surface: closing
// implies a status vocabulary and deleting implies aggregate semantics
// (the case row and its photo rows must go together, and the storage
// objects stay behind either way -- the case layer records references, it
// never owns object lifecycles), both of which are decisions the P3 UI
// round should own when it designs the list's interactions; the round
// brief's "maybe" is answered with the case for not-yet, recorded honestly
// in this package's model.go doc comment on the cases row.
//
// # House discipline
//
// Both tables are tenant data: each model embeds dbkit.TenantModel and
// each repository embeds dbkit.Repository[T] (root CLAUDE.md's
// multi-tenant isolation rule -- the exact shape the P2a follow-up round
// enforced on smilesim's simulation index), every lookup runs inside
// dbkit.WithTenantSession with the tenant half of the WHERE clause injected
// by dbkit's tenant-scoping plugin, never written by hand (the semgrep
// handwritten-tenant-id-filter rule stays silent), and no query reaches a
// raw bypass entry point (.Table/.Model/.Raw -- the raw-gorm-bypass rule
// stays silent; the only imperative SQL is the CREATE TABLE IF NOT EXISTS
// schema bootstrap, the same EnsureSchema shape smilesim's stores use,
// executed through Exec like theirs). Both repositories run the mandatory
// tenancytest.AssertIsolated suite (repository_test.go). Table creation
// follows the app's CREATE TABLE IF NOT EXISTS pattern (the P2a precedent
// internal/smilesim/simulation_store.go sets and documents), not
// dbkit.MigrationRegistry: these tables are bookkeeping specific to one
// reference-app package, with no other consumer and nothing shipped to a
// consuming project.
//
// # Honest record: what this round does NOT build
//
//   - No per-case permissioning beyond the tenant: any authenticated member
//     of a tenant may create and read cases (the routes run behind the
//     app's authn+tenancy middleware chain like every other hand-mounted
//     surface; the per-request user attribution is an identity seam, never
//     an authorization decision). rbac permissioning of the case surface
//     is P3 web work, exactly as notes' surface is gated today.
//   - The case list is clinic-wide (the block-A decision): every case of
//     the tenant, newest first, visible to every member of the tenant.
//     What the list deliberately does not do is track work assignment --
//     no per-user queues, no "my open cases" filter -- which is P3 web
//     work when the list's interactions are designed (the P2b round's
//     creator-scoped list was the earlier answer, replaced by this one
//     because the product's acceptance chain requires team sharing: one
//     patient, one chart, whichever colleague opens it).
//   - No patient deduplication, no real PHI handling (the two embedded
//     patient fields are treated as ordinary text; there is no encryption,
//     no consent, no identifiers beyond what a demo intake form needs), no
//     case status workflow, no close/delete, no add-photo-after-create
//     (photos arrive with the create request; attaching more later needs
//     the position-append the schema already supports but no consumer yet).
//   - Create does not verify the referenced photo objects exist or belong
//     to the tenant: go/storage's own object access controls remain the
//     protection when bytes are opened, and the case layer records
//     references only (no cross-module foreign keys, per root CLAUDE.md).
//   - No domain events and no audit rows for case writes: nothing in this
//     app consumes either today (smilesim's completion event and notes'
//     create audit exist because each had a consumer); P3 can add them
//     when a consumer exists, on the same registries notes and smilesim
//     already demonstrate.
//   - A genuine concurrent race -- two create requests in the same tenant
//     attaching the same photo object at the same moment -- can surface as
//     an internal error from the unique index rather than the coded
//     conflict the sequential path returns; the Service's pre-flight check
//     makes the friendly answer the common case, and the race is recorded
//     rather than papered over (service.go's Create doc comment).
//
// What P3 will need that this shape already serves: the case list query
// (clinic-wide, newest first), the detail query (case plus ordered
// photos), the per-photo simulation pairing (each photo's object id
// feeding the P2a enumeration route), and room for per-photo metadata,
// an add-photo position-append and a real patient extraction without
// re-modeling what this round ships.
package cases
