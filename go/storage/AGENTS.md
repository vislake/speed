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
`LifecycleService`'s crash-convergent delete protocol and expiry sweep; the
HTTP surface (`api/openapi.yaml`, the generated `api.ServerInterface`,
`Handler`, mounted by `Register` at `/api/v1/storage`); the Docker-backed
`integration_test/` tier (a PostgreSQL leg and a RustFS/S3 leg, see
"Testing"); and the reference app wired as the module's mandatory first
consumer end to end (`examples/reference-app/internal/app/server.go`,
`flowtests/storage_flow_test.go`). See "Deferred and not shipped" for what
is deliberately absent. All gates run green: `go build ./...`,
`go vet ./...`, `golangci-lint run ./...`, `go test ./... -race` (from this
directory), `go test -race -tags=integration ./...` (Docker required for
the integration tier), plus the workspace
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

- `Create(ctx, CreateParams{DeclaredSize, DeclaredType, DeclaredChecksum, Retention, NoExpiry})`
  validates the declaration and reserves the row in `ObjectStateUploading`.
  Refusals happen here, before any byte transfers: non-positive size
  (`storage.invalid_size`), size above the module ceiling
  (`storage.object_too_large`, param `max_bytes`), malformed checksum
  (`storage.invalid_checksum`), a declared type outside the allowlist
  (`storage.type_not_allowed`), retention past the module maximum
  (`storage.invalid_expiry`), a never-expiring request on a module whose host
  did not opt in (`storage.no_expiry_not_allowed`), and a never-expiring
  request that also names a finite retention (`storage.invalid_expiry`). The
  declared type is canonicalized (parameters
  stripped, case folded) before storage; the declared checksum must be 64
  lowercase hex characters, refused otherwise so no two spellings of one digest
  can drift apart. A context without a tenant fails closed (`storage.internal_error`
  wrapping `pkgcore.ErrNoTenant`).
  **The row's expiry is settled at create**: a requested finite retention
  lands as that deadline; NO request — the ordinary upload — lands as an
  expiry at the module's configured maximum lifetime (`WithMaxObjectLifetime`,
  so an unconfigured upload is bounded by the host's ceiling, never silently
  permanent); and only an explicit `NoExpiry: true` on a module built with
  `WithNoExpiryAllowed()` leaves the row without an expiry (the `expires_at`
  NULL state the sweep skips). Never-expiring objects therefore require two
  deliberate acts — a host option and a per-object request — never an omission.
