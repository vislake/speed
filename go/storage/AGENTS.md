# storage

go/storage is the platform's media-object module: the metadata that describes one
tenant's stored objects, the internal keys their bytes live under, and the transfer
lifecycle that moves those bytes into and out of the host's object store with
server-side revalidation. It sits on the `dbkit` / `jobs` tier of the dependency
graph and is consumed by modules above it that handle media (ai-gateway, sharing).

**Status: implemented and tested** — the metadata model, both repository
types, the key grammar, the migration sets for both dialects, the bilingual
locale bundles, the module wiring, and the full object lifecycle across the
three services `NewModule` builds: `ObjectService`'s transfer lifecycle with
its revalidation pipeline, `DeriveService`'s thumbnail derivation, and
`LifecycleService`'s crash-convergent delete protocol and expiry sweep (see
"Deferred and not shipped" for what is deliberately absent). All gates run
green: `go build ./...`, `go vet ./...`, `golangci-lint run ./...`,
`go test ./... -race` (from this directory), plus the workspace
`go build github.com/vislake/speed/go/...` form.

## What the module tracks

The module owns **metadata, not bytes**. An object's bytes live in the host's
`pkgcore.ObjectStore` (local directory in standalone deployments, S3 in
distributed ones — this module never knows which) at a key built by `key.go` and
never exposed through any API: original content at `<tenant>/<object>/original`,
generated derivatives at `<tenant>/<object>/derivatives/<kind>`. Keys embed the
tenant id and object id, so revealing one would leak both the tenant's storage
layout and its object ids; every consumer names objects by id.

One row in `objects` (`model.go`) carries the whole story of one object: what the
uploader declared before any bytes arrived (size, media type, optional SHA-256
checksum, optional retention), what the pipeline established once the bytes were
in (finalized size, probed MIME, digest, pixel dimensions), and where the object
stands in its lifecycle:

- `uploading` — row reserved, upload window open (`UploadExpiresAt`),
- `completed` — bytes passed the full revalidation pipeline, object readable,
- `deleting` — a delete protocol in flight (`LifecycleService.Delete`'s
  guarded mark; the crash-convergent protocol behind it is documented under
  "Ending object life: LifecycleService" below).

Both tables are tenant-scoped and reachable only through the module's repositories
(`repository.go`), which embed `dbkit.Repository[T]` and so inherit all three
isolation layers; both run the shared `tenancytest.AssertIsolated` suite
(`repository_test.go`).

## The transfer lifecycle: ObjectService

`Module.ObjectService()` returns the module's runtime, constructed by `NewModule`
after the `With*` options have been applied and handed the registry when
`Register` runs. Its methods:

- `Create(ctx, CreateParams{DeclaredSize, DeclaredType, DeclaredChecksum, Retention})`
  validates the declaration and reserves the row in `ObjectStateUploading`.
  Refusals happen here, before any byte transfers: non-positive size
  (`storage.invalid_size`), size above the module ceiling
  (`storage.object_too_large`, param `max_bytes`), malformed checksum
  (`storage.invalid_checksum`), a declared type outside the allowlist
  (`storage.type_not_allowed`), retention past the module maximum
  (`storage.invalid_expiry`). The declared type is canonicalized (parameters
  stripped, case folded) before storage; the declared checksum must be 64
  lowercase hex characters, refused otherwise so no two spellings of one digest
  can drift apart. A context without a tenant fails closed (`storage.internal_error`
  wrapping `pkgcore.ErrNoTenant`).
- `Upload(ctx, objectID, contentLength *int64, body)` streams the body into the
  host's store, bounded byte for byte by the declared size. The optional
  `contentLength` is the transport-observed length when one exists: a value that
  disagrees with the declaration is refused before any store write
  (`storage.content_length_mismatch`, param `declared`); nil means the byte count
  is reconciled alone. An upload of a row past its window or not in `uploading`
  state is refused (`storage.content_missing` / `storage.object_not_uploading`,
  param `id`). A short or oversize body is refused after the store write and the
  partial bytes are deleted best-effort (`storage.size_mismatch`); cleanup
  failure degrades to a warning log, never a silent success.
- `Complete(ctx, objectID)` runs the revalidation pipeline over the stored bytes
  and finalizes the row (see below). Side effects — the completion event publish
  and the thumbnail-derive enqueue — cannot fail the call; they warn on failure.
