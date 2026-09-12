// Package cases is the reference app's Case/Patient domain layer -- the
// tenant-scoped record structure the case web UI renders from. It sits
// deliberately ABOVE the photo, never beside it: a smile simulation is
// generated from one go/storage photo object and durably indexed per photo
// (internal/smilesim's per-photo enumeration is the smile gallery's data
// source), and this package adds the layer that groups photos with each
// other and with the patient they belong to -- a Case (a treatment
// scenario a clinic staff member works, in this dental smile-simulation
// product's vocabulary) carrying the clinic-given patient record and the
// case's photos, with each photo's simulations remaining on the
// per-photo enumeration route.
//
// # Shape decision: one cases table with the patient embedded; one
// case_photos table per photo (no patients table, no EMR)
//
// The surface's queries -- a clinic-wide case list, a case detail showing
// its photos and their simulations, and the before/after pairing where
// one photo's simulations are the "after" candidates for that "before" --
// are all keyed by (tenant) or (tenant, case). The clinic-wide list is
// deliberately tenant-scoped, never creator-scoped: a case a receptionist
// opens must be visible to the dentist who sees the patient next, so only
// the tenant boundary (never the creator column) may hide a case from a
// colleague in the same clinic. Nothing rendered is keyed by patient: no
// per-patient page, no "all cases of this patient" list, no patient
// deduplication or merge. A separate patients table would earn its keep
// only once a patient can own several cases AND the product lists or
// dedupes by patient -- neither of which has a consumer -- so the patient
// is embedded in the cases row as the two fields a clinic intake form
// provides (a display name and an optional clinic-given reference),
// exactly the minimal "no PHI beyond what a demo needs" shape. A real
// patient entity, when one is needed, extracts from these two columns;
// nothing about this shape blocks that.
//
// Photos are a child table (case_photos), not a JSON array column on the
// case row, for three reasons: photos accrete over a case's life and each
// attachment must be its own independent tenant-scoped write (a JSON
// array's read-modify-write would silently lose a concurrent sibling
// attachment), the client's attachment order is worth a deterministic
// column (position) rather than timestamp luck, and a per-photo row is
// where later per-photo metadata (an angle label, a capture date) will
// live without a schema re-model. One row per (tenant, case, photo),
// enforced by a unique index; a photo of one case may not also be attached
// to another case of the same tenant (Service refuses with a coded
// conflict), which keeps the before/after pairing unambiguous -- a photo's
// per-photo simulation list shows in exactly one case's detail.
//
// The case layer deliberately does NOT join the simulation layer: the
// detail the HTTP surface serves returns the case with its photos, and
// each photo's simulations stay on the per-photo enumeration route
// (GET /api/v1/smile-simulation/photos/{photoObjectID}/simulations). A
// case-detail view composes the two -- one case fetch, one enumeration
// fetch per photo -- and that split is a choice, not an accident: the
// enumeration is the live per-photo data source whose entries change as
// jobs finish and new generations are requested, so per-photo lists want
// their own fetch and cache granularity, and a simulation completing must
// not invalidate the case record itself. The case package therefore has no
// dependency on smilesim, go/jobs or go/ai-gateway at all.
//
// # Surface decision: a spec fragment
//
// The case surface is a spec fragment, this package's api/ directory
// (see the fragment's own header), implemented by internal/app's handlers
// behind the generated ServerInterface with five operations: POST
// /api/v1/cases (create: patient name, optional patient reference,
// optional initial photo object ids), GET /api/v1/cases (the clinic-wide
// list -- no creator attribution needed, any authenticated member of the
// tenant may read it), GET /api/v1/cases/{id} (the case's detail with its
// photos), and the two photo operations the case view needs. Upload (POST
// /api/v1/cases/photos/upload) is the one-shot form of go/storage's
// three-step upload protocol the case UI runs before a create: the handler
// performs create/upload/complete itself over base64-encoded bytes -- JSON
// is the only transport the app's generated frontend surface carries, so
// the bytes travel encoded -- and returns the object id the create then
// references. Content (GET /api/v1/cases/{caseId}/photos/
// {photoObjectID}/content) serves one attached photo's stored bytes the
// same way, for the case view to render.
// "Close/delete case" is deliberately NOT in this surface: closing
// implies a status vocabulary and deleting implies aggregate semantics
// (the case row and its photo rows must go together, and the storage
// objects stay behind either way -- the case layer records references, it
// never owns object lifecycles), both of which are interactions a list UI
// would own when it designs them; the case is answered with a not-yet,
// recorded in this package's model.go doc comment on the cases row.
//
// # House discipline
//
// Both tables are tenant data: each model embeds dbkit.TenantModel and
// each repository embeds dbkit.Repository[T] (the multi-tenant isolation
// discipline -- the exact shape internal/smilesim's simulation index
// follows), every lookup runs inside dbkit.WithTenantSession with the
// tenant half of the WHERE clause injected by dbkit's tenant-scoping
// plugin, never written by hand (the semgrep handwritten-tenant-id-filter
// rule stays silent), and no query reaches a raw bypass entry point
// (.Table/.Model/.Raw -- the raw-gorm-bypass rule stays silent: the
// package's SQL lives in its migration set, never in a query). Both
// repositories run the mandatory tenancytest.AssertIsolated suite
// (repository_test.go). The two tables and their three lookup indexes are
// the domain's versioned migrations (internal/cases/migrations), carried
// by the "cases" component (component.go) and applied by the database
// component during the assembly's Verify stage -- the same
// dbkit.ApplyMigrations path every module's schema takes, so this app's
// own bookkeeping tables are versioned, ledgered schema like the rest.
//
// # Known limitations
//
//   - No per-case permissioning beyond the tenant: any authenticated member
//     of a tenant may create and read cases (the routes run behind the
//     app's authn+tenancy middleware chain like every other mounted
//     surface; the per-request user attribution is an identity seam, never
//     an authorization decision). rbac permissioning of the case surface
//     stays unwired, exactly as notes' surface is gated today.
//   - The case list is clinic-wide: every case of the tenant, newest
//     first, visible to every member of the tenant. It does not track work
//     assignment -- no per-user queues, no "my open cases" filter -- a
//     list-interaction decision a web UI would own; the
//     creator-scoped alternative was rejected because the product's
//     acceptance chain requires team sharing: one patient, one chart,
//     whichever colleague opens it.
//   - No patient deduplication, no real PHI handling (the two embedded
//     patient fields are treated as ordinary text; there is no encryption,
//     no consent, no identifiers beyond what a demo intake form needs), no
//     case status workflow, no close/delete, no add-photo-after-create
//     (photos arrive with the create request; attaching more later needs
//     the position-append the schema already supports but no consumer yet).
//   - Create does not verify the referenced photo objects exist or belong
//     to the tenant: go/storage's own object access controls remain the
//     protection when bytes are opened, and the case layer records
//     references only (no cross-module foreign keys).
//   - No domain events and no audit rows for case writes: nothing in this
//     app consumes either (smilesim's completion event and notes' create
//     audit exist because each had a consumer).
//   - A genuine concurrent race -- two create requests in the same tenant
//     attaching the same photo object at the same moment -- can surface as
//     an internal error from the unique index rather than the coded
//     conflict the sequential path returns; the Service's pre-flight check
//     makes the friendly answer the common case, and the race is recorded
//     rather than papered over (service.go's Create doc comment).
package cases
