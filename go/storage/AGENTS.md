# storage

go/storage is the platform's media-object module: the metadata that describes one
tenant's stored objects, the internal keys their bytes live under, and the transfer
lifecycle that moves those bytes into and out of the host's object store with
server-side revalidation. It sits on the `dbkit` / `jobs` tier of the dependency
graph and is consumed by modules above it that handle media (ai-gateway, sharing).

**Status: implemented and tested** — the metadata model, both repository types, the
key grammar, the migration sets for both dialects, the bilingual locale bundles,
the module wiring, and the full `ObjectService` transfer lifecycle with its
revalidation pipeline (see "Deferred and not shipped" for what is deliberately
absent). All gates run green: `go build ./...`, `go vet ./...`,
`golangci-lint run ./...`, `go test ./... -race` (from this directory), plus the
workspace `go build github.com/vislake/speed/go/...` form.

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
- `deleting` — a delete protocol in flight (defined by the state machine; the
  protocol itself is a later round — see "Deferred and not shipped").

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

## Module wiring

`NewModule(db *gorm.DB, opts ...Option)` builds the repositories and the service.
The `With*` options override named package defaults — never magic numbers:

| Option | Default | Meaning |
|---|---|---|
| `WithMaxUploadBytes` | 100 MiB | single-object ceiling, enforced at create and by the bounded upload |
| `WithMaxImagePixels` | 40 000 000 | image pixel ceiling, enforced at complete |
| `WithDerivativeMaxEdge` | 320 px | longer-edge cap for generated derivatives (a later round) |
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
  `storage.object.deleted` (payload `storage.ObjectDeleted`: `object_id`;
  declared up front so the catalog is complete for subscribers, published by the
  deletion round);
- attaches the registry to the service (`attach`) — a plain assignment.

Audit actions are declared but **not emitted** by the service in this round; the
reference-app pattern of explicit `audit.Emit` calls under already-declared
actions is the standing route for hosts that need rows now (see "Deferred and
not shipped").

Error codes live in `errors.go` as `*apperr.Error` vars, each with its status
class, a canonical `storage.*` code, and bilingual `zh-CN` / `en-US` message
entries in `locales/` (identical key sets, pinned by `errors_test.go`). Frontends
map codes to text; no localized string crosses an API.

## Testing

Unit files map 1:1 onto their sources (`object.go` → `object_test.go`,
`validate.go` → `validate_test.go`, ...); shared helpers live in
`internal/testutil` (a migrated in-memory SQLite connection, deterministic JPEG
and PNG fixtures with EXIF/XMP/eXIf carriers). Highlights, all in the plain unit
suite under `-race`:

- `object_test.go` — the 24-function lifecycle matrix: create validation and
  canonicalization, state and upload-window gates, exact-byte streaming,
  content-length and size reconciliations, checksum/type/pixel refusals, JPEG and
  PNG strip rewrites whose finalized digest describes the stored bytes, refusal
  of undecodable images, side-effect failures that do not fail the finalize, the
  completion event and derive enqueue, completed-rows-only reads, and
  fail-closed behavior without an attached host;
- `module_test.go` — the register-time wiring proof: after a real standalone
  `pkgcore.Kernel.Bootstrap`, `Module.Register` hands the service the registry,
  and a real Create→Upload→Complete→OpenContent round trip runs through the
  kernel-resolved (real temp-dir) local store, not a fake;
- `repository_test.go` — cursor listing plus `tenancytest.AssertIsolated` for
  both repositories;
- `example_test.go` — two compiled-and-run godoc examples: the repository-level
  journey (`Example`) and the full host-shaped transfer lifecycle
  (`ExampleObjectService`), the latter a real migration, real kernel bootstrap,
  and a deterministically encoded PNG.

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
- The completion event and the derive enqueue warn rather than fail the call, by
  design; a host that needs delivery guarantees subscribes through the bus
  machinery those guarantees belong to.

## Deferred and not shipped (with reasons)

- **HTTP surface.** `OpenAPISpec()` returns nil; there is no `api/` fragment, no
  handler. The module's surface is the Go API above; the spec-first HTTP round
  (routes in front of `ObjectService`, object keys still never crossing the
  wire) is its own round because a fragment and its generated interface are
  locked by the api-contract pipeline.
- **Delete and expiry reclaim.** `ObjectStateDeleting` is defined but
  unreachable, `storage.object.deleted` is declared but unpublished, and no
  sweeper reclaims rows stuck in `uploading` past their window or completed
  objects past their retention — retention is validated at create, not enforced.
  Delete + sweep is one round, with `storage.object.delete` and the deleted
  event already declared for it.
- **Thumbnail derivation.** Completion enqueues the derive task
  (`storage.object.derive.thumbnail`, payload `{"object_id": ...}`, idempotency
  key `storage.thumbnail:<object_id>`) but no worker is registered to claim it;
  the queue is required at Register precisely so a later round's handler can be
  added without a wiring break.
- **Audit emission.** The three audit actions are declared; the service does not
  emit rows. Emission wiring lands with the deletion/complete rounds or a host
  emits explicitly under the declared actions (the reference-app notes pattern).
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