- `Get`, `OpenContent` and `List` serve **completed** objects only.
  `OpenContent` returns the row and a read stream off the store; a missing or
  not-yet-completed object reads as `storage.object_not_found` (param `id`).
  `List(ctx, limit, beforeID)` pages completed objects newest first on a
  (created_at desc, id desc) keyset cursor, `limit <= 0` meaning the default page
  of 50.

Store and bus arrive through two seams read at call time, never by import: the
service holds the registry `Register` handed it (`attach`) and calls
`Registry.ObjectStore()` / `Registry.EventBus()` per operation, so a revoke or a
replacement is honored by the next call. Before `Register`, or on a registry
carrying no store, every store-needing method fails closed with
`storage.store_unavailable` rather than panicking on a nil store.

### The revalidation pipeline (validate.go, sanitize.go)

`Complete`'s checks run in a settled order over the bytes as they actually
arrived; the declared values are claims, and the probe of the stored bytes is the
authority:

1. State gate and upload-window check (`storage.object_not_uploading` /
   `storage.content_missing`).
2. Stored size reconciled with the declaration (`storage.size_mismatch`).
3. Stored SHA-256 reconciled with the declared checksum when one was declared
   (`storage.checksum_mismatch`). No checksum declared, no comparison.
4. Media type probed from content magic bytes (`http.DetectContentType` — never a
   filename or a caller-controlled header), checked against the allowlist
   (`storage.type_not_allowed`, param `allowed`), then the declared type checked
   against the probe when one was declared (`storage.type_mismatch`).
5. Images decode their header only (`image.DecodeConfig` — no full decode in this
   round) and their pixel count is checked against the module ceiling
   (`storage.pixel_limit_exceeded`, param `max_pixels`; an undecodable header is
   `storage.image_unreadable`).
6. The metadata strip (`sanitize.go`) removes location and authorship metadata
   before the object is readable: JPEG APP1 segments carrying EXIF (the container
   that holds GPS coordinates and camera authorship) and Adobe XMP, and PNG's
   `eXIf` chunk. Stripping is **structural, not re-encoding**: decodable pixel
   data passes through untouched, and the walkers are strict about structure
   (bounds, lengths, CRCs, required IEND terminator) and fail closed on anything
   they cannot account for — a file whose metadata could be stripped but whose
   structure cannot be verified is refused, never passed through on good faith.
   When the strip rewrites the bytes they replace the stored content, and the
   finalized row reflects the bytes actually stored.
7. The row advances to `completed`; the module logs `object completed`
   (attrs `object_id`, `size`, `mime`, `sanitized`), enqueues thumbnail
   derivation, and publishes `storage.object.completed`.

The checksum/media-type helpers `Create` uses to refuse malformed declarations
live in the same validate.go, so the pipeline ordering policy lives with its one
consumer and every primitive is testable on its own.

## Derived content: DeriveService

`Module.DeriveService()` returns the runtime that turns a completed image
object's stored bytes into its thumbnail derivative — the module's first (and
so far only) consumer of the `object_derivatives` rows and derivative-key
grammar, the bytes the completion pipeline's enqueue refers to but never waits
for. Its one method, `DeriveThumbnail`, is the derive worker's body (the
handler Register registers) and doubles as a synchronous entry point for a
host that wants one object's thumbnail in-call.

A derive reads the object's sanitized original from the store (the same bytes
`OpenContent` serves), downscales it with a pure-stdlib exact area average to
the configured longer-edge cap, encodes the result in the source's own format
(JPEG at a fixed quality 75, PNG lossless), puts the bytes under the
derivative key, and only then inserts the derivative row — row-last, so a
crash between the two leaves nothing but re-derivable bytes, never a row that
points at missing content. Re-running a finished derive is a no-op (the row's
existence is checked before any work, and the repository's insert-if-absent
write closes the same race at the insert), and a derive of something that has
nothing to derive from — an object that is gone, not completed, not an image,
or of an image media type this service has no encoder for — is a logged skip,
not an error: a job that converges on nothing to do must complete cleanly, or
the queue would re-run it into a dead letter. Genuine failures — store errors,
undecodable content, a source over the pixel ceiling (re-checked against the
stored bytes before the full decode, so a worker never decodes an image the
transfer pipeline already refused) — are errors, which is exactly what the
jobs layer's retry policy exists for.