- `Upload(ctx, objectID, contentLength *int64, body)` streams the body into the
  host's store, bounded byte for byte by the declared size. The optional
  `contentLength` is the transport-observed length when one exists: a value that
  disagrees with the declaration is refused before any store write
  (`storage.content_length_mismatch`, param `declared`); nil means the byte count
  is reconciled alone. An upload of a row past its window or not in `uploading`
  state is refused (`storage.content_missing` / `storage.object_not_uploading`,
  param `id`). A short or oversize body is refused after the store write and the
  partial bytes are deleted best-effort (`storage.size_mismatch`); cleanup
  failure degrades to a warning log, never a silent success. A successful write
  that interleaves with the expiry sweep's reclaim of the same row — the window
  closes mid-stream, the sweep removes the bytes and the row — is caught by a
  post-write re-read: a reclaimed row or a closed window answers
  `storage.content_missing` and the write's bytes are taken back best-effort, so
  a reclaim never inherits a late write under the key it just emptied.
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
   against the probe when one was declared (`storage.type_mismatch`). The
   allowlist itself is bounded before any of this runs: Register's admission
   gate (`checkAdmittedMediaType`, validate.go's `mediaTypeSafety`) refuses a
   configured type the module cannot pixel-check AND metadata-strip, so an
   admitted type is a promise that steps 5 and 6 both apply to it.
5. Images decode their header only (`image.DecodeConfig` — header-only, no
   full decode) and their pixel count is checked against the module ceiling
   (`storage.pixel_limit_exceeded`, param `max_pixels`; an undecodable header is
   `storage.image_unreadable`).
6. The metadata strip (`sanitize.go`) removes location and authorship metadata
   before the object is readable, by carrier class: on JPEG, APP1 segments
   carrying EXIF (the container that holds GPS coordinates and camera
   authorship) or Adobe XMP, APP13 segments carrying Photoshop's
   image-resource block (IPTC-IIM, the By-line/Credit/Copyright and
   City/Country vocabulary), and COM comment segments (free text, no
   signature can classify it); on PNG, the `eXIf` chunk and the text-chunk
   family `tEXt`/`zTXt`/`iTXt` — `iTXt` being PNG's own carrier for the same
   XMP packet APP1 carries on JPEG. Stripping is **structural, not
   re-encoding**: decodable pixel
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
or of an image media type this service has no encoder for, or one deleted
between its row read and its byte read — is a logged skip, not an error: a
job that converges on nothing to do must complete cleanly, or the queue
would re-run it into a dead letter. The mid-run deletion is told apart from
a genuinely vanished completed row's bytes by re-reading the row at the
not-found byte answer (derive.go): a row that is gone or deleting cancels
the derive; only a row still completed keeps the store-error classification.
Genuine failures — store errors,
undecodable content, a source over the pixel ceiling (re-checked against the
stored bytes before the full decode, so a worker never decodes an image the
transfer pipeline already refused) — are errors, which is exactly what the
jobs layer's retry policy exists for.

The derive worker is the delete protocol's race partner, and the derivative
row's insert is where the two converge, as derive.go's and cleanup.go's
headers record: the insert is gated in one transaction on the object's own
row still existing and reading completed (repository.go's
`insertDerivativeIfAbsent`), so a delete that removed the object first wins
the gate — the insert is refused and the worker drops the bytes it just
wrote, best effort — while a delete that races the gate blocks on the
locked object row until the insert commits and its own row removal, object
row first and derivative rows last, then removes what just landed. No
window is left to close later.

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
`expires_at` NULL — an object that never expires — is skipped). One object's
failure does not stop the pass — the shape compliance's `SweepTenant` applies
to its own participants, adopted here because the sweep listings are
deterministic and a fail-fast pass would hit the same first refusing row on
every run, starving every row after it indefinitely: a permanently failing
object (a poisoned store key, say) must not block the rest of the tenant's
expiry work. Each failing row is logged with its id and left in the state the
next pass resumes — deleting rows stay deleting, expired rows stay as they
were — and the pass ends with `storage.sweep_partial_failure` (param
`failed_rows`) when any row failed, so a caller that checks only `err != nil`
still learns the pass was not clean.
Reclaiming an upload is safe against a completion racing it only because the
upload window is enforced at the finalize write itself, not at listing time:
`finalizeUpload` refuses a completion whose window closed mid-flight
(repository.go enforces the deadline at the write), so a row
this sweep listed can never complete behind its back — either the completion
committed before the window closed, in which case the row is completed and no
longer matches the reclaim listing, or the write is refused and the row is
reclaimed. The convergence runs the other way too: a transfer whose own store
write interleaves with the reclaim is caught on the transfer side, which
re-reads the row after the write and takes its bytes back when the row is
gone or the window has closed (see `Upload` above and `Complete`'s
lost-finalize branch in object.go) — so a reclaim never leaves a late
transfer write orphaned under the key it just emptied, and a transfer never
leaves bytes a reclaim already removed. Rows a concurrent sweep already
removed are nothing left to do, not errors.

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
| `WithMaxObjectLifetime` | 90 days | the default life of an upload that requests no retention AND the ceiling on requested finite retentions — an unconfigured upload expires at this ceiling, never silently; only an explicit never-expiring request may outlive it (see `WithNoExpiryAllowed`) |
| `WithNoExpiryAllowed` | off | permits never-expiring objects: without it a `CreateParams.NoExpiry` request is refused (`storage.no_expiry_not_allowed`); with it, each such object still needs its own explicit `NoExpiry: true` |
| `WithAllowedTypes` | image/jpeg, image/png | media-type allowlist; nil resolves to the module default. Register refuses (`storage.allowed_type_unsupported`) any configured type the module cannot pixel-check AND metadata-strip (validate.go's `mediaTypeSafety`) |

`Register(reg *pkgcore.Registry)` performs no I/O and:

- requires the queue `WithQueue` wired — a queueless Register fails with
  `storage.queue_required` before declaring anything;
- runs the allowlist admission gate — every configured type (the module
  default when none was configured) must be one the module can pixel-check
  AND metadata-strip (`checkAdmittedMediaType`, validate.go), a misconfiguration
  failing with `storage.allowed_type_unsupported` before anything is declared.
  The gate makes the whitelist's safety promise a checked one: a type can no
  longer be admitted with a decoder but no strip coverage (image/gif's state —
  see `mediaTypeSafety`), and the three facts — which types have a decoder,
  which have a strip walker, what may be admitted — live in the one table;
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
  thumbnails and sweeps expiry;
- builds `Handler` (`handler.go`) and mounts the module's HTTP surface on
  `reg.Routes` at `apiPath` (`/api/v1/storage`, agreed with the fragment's
  `paths:` keys so the host's outer mux knows which requests to hand over).
  `Handler` is built here, not in `NewModule`, so it serves the service and
  repository instances the host's `With*` options actually configured —
  the same `m.svc`, `m.life`, `m.objects` and `m.derivatives` the job
  handlers above are bound to. `Routes.Mount` is a plain registration, no
  I/O, so Register's no-I/O contract stands.

### The HTTP surface — `handler.go`, `api/`

`api/openapi.yaml` is the module's OpenAPI fragment, one of the eleven
platform-module fragments the merged document carries (org, storage,
notification, sharing, pki, admin, integration, ai-gateway, billing,
authn and config; the reference app's own notes, cases and smilesim fragments are
the app's own API, not merge members): paths all `/api/v1/storage/...`,
operationIds `storage_<action><Resource>`, schemas `Storage<Type>`, tag
`storage`, **no `tenant_id` anywhere on the surface** (the tenant comes from
the context `tenancy.Middleware` resolved before the handler runs, per root
CLAUDE.md's isolation rule). Seven operations define the whole surface: the
three-step upload lifecycle (`storage_createObject`,
`storage_uploadObjectContent`, `storage_completeObject`), object reads
(`storage_listObjects`, `storage_getObject`, `storage_getObjectContent`)
and object deletion (`storage_deleteObject`). `api/oapi-codegen.yaml` pins
the same generator version notes and org use (v2.8.0);
`api/storage-server.gen.go` is generated and committed, never hand-edited —
`task api:gen` regenerates it from the fragment and api-contract.yml's own
diff gate re-checks it on every spec-touching PR. The fragment joins the merged `contracts/speed.yaml` document and
through it the generated `@speed/api-sdk` -- the module-driven inclusion
policy: every platform module with an HTTP fragment is a merge member.
The fragment is embedded
(`//go:embed`), so the spec and the generated types travel inside the
module binary (`OpenAPISpec()`); object keys still never cross the wire —
consumers name objects by id, exactly as this file's key-grammar section
promises.

`Handler` implements the generated `api.ServerInterface` behind the
compile-time assertion at the bottom of handler.go — "spec changed, handler
not" is a compile failure, never a runtime surprise — and performs no data
access of its own: it drives the same services and repositories Register
attached, and only the two lookups no service method expresses (the delete
pre-read the HTTP surface needs to promise 404 where the service converges
on success, and the derivative listing `storage_getObject` carries). Two
error codes exist for the surface only: `storage.invalid_request_body`
(malformed JSON on any operation) and `storage.invalid_limit` (list page
size outside the 1-200 window the spec documents and the handler enforces).

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
  `deleteObjectRows`, the state and expiry listings),
  `finalizeUpload`'s write-time deadline and guarded transition — the fix the
  completion/sweep race needed, pinned from the repository side — and
  `insertDerivativeIfAbsent`'s object-state gate (a refused insert lands no
  row for an object that is gone, deleting or uploading — the close of the
  delete/derive race, pinned from the repository side);
- `derive_test.go` — the thumbnail pipeline: the exact-area-average downscaler
  (dimension math, alpha-weighted averaging), JPEG and PNG re-encoding at the
  configured edge, idempotent re-runs, logged skips on nothing-to-derive,
  store-failure errors, the pixel-ceiling re-check, the
  object-disappears-mid-derive race (the insert gate's refusal drops the
  just-written bytes; the byte-read side — a deletion landing between the
  row read and the byte read — is re-checked against the row and cancels the
  derive cleanly, while a still-completed row whose bytes are gone keeps its
  store_error), and the handler's task shape and payload refusal;
- `cleanup_test.go` — the delete protocol and the sweep: a full
  create→upload→complete→derive→delete journey whose `storage.object.deleted`
  lands exactly once (a second delete converges silently, cross-tenant runs see
  nothing), the `storage.object_uploading` refusal, fail-closed behaviour with
  no store wired (the mark lets a later run finish), a mid-protocol store
  failure leaving the work resumable, warn-and-stand event publishing, the
  sweep's three phases (deterministic resumption order, silent upload
  reclamation, expired-completed deletion), the partial-failure contract —
  one row's refusal never starves the rows after it, the failed row is left
  resumable and the pass answers `storage.sweep_partial_failure` — and the
  expiry-sweep task's shape, tenant requirement and handler;
- `example_test.go` — three compiled-and-run godoc examples: the
  repository-level journey (`Example`), the full host-shaped transfer
  lifecycle (`ExampleObjectService`), and the end-of-life journey
  (`ExampleLifecycleService`), which deletes a real completed object and sweeps
  a stale upload — each over a real migration, a real kernel bootstrap, and a
  deterministically encoded PNG.

The migration SQL for both dialects ships under
`migrations/{postgres,sqlite}` and applies from zero on SQLite in the unit
tier. A Docker-backed integration tier (`integration_test/`, built with
`//go:build integration` so a plain unit run never touches it) re-proves on
real infrastructure what a SQLite-only suite cannot, run as
`go test -race -tags=integration ./...` from this directory — the exact
invocation full-check.yml's integration-tiers job runs for this module:

- `postgres_leg_test.go` — the module's postgres/*.sql migration files
  apply from zero against a real PostgreSQL server (testutil.NewPostgres
  through dbkit's dbtest helper, skipping when no Docker daemon is
  reachable), and `tenancytest.AssertIsolated` re-runs over both
  tenant-scoped repositories (objects, object_derivatives) on the second
  dialect, with fixtures filling every NOT NULL column exactly as the unit
  tier's do;
- `rustfs_leg_test.go` — one object's full lifecycle driven against a real
  RustFS (https://github.com/rustfs/rustfs) server through
  `s3.NewObjectStore` (`go/pkgcore/objectstore/s3`), the implementation the
  distributed deployment mode composes: every assertion on the store's
  physical contents is made through a raw minio-go client (the client
  library stays minio-go regardless of which S3-compatible server backs
  it), never through the module's own read paths, so nothing the module
  believes about its writes goes unchecked. The composition is a
  standalone-mode kernel whose ObjectStore the host overrides via
  `WithObjectStore` — the injectable seam a distributed-mode host wires,
  exercised against real RustFS.

## Known limitations

- **The strip is structural, not a full decode, and it classifies carriers,
  never payload bytes.** On the admitted types it covers the standardized
  carriers of location and authorship metadata — on JPEG, EXIF and XMP
  (APP1), IPTC-IIM inside Photoshop's image-resource block (APP13), and COM
  free-text comments; on PNG, `eXIf` and the text-chunk family
  `tEXt`/`zTXt`/`iTXt` (iTXt the XMP carrier). A segment or chunk that
  carries no recognized vocabulary rides through untouched, exactly as
  metadata smuggled into the entropy-coded scan data does — the walker can
  verify container structure, never content semantics, and unclassifiable
  payload is out of a structural walker's reach. The header-only pixel probe
  (`DecodeConfig`) cannot see corruption or smuggled metadata below the
  header. The module's own tests construct real carriers for every covered
  class; anything the walker cannot verify structurally is refused rather
  than passed through. Full-decode verification and re-encode-based
  sanitizing are not shipped; the thumbnail pipeline is where a full decode
  already happens.
- The cursor page composes the keyset query on the plugin-guarded `*gorm.DB`
  exactly as the dbkit layering rule's option 1 prescribes, inside
  `dbkit.WithTenantSession`; this file never writes `tenant_id = ?` and never
  reaches for `db.Table` / `db.Model` / `db.Raw`.
- The completion event, the derive enqueue and the object-deleted publish all
  warn rather than fail their calls, by design; a host that needs delivery
  guarantees subscribes through the bus machinery those guarantees belong to.
- **Expiry is enforced by the sweep the host schedules, not by the module
  itself.** Nothing in the module runs a timer: expired uploads are reclaimed
  and expired objects deleted only when a host actually enqueues each tenant's
  expiry-sweep task (through `EnqueueExpirySweep`) or calls `Sweep` — retention
  is validated at create and enforced at sweep time, and a host that schedules
  no sweeps retains everything. The window-scoped idempotency key
  (`expirySweepIdempotencyKey`, one `expirySweepWindowSize` window per key)
  keeps one window's concurrent enqueues from racing each other — the sweeps
  that do run never duplicate within a window — while later windows' enqueues
  schedule the sweep again.
- **The sweep keys are window-scoped.** The app's host-side periodic-task
  scheduler (`examples/reference-app/internal/app/periodic_scheduler.go`)
  enqueues one sweep per unique host tenant every tick — the cadence
  `cfg.PeriodicTaskInterval` (one minute by default,
  `defaultPeriodicTaskSchedulerInterval`), the tenants the values of the
  host's own `cfg.HostTenants` map, deduplicated, each sweep enqueued under
  the tenant's own `pkgcore` context because the sweep handler runs
  tenant-scoped — started and stopped with the queue worker in
  `internal/app/server.go`'s `BuildServer` (the same `cfg.DisableQueueWorker`
  gate). `EnqueueExpirySweep` derives its idempotency key from the
  `expirySweepWindowSize` window the enqueue falls in
  (`expirySweepIdempotencyKey`/`expirySweepWindowStart`, cleanup.go), not
  from the tenant alone: on the host's `StandaloneQueue`, whose resolved
  idempotency keys are held forever (go/jobs), the ticks inside one window
  still merge into the window's one job — the concurrency protection the
  key exists for — but the first tick of every later window resolves a
  fresh key and schedules the sweep again, at most one sweep per tenant
  per window. A sweep job that dead-letters therefore poisons only its own
  window; the next window's tick is a new job. The end-to-end deletion
  proof — two boots over one database file and one object-store directory,
  boot 1 hosting a completed object whose retention deadline passes plus a
  no-deadline survivor with the worker-and-scheduler pair disabled (expiry
  alone removes nothing), boot 2 starting the normal gate so the sweep its
  first tick enqueues removes the expired object's row and bytes — is
  `TestBuildServer_PeriodicScheduler_ExpirySweep_RemovesExpiredObject`
  (`examples/reference-app/flowtests/periodic_scheduler_flow_test.go`),
  observed through the host's HTTP surface, a second-connection repository
  read and the filesystem, with the survivor untouched. That proof holds
  unchanged under the windowed key (boot 2's first tick is in a fresh
  window), and the windowed keying is pinned by this module's own
  `sweep_window_test.go` (same-window collapse, later-window re-run,
  dead-lettered-window non-poisoning, each against a real
  `jobs.StandaloneQueue`). What remains genuinely limited: an object whose
  retention deadline passes is reaped at most once per
  `expirySweepWindowSize` per tenant — a host that needs tighter reaping
  bounds must enqueue more often or shrink the window constant — and, as
  always, a host that schedules no sweeps retains everything.
- **Upload and Complete serialize per object only inside one process.** The
  service's `objectLocks` (object.go) keep a completed row's finalized
  metadata honest within a single process: a second Upload of the same object
  can no longer land between Complete's read of the bytes and its finalize,
  which is the interleaving that would leave a completed row describing bytes
  the key no longer holds. The lock map is process-local, though, so two
  replicas of a distributed deployment — which share the ObjectStore but not
  the map — can still interleave an Upload on one replica with a Complete on
  another. Closing that residue would need a store-level compare-and-swap
  the ObjectStore seam does not carry; the standalone shape (one process,
  one store) is airtight and the multi-replica one is not — recorded here
  rather than pretended away.
- **A lost finalize's writeback take-back is one-shot best-effort.**
  When `Complete`'s finalize commits zero rows and the re-read finds the
  row reclaimed, its upload window closed, or deleting, a sanitizer
  writeback (`changed`) is taken back with one best-effort `DeleteObject`
  call (object.go's lost-finalize branch); a failure is warned about,
  never retried -- the call is
  answering the transfer pipeline's caller, not running a protocol it owns.
  A re-read that itself fails is narrower: only its not-found
  shape -- the row genuinely vanished -- triggers the take-back, and every
  other failure is reported unchanged with nothing removed, because the
  row's state is unknown and may be exactly the live completed row a
  concurrent completion just won, whose bytes deleting on a guessed shape
  would empty (the anomaly every later read reports as `store_error`); the
  rule is derive.go's own "a re-read that itself fails leaves the question
  unanswered". The delete protocol's own byte removal has different retry
  semantics on purpose: a failure there keeps the row `deleting` and the
  sweep's first phase re-runs the protocol until the removal lands. The
  take-back leaves no such trace, and in two of its three shapes a failed
  take-back can be a permanent residue: against a vanished row -- the
  reclaim already removed the row, so nothing remains to re-run -- or
  against a deleting row whose own delete run then finishes, removing the
  row the take-back's failure could have been retried on. (A deleting row
  whose protocol is itself interrupted is a different story: its rows stay
  for the sweep, whose resumed run re-removes the key before the rows, so
  that interleaving converges.) A permanent residue leaves the writeback's
  bytes orphaned under the key: no row references them, no reader can reach
  them (reads serve completed rows, and the row is gone or doomed), and
  nothing will ever reclaim them -- the module never lists keys (the
  `ObjectStore` seam has no listing) and object ids are never reused, so the
  key can neither be claimed by a future object nor cleaned by a row-driven
  sweep. A non-not-found re-read failure coinciding with the row's removal
  leaves the same residue by the same route: the take-back is skipped, and
  the writeback sits under a key nothing will revisit. The window-closed
  shape is bounded: the row stays listed as an expired upload, so the next
  sweep's reclaim of it re-deletes the key. The residue is reachable only
  across the replicas of a distributed deployment (within one process the
  per-object lock serializes completions and no actor flips an uploading
  row to deleting). Closing it would need machinery that revisits keys,
  which is not shipped: a key-reaping sweep over the store's key space, or
  key removal folded into the delete protocol's own convergence at a point
  guaranteed to follow any writeback (closing the deleting shape; the
  vanished shape's row is already gone when its writeback lands, so only a
  key sweep reaches it). Recorded here rather than pretended away.
- **A failed finalize's writeback rollback is one-shot best-effort.**
  When `Complete`'s finalize write errors outright -- as
  opposed to committing zero rows, the lost-finalize shapes above -- the
  pipeline rolls its sanitizer writeback back by restoring the pre-rewrite
  bytes it still holds (object.go's finalize-err branch): the row almost
  always still stands uploading, carrying the declared size of the original
  upload, and only the restore keeps the caller's own retry -- which
  re-probes, re-sanitizes (deterministically) and re-finalizes over the
  restored bytes -- from refusing with `storage.size_mismatch`, a
  server-side two-store divergence reported as a client data problem. The
  rollback is one best-effort `PutObject`; a failure is warned about, never
  retried, and leaves that divergence as residue: the sanitized short bytes
  stay under a key whose uploading row names the longer originals, the
  retry refuses with `size_mismatch`, and the upload stays uncompletable
  until its window closes and the expiry sweep reclaims the row and its
  bytes together. The rollback can also land after a concurrent completion
  on another replica won the transition while this finalize was failing --
  the interleaving where the row is completed by the winner with bytes
  identical to this writeback (sanitize is deterministic over the same
  generation), and the restore then overwrites them with the unsanitized
  originals, a live completed row whose stored bytes contradict its own
  metadata. That residue needs the same narrow coincidence as the
  take-back's own: a genuine database failure at the exact moment another
  replica completes the same object (a lost race answers zero rows, not an
  error -- PostgreSQL row locks serialize the two conditional writes), so
  it is reachable only across the replicas of a distributed deployment.
  Within one process the per-object lock serializes completions, and the
  only other actor that can flip an uploading row is the expiry sweep,
  which touches only rows whose window has closed -- and a finalize whose
  window closed at write time answers zero rows (a lost finalize), not an
  error -- so the err shape's row is the untouched uploading one except
  when the window closed under the pipeline in the same instant the write
  itself failed; the restore then lands inside the sweep's own reclaim
  race, bounded or converged exactly like the writeback's (a still-listed
  window-closed row is reclaimed by the next sweep, bytes and all; a
  restore landing between the reclaim's byte removal and its row removal
  leaves the orphan-keys residue above). Recorded here rather than
  pretended away.

## Deferred and not shipped (with reasons)

- **Audit emission is not wired.** The three audit actions are declared;
  the services log their transitions (`object completed`, `object deleted`,
  `expired upload reclaimed`) but emit no audit rows. A host that needs
  rows emits explicitly under the declared actions (the reference-app
  notes pattern).
- **No upload-credential or short-lived-read-URL machinery.** No presigner
  exists and none is imported: uploads stream through `Upload` and reads
  through `OpenContent` inside the server, which suits the standalone and
  small-replica shapes. Direct-to-store client uploads with presigned
  credentials, and short-lived read URLs, are not shipped.

## JPEG EOI strictness

`sanitizeJPEG` walks every scan to its terminating marker — byte-stuffed
0xFF 0x00 pairs, restart markers 0xFFD0-0xFFD7 and 0xFF fill are scan
content, walked past, never parsed — and dispatches that marker like any
other, so the scans of a progressive or hierarchical JPEG are each walked
and only the file's final EOI ends the walk. A JPEG whose entropy-coded
data never terminates in EOI is refused as a structure error (the PNG
path's required-IEND doctrine, mirrored); anything appended after a real
EOI (a second EXIF/XMP APP1, arbitrary bytes) is dropped at the boundary,
mirroring the PNG walker's post-IEND rule, except a tail of pure 0xFF
fill, which is conventionally legal padding and is carried over so a
padded clean file passes through byte-identical with nothing written
back. An EXIF/XMP APP1 sitting between scans of a progressive file is
stripped like any other marker. What remains invisible to a structural
strip is unchanged and still recorded in "Known limitations": metadata
smuggled into the entropy-coded data itself.

## Lifecycle default, sweep partial failures, whitelist admission gate, key shape

- **Every row's expiry is settled at create.** `WithMaxObjectLifetime`
  promises "the longest an object may be retained before it expires", and
  `Create` settles every row's expiry against it: a requested finite
  retention lands as that deadline (capped by the ceiling), NO request
  defaults to the configured maximum, and only an explicit
  `CreateParams.NoExpiry` request — on a module whose host opted in with
  `WithNoExpiryAllowed()` — leaves the row without an expiry. Never-expiring
  objects therefore remain representable (the `expires_at` NULL state the
  sweep skips) but require two deliberate acts, a host option and a
  per-object request; omission produces bounded life, never permanence. The
  module's own HTTP surface offers no never-expiring spelling (the
  fragment's create description says so); a host whose product needs
  permanent objects requests them through `ObjectService` behind its own
  opt-in.
- **The sweep does not fail fast; one row's failure cannot starve the
  tenant's expiry pass.** `Sweep` runs every row of every phase, logs each
  failure with its id, and answers `storage.sweep_partial_failure` (param
  `failed_rows`) when any row failed; failed rows stay in the state the
  next pass resumes. Fail-fast would be wrong here: the sweep's listings
  are deterministic, so a row that fails permanently is re-listed first on
  every pass, and a fail-fast sweep would never reach the rows after it —
  the same row-granularity shape compliance's `SweepTenant` uses when it
  runs every participant and aggregates the failures.
- **The whitelist admission gate makes the safety envelope a checked
  invariant.** The three facts — which probed types have a decoder, which
  have metadata-strip coverage, what the whitelist may admit — live in
  validate.go's `mediaTypeSafety` table, and Register refuses
  (`storage.allowed_type_unsupported`, naming the type and the admissible
  set) any configured type the module cannot pixel-check AND
  metadata-strip. image/gif is recorded with its honest halves (decodable,
  not strippable) and is therefore refused admission — admitting GIFs
  requires a GIF strip walker first, the change that would flip the table's
  strippable half. The content endpoint's two hardening headers —
  `X-Content-Type-Options: nosniff` and `Content-Disposition: attachment`
  — are unconditional, so serving bytes is safe regardless of how wide the
  whitelist ever grows.
- **The reclaim's load-bearing dependency is recorded at the code.**
  `reclaimUpload`'s no-event shortcut and its licence to remove a row
  family outside the delete protocol rest on there being no path that
  returns a completed row (which may carry derivatives) to uploading. The
  day a re-upload or back-to-uploading transition appears, the reclaim
  would delete a completed object's family with no protocol and no event —
  that hazard is written down beside the reclaim code.
- **The key grammar's fixed shapes are enforced by segment count.** The
  validator checks every segment and how many there are: a tenant id or
  object id that smuggled in a "/" would otherwise pass every per-segment
  rule while fabricating segments and blurring the boundary between key
  components and key families. uuid-shaped ids cannot contain a "/", so no
  reachable input trips the count — the checks in `ObjectKey` (exactly
  three segments) and `DerivativeKey` (exactly four) exist so the day an
  id alphabet changes, the violation is a loud builder error at the single
  create/derive site, never a silently reshaped key.

## Carrier-class metadata strip: APP13 IRB/IPTC, COM, the PNG text family

The strip's rule enumerates two content classes — location and authorship
metadata — and the walkers drop every carrier family that can hold either
class on an admitted type, wholesale. On JPEG: EXIF and XMP (APP1, by
payload signature), IPTC-IIM (APP13 whose payload is a Photoshop
image-resource block, dropped whole so the strip never depends on the
IRB's internal resource layout), and COM — free text has no signature that
could classify it, so the whole marker is the carrier and every comment
segment goes. On PNG: `eXIf` plus the tEXt family (`tEXt`/`zTXt`/`iTXt`),
iTXt being the PNG standard's own carrier for the same Adobe XMP packet
`eXIf` carries (under the `XML:com.adobe.xmp` keyword), the other two
keyworded free text no structure check can classify. A Lightroom/Photoshop
JPEG export commonly carries APP13 IPTC and a PS-saved PNG carries iTXt
XMP; both pass strict structural validation (correct lengths, CRCs
included) while carrying GPS and bylines into the object the platform
serves — through `Complete`'s irreversible write-back and, in the
reference app, onto sharing's unauthenticated access route — so the
carrier families are dropped, never passed through.

The strip classifies carriers, never payload bytes — the honest boundary,
stated in sanitize.go's scope note and in the Known limitations list
above: an APP segment or ancillary chunk carrying no recognized
vocabulary rides through, exactly as entropy-smuggled bytes do. The
admission gate's promise is reconciled to that same boundary: errors.go's
"an admitted type is a promise" wording and validate.go's `strippable`
measure against the carrier classes the walkers actually enumerate, never
against byte purity no structural walk could deliver. ICC APP2 stays kept
(decoders need it for correct color), as do APP0 and other vendor APP
segments — pinned by the keep-tests and the non-IRB APP13 and tIME
boundary pins. Regressions build real carriers — an IRB-with-IPTC APP13
holding IIM By-line/City/Copyright data sets, COM comments, an iTXt XMP
packet with exif GPS and dc:creator content, tEXt Author/Location pairs
and a zlib-compressed zTXt — and assert the marker content itself is
absent from the stripped bytes, not merely that a rewrite happened; every
strip stays idempotent and decode-equal to the pristine base.
