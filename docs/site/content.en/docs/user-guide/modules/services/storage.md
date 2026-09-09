---
title: storage
description: "Media-object storage: metadata in tenant tables, bytes in your ObjectStore, and a three-step upload protocol whose completion revalidates what actually arrived."
weight: 1
---

# storage

storage is speed's media-object module: the metadata describing one
tenant's stored objects lives in the database, the bytes live in the
kernel-resolved `ObjectStore`, and a three-step upload protocol moves
them in with server-side revalidation.

## What it is for

The module owns **metadata, not bytes**. Bytes sit in your
`ObjectStore` (local directory or S3 — the module never knows which)
at keys the module derives from tenant and object id and never
exposes: consumers name objects by id. Two tenant-scoped tables
(`objects`, `object_derivatives`) carry the lifecycle story.
`NewModule` builds three services:

- `ObjectService` — the transfer lifecycle (`Create`, `Upload`,
  `Complete`) and the read surface (`Get`, `OpenContent`, `List`);
- `DeriveService` — thumbnail derivation from a completed image;
- `LifecycleService` — deletion and the expiry sweep (`Delete`,
  `Sweep`, `EnqueueExpirySweep`).

Uploads run the three-step protocol. `Create` validates your
declaration — size, media type, optional SHA-256 checksum, optional
retention — and opens an upload window (the row is `uploading`).
`Upload` streams the body into the store, bounded byte for byte by the
declared size. `Complete` revalidates what actually arrived before the
row becomes readable: stored size and checksum reconciled with the
declaration, MIME probed from content magic bytes and checked against
the allowlist, a header-only pixel check, and a structural metadata
strip (JPEG EXIF/XMP/IPTC/COM, PNG `eXIf` and text chunks) — a file
the walker cannot verify structurally is refused, never passed
through. Only then does the row advance to `completed`, the
thumbnail-derive task get enqueued and `storage.object.completed` be
published.

Deletion is a crash-convergent protocol: mark `deleting`, remove the
object's bytes and each derivative's, then remove the rows in one
transaction — an interruption is converged by the next run, never
duplicated. Reads serve `completed` objects only.

What it is **not**: not a general blob store — an admitted media type
must be one the module can pixel-check *and* metadata-strip (the
register-time admission gate refuses anything else); not a
presigned-upload service — uploads stream through `Upload` inside
your server; and not a retention timer — expiry is validated at create
and enforced by the per-tenant sweep your host schedules
(`EnqueueExpirySweep`, task `storage.expiry_sweep`), never by a
module-owned timer.

## When to choose it

Your product accepts user-uploaded media — images first — that must be
stored outside the database, served back with location and authorship
metadata removed, and cleaned up when retention passes. If you need
thumbnails, the derive pipeline ships. Documents of arbitrary type,
or browser-to-store uploads without a server hop, do not fit yet.

## Wiring it in

```go
m := storage.NewModule(db,
    storage.WithQueue(queue),           // a jobs.Queue — Register refuses without one
    storage.WithMaxUploadBytes(10<<20), // optional: per-object ceiling
)
// hand m to Kernel.Bootstrap's module set; the ObjectStore and
// EventBus come from the resolved registry, read per call.

svc := m.ObjectService()
created, err := svc.Create(ctx, storage.CreateParams{
    DeclaredSize:      size,          // transport-observed byte length
    DeclaredType:      "image/jpeg",  // allowlisted; "" declares no belief
    DeclaredChecksum:  hexSHA256,     // optional: 64 lowercase hex chars
})
// handle err — refusals happen here, before any bytes move
err = svc.Upload(ctx, created.ID, &size, body)
finalized, err := svc.Complete(ctx, created.ID)
```

`DeriveService` (`DeriveThumbnail`) and `LifecycleService` (`Delete`,
`Sweep`, `EnqueueExpirySweep`) are the other two faces; the expiry
sweep is per tenant, so a host schedules one task per tenant on its
own cadence.

## Core concepts and API surface

- **The expiry is settled at create.** A requested finite retention
  lands as that deadline (capped by the host's
  `WithMaxObjectLifetime` ceiling, 90 days by default); no request —
  the ordinary upload — lands as an expiry at that same ceiling; only
  an explicit `NoExpiry: true` on a module built with
  `WithNoExpiryAllowed()` leaves a row without an expiry. Permanence
  requires two deliberate acts, never an omission.
- **State and window gates.** A row past its upload window or not
  `uploading` refuses further writes; a completion whose window closed
  mid-flight loses the finalize; the sweep and a concurrent transfer
  cannot leave orphaned or double-owned bytes.
- **Options** (each with a named default): `WithMaxUploadBytes`
  (100 MiB), `WithMaxImagePixels` (40 000 000), `WithDerivativeMaxEdge`
  (320 px), `WithUploadTTL` (30 min), `WithMaxObjectLifetime`
  (90 days), `WithNoExpiryAllowed` (off), `WithAllowedTypes`
  (image/jpeg, image/png).
- **HTTP surface.** Seven operations under `/api/v1/storage` —
  `storage_createObject`, `storage_uploadObjectContent`,
  `storage_completeObject`, `storage_listObjects`,
  `storage_getObject`, `storage_getObjectContent`,
  `storage_deleteObject`; no `tenant_id` on the wire. The module
  declares permissions `storage:read`/`storage:write`, audit actions
  `storage.object.*` (declared, not emitted) and the two
  completion/deletion events.

## Limitations and links

- The metadata strip classifies **carriers, never payload bytes**:
  metadata smuggled into entropy-coded image data rides through, and
  the header-only pixel probe cannot see below the header.
  Full-decode re-encoding is not shipped.
- Upload and `Complete` serialize per object **within one process**
  only; across replicas of a distributed deployment the residual races
  are recorded in the module's `AGENTS.md`, not hidden.
- A host that schedules no sweeps retains everything — the module runs
  no timer of its own, and a sweep never fails fast
  (`storage.sweep_partial_failure` names the failed rows).
- Side-effect publishes (completion event, derive enqueue, delete
  event) warn rather than fail their calls.
- Coded errors: see the [error code
  index](../../../error-codes/#storage) — `storage.type_not_allowed`,
  `storage.size_mismatch`, `storage.object_not_found`,
  `storage.store_unavailable` and the rest.

### Source

- [go/storage/AGENTS.md](https://github.com/vislake/speed/blob/main/go/storage/AGENTS.md) — the authoritative document (lifecycle, revalidation pipeline, key grammar, known limitations, deferred list)
- Design rationale: [docs/internal/07-platform-services.md](https://github.com/vislake/speed/blob/main/docs/internal/07-platform-services.md)
- Related pages: [Platform services](../), [notification](../notification/), the domain guide [Storage, sharing and AI](../../../domains/storage-sharing-and-ai/)