The derive worker is the delete protocol's race partner, and the two converge
as cleanup.go's header records: bytes the worker wrote after the delete's walk
are dropped on the worker's own re-read, and the microsecond window the two
still share is closed by the sweep, which treats a ghost derivative as an
object's ordinary row and removes it — and the bytes it names — with the next
resumed run (see "Known limitations").

## Ending object life: LifecycleService

`Module.LifecycleService()` returns the module's deletion and expiry runtime,
built by `NewModule` next to the other two services from the same repositories
and queue, and inert until `Register` attaches the registry like them.

`Delete(ctx, objectID)` runs the crash-convergent delete protocol: mark the
row (completed → deleting, a guarded flip), remove the original bytes from
the store, list the derivative rows in the repository's deterministic order
and remove each one's bytes, then remove all the rows in one transaction —
the protocol's commit point. The mark is the protocol's crash point: once the
row reads deleting, its readers already see nothing (every read surface serves
completed rows only), and every later step can be re-run safely — byte removal
is idempotent per the store's `DeleteObject` contract, the derivative walk
lists what still exists, and the row removal reports whether it committed — so
a run interrupted at any step leaves work the next run converges, never
duplicates. The run whose row removal commits — exactly one, however many
runs raced over the object — logs the deletion and publishes
`storage.object.deleted`; publishing is warn-and-stand like the completion
event's. `Delete` is idempotent end to end: a caller may run it any number of
times, concurrently included, and a run that finds the object already gone —
deleted by an earlier run, or belonging to another tenant — converges on nil,
never an error. It refuses exactly one state: an object still `uploading`
reports `storage.object_uploading` and is left untouched, because an upload in
flight belongs to the transfer runtime and may still complete — only the sweep
reclaims uploading rows, once their window closed. Store errors stop the
protocol with the row left deleting and the error reported
(`storage.store_unavailable` when no store is wired at all, `storage.store_error`
for a failed removal) — the mark survives, so a later run finishes the
deletion.

`Sweep(ctx)` runs one tenant's full cleanup pass on one captured `now`: it
resumes every interrupted deletion (each `deleting` row re-runs the protocol
and announces its event), reclaims every upload whose window closed (bytes and
rows removed silently — nothing ever read the upload, so no subscriber has
anything to forget), and deletes every completed object whose retention
deadline passed (the same protocol and event as an explicit delete;
`expires_at` NULL — an object that never expires — is skipped). Each phase
fails fast on the first refusing row, leaving that row in a state the next
pass resumes: deleting rows stay deleting, expired rows stay as they were.
Reclaiming an upload is safe against a completion racing it only because the
upload window is enforced at the finalize write itself, not at listing time:
`finalizeUpload` refuses a completion whose window closed mid-flight
(repository.go; the write-time deadline this round's fix shipped), so a row
this sweep listed can never complete behind its back — either the completion
committed before the window closed, in which case the row is completed and no
longer matches the reclaim listing, or the write is refused and the row is
reclaimed. Rows a concurrent sweep already removed are nothing left to do, not
errors.

`EnqueueExpirySweep(ctx)` puts one tenant's sweep on the queue as task
`storage.expiry_sweep`, tenant-scoped because every query the sweep runs is:
a host with many tenants schedules one task per tenant (a platform loop is the
ordinary shape), and the tenant rides in the task's `TenantID` — rebuilt into
context by the worker before the handler runs, never inherited from the
enqueuing side. The task carries no payload (the sweep reads the rows and the
clock at run time) and a deterministic per-tenant idempotency key
(`storage.sweep:<tenant>`), so concurrent enqueues — a scheduler with
replicas, a manual re-run — collapse into one job and a tenant is never swept
by two workers at once. A nil queue makes the enqueue fail with a plain error:
sweeping is optional work, and a host that runs no workers must not be forced
to wire a queue it cannot drain — the module's queue requirement is about the
thumbnail work the completion pipeline already promised (see "Known
limitations" for who actually schedules sweeps).

## Module wiring

`NewModule(db *gorm.DB, opts ...Option)` builds the repositories and the three
services above. The `With*` options override named package defaults — never
magic numbers:

| Option | Default | Meaning |
|---|---|---|
| `WithMaxUploadBytes` | 100 MiB | single-object ceiling, enforced at create and by the bounded upload |
| `WithMaxImagePixels` | 40 000 000 | image pixel ceiling, enforced at complete |
| `WithDerivativeMaxEdge` | 320 px | longer-edge cap `DeriveService` downscales generated derivatives to |
| `WithUploadTTL` | 30 min | how long a declared upload may stay unfinished |
| `WithMaxObjectLifetime` | 90 days | retention ceiling, enforced at create |
| `WithAllowedTypes` | image/jpeg, image/png | media-type allowlist; nil resolves to the module default |

`Register(reg *pkgcore.Registry)` performs no I/O and:

- requires the queue `WithQueue` wired — a queueless Register fails with
  `storage.queue_required` before declaring anything;
- declares permissions `storage:read` and `storage:write`, audit actions
  `storage.object.create` / `storage.object.complete` / `storage.object.delete`,
  and the published events `storage.object.completed` (payload
  `storage.ObjectCompleted`: `object_id`, `size`, `mime`) and
  `storage.object.deleted` (payload `storage.ObjectDeleted`: `object_id`);
- attaches the registry to all three services (`attach`) — plain assignments;
- claims the handlers of the two task types the module's services schedule on
  `reg.Jobs`: the thumbnail-derive task the completion pipeline enqueues
  (derive.go's `deriveHandler`, backed by `DeriveService`) and the
  expiry-sweep task `EnqueueExpirySweep` schedules (cleanup.go's
  `expirySweepHandler`, backed by `LifecycleService`) — catalog insertions a
  host drains onto its queue after Bootstrap and gets a worker that produces
  thumbnails and sweeps expiry.

Audit actions are declared but **not emitted** by the services; the
reference-app pattern of explicit `audit.Emit` calls under already-declared
actions is the standing route for hosts that need rows now (see "Deferred and
not shipped").

Error codes live in `errors.go` as `*apperr.Error` vars, each with its status
class, a canonical `storage.*` code, and bilingual `zh-CN` / `en-US` message
entries in `locales/` (identical key sets, pinned by `errors_test.go`). Frontends
map codes to text; no localized string crosses an API.

## Testing

Unit files map 1:1 onto their sources (`object.go` → `object_test.go`,
`validate.go` → `validate_test.go`, ...). The suite shares its scaffolding
across files rather than duplicating it: `repository_test.go` defines the
same-package helpers the test files reuse (`newTestDB`, `tenantCtx`, the
`newUpload` / `newCompleted` / `seedObject` builders), and
`internal/testutil` hosts the cross-package fixtures (a migrated SQLite
connection harness, deterministic JPEG and PNG images). Highlights, all in the
plain unit suite under `-race`:

- `object_test.go` — the 24-function lifecycle matrix: create validation and
  canonicalization, state and upload-window gates, exact-byte streaming,
  content-length and size reconciliations, checksum/type/pixel refusals, JPEG and
  PNG strip rewrites whose finalized digest describes the stored bytes, refusal
  of undecodable images, side-effect failures that do not fail the finalize, the
  completion event and derive enqueue, completed-rows-only reads, and
  fail-closed behavior without an attached host;
- `module_test.go` — the register-time wiring proof: after a real standalone
  `pkgcore.Kernel.Bootstrap`, `Module.Register` hands the registry to all three
  services and claims the two job handlers on it, each bound to the module's
  own service instance (a new service or handler that Register stopped wiring
  fails here), and a real Create→Upload→Complete→OpenContent round trip runs
  through the kernel-resolved (real temp-dir) local store, not a fake;
- `repository_test.go` — cursor listing plus `tenancytest.AssertIsolated` for
  both repositories, the delete protocol's row primitives (`markDeleting`,
  `deleteObjectRows`, the state and expiry listings), and
  `finalizeUpload`'s write-time deadline and guarded transition — the fix the
  completion/sweep race needed, pinned from the repository side;
- `derive_test.go` — the thumbnail pipeline: the exact-area-average downscaler
  (dimension math, alpha-weighted averaging), JPEG and PNG re-encoding at the
  configured edge, idempotent re-runs, logged skips on nothing-to-derive,
  store-failure errors, the pixel-ceiling re-check, the
  object-disappears-mid-derive race (bytes dropped on re-read), and the
  handler's task shape and payload refusal;
- `cleanup_test.go` — the delete protocol and the sweep: a full
  create→upload→complete→derive→delete journey whose `storage.object.deleted`
  lands exactly once (a second delete converges silently, cross-tenant runs see
  nothing), the `storage.object_uploading` refusal, fail-closed behaviour with
  no store wired (the mark lets a later run finish), a mid-protocol store
  failure leaving the work resumable, warn-and-stand event publishing, the
  sweep's three phases (deterministic resumption order, silent upload
  reclamation, expired-completed deletion), fail-fast recovery on the next
  pass, and the expiry-sweep task's shape, tenant requirement and handler;
- `example_test.go` — three compiled-and-run godoc examples: the
  repository-level journey (`Example`), the full host-shaped transfer
  lifecycle (`ExampleObjectService`), and the end-of-life journey
  (`ExampleLifecycleService`), which deletes a real completed object and sweeps
  a stale upload — each over a real migration, a real kernel bootstrap, and a
  deterministically encoded PNG.

The migration SQL for both dialects ships under `migrations/{postgres,sqlite}`
and applies from zero on SQLite in the unit tier; no PostgreSQL/Redis integration
tier exists in this module yet (see below).

## Known limitations

- **The strip is structural, not a full decode.** Metadata smuggled into the
  entropy-coded scan data of an image — rather than into the segment/chunk
  carriers this walker knows — is out of scope for a structural strip, and the
  header-only pixel probe (`DecodeConfig`) cannot see corruption or smuggled
  metadata below the header. The module's own tests construct the carriers the
  walker knows; anything the walker cannot verify structurally is refused rather
  than passed through. Full-decode verification (and re-encode-based sanitizing)
  is the processing round's work, which is where thumbnails already force a
  full decode to happen.
- The cursor page composes the keyset query on the plugin-guarded `*gorm.DB`
  exactly as `go/dbkit/AGENTS.md`'s "Known limitations" option 1 prescribes,
  inside `dbkit.WithTenantSession`; this file never writes `tenant_id = ?` and
  never reaches for `db.Table` / `db.Model` / `db.Raw`.
- The completion event, the derive enqueue and the object-deleted publish all
  warn rather than fail their calls, by design; a host that needs delivery
  guarantees subscribes through the bus machinery those guarantees belong to.
- **The delete/derive race keeps one microsecond window open.** The derive
  worker drops its bytes when its re-read finds the object gone or deleting,
  and the delete protocol removes every derivative row its walk lists — but a
  derivative row inserted after that walk (its bytes written before the
  deletion began) can still land behind the deletion. The gap is deliberately
  not closed eagerly this round: the sweep treats such a ghost derivative as
  an object's ordinary row and removes it, and the bytes it names, with the
  next resumed run. derive.go's and cleanup.go's headers record the window so
  a later round can decide whether eager reconciliation is worth it.
- **Expiry is enforced by the sweep the host schedules, not by the module
  itself.** Nothing in the module runs a timer: expired uploads are reclaimed
  and expired objects deleted only when a host actually enqueues each tenant's
  expiry-sweep task (through `EnqueueExpirySweep`) or calls `Sweep` — retention
  is validated at create and enforced at sweep time, and a host that schedules
  no sweeps retains everything. The per-tenant idempotency key keeps the
  sweeps that do run from racing each other.

## Deferred and not shipped (with reasons)

- **HTTP surface.** `OpenAPISpec()` returns nil; there is no `api/` fragment, no
  handler. The module's surface is the Go API above; the spec-first HTTP round
  (routes in front of `ObjectService` and the deletion surface, object keys
  still never crossing the wire) is its own round because a fragment and its
  generated interface are locked by the api-contract pipeline.
- **Audit emission.** The three audit actions are declared; the services log
  their transitions (`object completed`, `object deleted`, `expired upload
  reclaimed`) but emit no audit rows. A host that needs rows now emits
  explicitly under the declared actions (the reference-app notes pattern);
  emission wiring is a later round of its own.
- **Eager ghost-derivative reconciliation.** The microsecond delete/derive
  window (see "Known limitations") is closed lazily by the sweep; an eager
  reconciliation after a delete would shrink the window at the cost of another
  protocol step and is deferred until a consumer reports the residue matters.
- **Upload-credential and short-lived-read-URL machinery.** No presigner exists
  and none is imported: uploads stream through `Upload` and reads through
  `OpenContent` inside the server, which suits the standalone and small-replica
  shapes. Direct-to-store client uploads with presigned credentials, and
  short-lived read URLs, are a later distributed-mode round and are deliberately
  not half-built here.
- **An integration tier.** The unit tier proves SQLite; the dual-dialect rule's
  PostgreSQL leg and a real store round trip through `pkgcore`'s S3
  implementation remain unproven in this module's own suites until a Docker
  integration tier lands, in the same shape the platform's other modules run
  under `full-ci`.
