package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is the module's transfer runtime. The ObjectService below
// drives one object through the lifecycle the model.go state machine
// describes -- a create declares the object and opens its upload window,
// an upload streams the bytes into the host's ObjectStore, a complete runs
// the revalidation pipeline over them and finalizes the row, and reads are
// served from completed objects only.
//
// The revalidation pipeline itself lives in validate.go and sanitize.go;
// ObjectService is their caller, the single place the checks run in their
// settled order and the single place a row's state advances. The host's
// infrastructure arrives through two seams, never by import: bytes go in
// and out of the ObjectStore the Registry resolves (local directory or S3
// -- the module never knows which), and the completion event goes out on
// the Registry's event bus. The queue the completion pipeline enqueues
// thumbnail derivation on is the same jobs.Queue Module.Register requires
// (WithQueue).
//
// What this file deliberately does not do: mount an HTTP surface (that is
// Handler's, built in handler.go from the api/ fragment and mounted by
// Register; the methods here are its service half), derive thumbnails
// (enqueued here, claimed by DeriveService in derive.go), or end object
// life (deletion and expiry sweeping live in LifecycleService in
// cleanup.go). This file stops at the transfer lifecycle and says so.

// ObjectService drives uploads through their lifecycle: Create validates a
// declaration and reserves the object's id and store key, Upload streams
// the bytes, Complete runs the revalidation pipeline and finalizes the
// row, and Get, OpenContent and List serve completed objects. It is the
// module's runtime, constructed by NewModule and reachable through
// Module.ObjectService(); Module.Register attaches the registry it reads
// its host seams from, and until then its methods that need a store fail
// closed with ErrStoreUnavailable rather than pretending one exists.
type ObjectService struct {
	// objects is the metadata repository every method routes through, the
	// same instance Module.Objects() hands out.
	objects *ObjectRepository

	// queue is the jobs.Queue the completion pipeline enqueues thumbnail
	// derivation on. It is nil only on a Module that never registered (a
	// Module whose Register ran is guaranteed a queue by ErrQueueRequired);
	// the pipeline tolerates a nil queue by warning and completing anyway,
	// so a mis-wired direct construction degrades loudly instead of
	// panicking mid-completion.
	queue jobs.Queue

	// serviceHost carries the registry slice this service reads at call
	// time -- the ObjectStore bytes live in and the EventBus completion is
	// announced on -- plus the guarded seam accessors every storage
	// service shares (events.go). host is nil until Module.Register hands
	// the registry over (attach); methods that need a store fail closed
	// with ErrStoreUnavailable before then.
	serviceHost

	// cfg is the frozen policy the service enforces, resolved from the
	// Module's With* options at construction (see newObjectService).
	cfg serviceConfig
}

// serviceConfig is the resolved policy ObjectService enforces. Module builds
// one from its own option-set fields; tests build one directly. A nil
// allowedTypes is resolved by newObjectService to the module default list,
// so "the host configured nothing" and "the host configured this module
// default" are indistinguishable to the enforcer -- the module default is a
// real restriction ({image/jpeg, image/png}), never an open door.
type serviceConfig struct {
	// maxUploadBytes is the ceiling a create's declared size may not pass.
	maxUploadBytes int64
	// maxImagePixels is the pixel ceiling images are decoded against.
	maxImagePixels int64
	// uploadTTL is how long a declared upload stays completable. Rows whose
	// window has passed are refused by Upload and Complete alike; sweeping
	// them is a later round's job.
	uploadTTL time.Duration
	// maxObjectLifetime is the ceiling a create's requested retention may
	// not pass. Zero requested retention -- an object that never expires --
	// is always allowed; the ceiling clamps what a tenant may ask for.
	maxObjectLifetime time.Duration
	// allowedTypes is the module's allowlist, exact lowercase media types.
	// It gates both the declared type at create time and the probed type at
	// complete time.
	allowedTypes []string
}

// newObjectService returns an ObjectService enforcing cfg over objects.
//
// A nil cfg.allowedTypes resolves to the module default list (module.go's
// defaultAllowedTypes), which is what "the host never called WithAllowedTypes"
// means once the service exists to enforce the policy. Each caller's slice
// is copied, never held.
func newObjectService(objects *ObjectRepository, queue jobs.Queue, cfg serviceConfig) *ObjectService {
	if cfg.allowedTypes == nil {
		cfg.allowedTypes = append([]string(nil), defaultAllowedTypes...)
	}
	return &ObjectService{objects: objects, queue: queue, cfg: cfg}
}

// defaultListPageSize is the page size List serves a caller that asks for
// zero or a negative limit.
const defaultListPageSize = 50

// CreateParams is the uploader's declaration of intent for one new object,
// taken before any bytes exist. The Declared* values are claims: they bound
// and describe the upload that must follow, and Complete reconciles each of
// them against the bytes that actually arrive.
type CreateParams struct {
	// DeclaredSize is the byte length of the content the uploader intends
	// to send. It must be positive and at most the module's upload ceiling;
	// the upload is bounded by it byte for byte.
	DeclaredSize int64
	// DeclaredType is the media type the uploader believes the content
	// carries, free-form (a browser-derived value, say). It must be one of
	// the module's allowlisted types when non-empty; "" declares no belief
	// at all, in which case the bytes' probed type is checked alone.
	DeclaredType string
	// DeclaredChecksum is the SHA-256 digest, 64 lowercase hex characters,
	// of the content the uploader intends to send. "" declares no checksum,
	// in which case no comparison is performed.
	DeclaredChecksum string
	// Retention is how long the completed object may live before it
	// expires. Zero means the object does not expire. A positive value
	// longer than the module's configured maximum is refused at create
	// time, before any byte is transferred.
	Retention time.Duration
}

// Create opens a new object: it validates the declaration, reserves the
// object's id and internal store key, and writes the row in
// ObjectStateUploading with an upload window of the module's configured
// uploadTTL. No bytes exist yet; Upload must stream them before the window
// passes, and Complete finalizes them after that.
//
// Validation happens here, before any byte transfers, so a declaration that
// can never complete is refused cheaply: a non-positive DeclaredSize
// (storage.invalid_size), one above the module's ceiling
// (storage.object_too_large), a malformed DeclaredChecksum
// (storage.invalid_checksum), a DeclaredType outside the module's allowlist
// (storage.type_not_allowed), and a Retention past the module's maximum
// (storage.invalid_expiry). The declared type is canonicalized (parameters
// stripped, case folded) before it is stored, so the row always holds the
// canonical form of what the uploader meant.
//
// The caller's tenant comes from the context (pkgcore.WithTenant), never
// from a parameter -- the multi-tenant rules leave no other source. A
// context without a tenant fails closed with ErrInternal wrapping
// pkgcore.ErrNoTenant, exactly as the repositories fail closed.
//
// The returned Object is the stored row: State is ObjectStateUploading,
// UploadExpiresAt marks the upload deadline, and the Declared* fields carry
// the validated declaration. The object is invisible to Get, OpenContent
// and List until Complete finalizes it.
func (s *ObjectService) Create(ctx context.Context, params CreateParams) (Object, error) {
	if params.DeclaredSize <= 0 {
		return Object{}, ErrInvalidSize
	}
	if params.DeclaredSize > s.cfg.maxUploadBytes {
		return Object{}, ErrObjectTooLarge.WithParam("max_bytes", s.cfg.maxUploadBytes)
	}
	if params.DeclaredChecksum != "" && !validSHA256Hex(params.DeclaredChecksum) {
		return Object{}, ErrInvalidChecksum
	}
	declaredType := canonicalMediaType(params.DeclaredType)
	if declaredType != "" {
		if err := checkAllowedMediaType(declaredType, s.cfg.allowedTypes); err != nil {
			return Object{}, err
		}
	}
	if params.Retention > 0 && params.Retention > s.cfg.maxObjectLifetime {
		return Object{}, ErrInvalidExpiry.WithParam(
			"max_lifetime_days", int64(s.cfg.maxObjectLifetime/(24*time.Hour)))
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return Object{}, ErrInternal.WithCause(err)
	}

	id := uuid.NewString()
	key, err := ObjectKey(tenant, id)
	if err != nil {
		// The key grammar cannot fail on a UUID it just generated; if it
		// ever does, this is a bug in this module, not a client error.
		return Object{}, ErrInternal.WithCause(err)
	}
	now := time.Now()
	row := Object{
		ID:               id,
		TenantModel:      dbkit.TenantModel{TenantID: string(tenant)},
		Key:              key,
		State:            ObjectStateUploading,
		DeclaredSize:     params.DeclaredSize,
		DeclaredType:     declaredType,
		DeclaredChecksum: params.DeclaredChecksum,
		UploadExpiresAt:  now.Add(s.cfg.uploadTTL),
	}
	if params.Retention > 0 {
		expiresAt := now.Add(params.Retention)
		row.ExpiresAt = &expiresAt
	}
	if err := s.objects.Create(ctx, &row); err != nil {
		return Object{}, ErrInternal.WithCause(err)
	}
	return row, nil
}

// Upload streams the object's bytes into the object store: one atomic
// PutObject under the key Create reserved, bounded so that the stored byte
// count can never exceed the declared size by more than one byte (the
// probe that detects an oversize body).
//
// contentLength is the request body's length as the transport observed it
// (the HTTP Content-Length header, say). When the transport observed one,
// a length different from the declared size is refused up front with
// storage.content_length_mismatch -- the mismatch is detectable before the
// body is read, so the body is never streamed. nil means the transport
// could not observe a length (a chunked request body), the comparison is
// skipped, and the bounded streaming read below enforces the declared size
// on its own: a body that ends early or runs past the declared size lands
// in storage.size_mismatch, and the partial bytes are removed from the
// store again (best effort) so no truncated object lingers under the key.
//
// The object must still be in ObjectStateUploading and inside its upload
// window; Upload on a finalized row reports storage.object_not_uploading,
// and Upload on an expired window reports storage.content_missing, the same
// answer Complete gives -- the window is the window. The row itself is not
// touched: it stays uploading until Complete runs the revalidation
// pipeline. Callers that observed no body bytes at all may pass a nil body;
// it streams as an empty body and fails the size reconciliation like any
// other short body.
func (s *ObjectService) Upload(ctx context.Context, objectID string, contentLength *int64, body io.Reader) error {
	row, err := s.findByID(ctx, objectID)
	if err != nil {
		return err
	}
	if row.State != ObjectStateUploading {
		return ErrObjectNotUploading.WithParam("id", objectID)
	}
	if time.Now().After(row.UploadExpiresAt) {
		return ErrContentMissing.WithParam("id", objectID)
	}
	if contentLength != nil && *contentLength != row.DeclaredSize {
		return ErrContentLengthMismatch.WithParam("declared", row.DeclaredSize)
	}
	st, err := s.requireStore()
	if err != nil {
		return err
	}
	if body == nil {
		body = bytes.NewReader(nil)
	}
	counter := &countingReader{r: io.LimitReader(body, row.DeclaredSize+1)}
	if err := st.PutObject(ctx, row.Key, counter); err != nil {
		return ErrStoreError.WithCause(err)
	}
	if counter.n != row.DeclaredSize {
		// The stored bytes do not match the declaration. Remove them again
		// so a rejected attempt never leaves partial content behind; the
		// store's delete is idempotent, and a failure to clean up is
		// logged, not fatal -- the next upload attempt replaces the bytes
		// atomically anyway.
		if err := st.DeleteObject(ctx, row.Key); err != nil {
			observability.FromContext(ctx).Warn("rejected upload cleanup failed",
				"object_id", objectID, "error", err)
		}
		return checkStoredSize(counter.n, row.DeclaredSize)
	}
	return nil
}

// Complete finalizes an upload: it runs the revalidation pipeline over the
// stored bytes, and when they pass, advances the row to
// ObjectStateCompleted carrying the finalized metadata. Complete is the
// only transition out of ObjectStateUploading and the only place the
// pipeline runs.
//
// The pipeline's order is settled, and each refusal happens where it can be
// cheapest and most honest:
//
//  1. The row must exist in the caller's tenant and still be uploading
//     (storage.object_not_found, storage.object_not_uploading).
//  2. The upload window must not have passed (storage.content_missing): a
//     declaration whose window lapsed is reclaimed as never-arriving even
//     if bytes happen to sit under its key. The window is checked again at
//     the finalize write itself, so a pipeline that straddles its own
//     window end cannot commit after it.
//  3. The object store must be wired (storage.store_unavailable).
//  4. The stored bytes must exist (storage.content_missing -- the common
//     complete-before-upload shape) and must be readable
//     (storage.store_error).
//  5. The bytes must number exactly the declared size
//     (storage.size_mismatch); the read is bounded to declared+1 bytes, so
//     an oversize body is detected, never materialized.
//  6. When a checksum was declared, the bytes must digest to it
//     (storage.checksum_mismatch).
//  7. The bytes are probed (never trusted from a header) and the probed
//     type must be on the module's allowlist (storage.type_not_allowed).
//  8. A declared type must agree with the probe (storage.type_mismatch).
//     A missing declaration skips this check; the allowlist check above
//     still gates the probed type on its own.
//  9. Image bytes are decoded against the pixel ceiling
//     (storage.pixel_limit_exceeded past it, storage.image_unreadable when
//     undecodable), which also yields the finalized dimensions.
//  10. Image bytes pass the structural sanitizer (storage.image_unreadable
//     on refusal), and when the sanitizer removed metadata -- EXIF and XMP
//     from JPEG, the eXIf chunk from PNG; GPS coordinates and authorship
//     included -- the sanitized bytes atomically replace the stored ones.
//
// The row then advances to completed with the finalized metadata -- Size,
// MIME, ChecksumSHA256 and the dimensions, all describing the bytes
// actually stored, which after a sanitizer rewrite are the sanitized ones
// (so a finalized digest may differ from a declared one exactly when the
// declared digest described bytes carrying metadata that was stripped).
//
// The advance is a conditional commit (ObjectRepository.finalizeUpload):
// only a row still uploading with its window still open may be finalized.
// When the write commits zero rows -- the object vanished mid-pipeline,
// its window closed, or another completion won the transition -- Complete
// answers with the code that matches what a re-read finds
// (storage.object_not_found, storage.content_missing,
// storage.object_not_uploading) and runs no side effect: a finalize that
// did not commit announces nothing, enqueues nothing and logs nothing.
//
// Side effects follow the finalize, and neither can fail the call: an
// image object's thumbnail derivation is enqueued on the module's queue
// (a failure is logged and the completion stands -- the object is done, the
// thumbnail is a rendition of it, and the derive worker may also be retried
// by whatever reconciles queue work), and the EventObjectCompleted event is
// published on the registry's bus (a failure is logged the same way). Both
// use the request context, so tenant correlation survives into the queue
// task and the event.
//
// The returned Object is the completed row.
func (s *ObjectService) Complete(ctx context.Context, objectID string) (Object, error) {
	row, err := s.findByID(ctx, objectID)
	if err != nil {
		return Object{}, err
	}
	if row.State != ObjectStateUploading {
		return Object{}, ErrObjectNotUploading.WithParam("id", objectID)
	}
	if time.Now().After(row.UploadExpiresAt) {
		return Object{}, ErrContentMissing.WithParam("id", objectID)
	}
	st, err := s.requireStore()
	if err != nil {
		return Object{}, err
	}
	rc, err := st.GetObject(ctx, row.Key)
	if err != nil {
		if errors.Is(err, pkgcore.ErrObjectNotFound) {
			return Object{}, ErrContentMissing.WithParam("id", objectID)
		}
		return Object{}, ErrStoreError.WithCause(err)
	}
	raw, err := io.ReadAll(io.LimitReader(rc, row.DeclaredSize+1))
	if err != nil {
		_ = rc.Close()
		return Object{}, ErrStoreError.WithCause(err)
	}
	err = rc.Close()
	if err != nil {
		return Object{}, ErrStoreError.WithCause(err)
	}
	err = checkStoredSize(int64(len(raw)), row.DeclaredSize)
	if err != nil {
		return Object{}, err
	}
	err = checkContentChecksum(raw, row.DeclaredChecksum)
	if err != nil {
		return Object{}, err
	}
	probed := probeMediaType(raw)
	err = checkAllowedMediaType(probed, s.cfg.allowedTypes)
	if err != nil {
		return Object{}, err
	}
	err = checkDeclaredTypeMatches(row.DeclaredType, probed)
	if err != nil {
		return Object{}, err
	}

	stored := raw
	changed := false
	if isImageMediaType(probed) {
		width, height, decodeErr := decodeImageFacts(raw, s.cfg.maxImagePixels)
		if decodeErr != nil {
			return Object{}, decodeErr
		}
		sanitized, wasStripped, stripErr := sanitizeContent(raw, probed)
		if stripErr != nil {
			return Object{}, ErrImageUnreadable.WithCause(stripErr)
		}
		changed = wasStripped
		if wasStripped {
			if writeErr := st.PutObject(ctx, row.Key, bytes.NewReader(sanitized)); writeErr != nil {
				return Object{}, ErrStoreError.WithCause(writeErr)
			}
			stored = sanitized
		}
		row.Width = &width
		row.Height = &height
	}

	size := int64(len(stored))
	mime := probed
	digest := sha256HexDigest(stored)
	row.State = ObjectStateCompleted
	row.Size = &size
	row.MIME = &mime
	row.ChecksumSHA256 = &digest
	done, err := s.objects.finalizeUpload(ctx, row, time.Now())
	if err != nil {
		return Object{}, err
	}
	if !done {
		// The finalize wrote zero rows, so the transition did not commit:
		// between the entry checks and this write the row vanished (the
		// expiry sweep reclaimed it), its upload window closed (the
		// deadline is enforced at the write, not just at the entry check),
		// or a concurrent completion won the transition first. Re-read to
		// tell the three apart and answer with what the caller will find.
		// Reporting success for a finalize that did not commit would be a
		// silent false success -- and so would running the log line, the
		// thumbnail task or the completion event below it, which is why
		// they all sit behind the committed branch.
		current, err := s.findByID(ctx, objectID)
		if err != nil {
			return Object{}, err
		}
		if current.State == ObjectStateUploading {
			return Object{}, ErrContentMissing.WithParam("id", objectID)
		}
		return Object{}, ErrObjectNotUploading.WithParam("id", objectID)
	}

	observability.FromContext(ctx).Info("object completed",
		"object_id", row.ID, "size", size, "mime", mime, "sanitized", changed)
	if isImageMediaType(probed) {
		if enqueueErr := s.enqueueThumbnailDerive(ctx, row); enqueueErr != nil {
			observability.FromContext(ctx).Warn("thumbnail derive enqueue failed",
				"object_id", row.ID, "error", enqueueErr)
		}
	}
	if err := s.publish(ctx, pkgcore.Event{
		Type:     EventObjectCompleted,
		TenantID: row.GetTenantID(),
		Payload:  ObjectCompletedPayload{ObjectID: row.ID, Size: size, MIME: mime},
	}); err != nil {
		observability.FromContext(ctx).Warn("completion event publish failed",
			"object_id", row.ID, "error", err)
	}
	return *row, nil
}

// Get returns the metadata of one completed object of the caller's tenant.
//
// Only completed objects are readable: an object still uploading or being
// deleted reports storage.object_not_found, exactly as if the id did not
// exist, so the listing and the read agree that an unfinished object is not
// there. The bytes live in the object store under an internal key; Get
// returns metadata only, and OpenContent is the method that opens the
// bytes themselves.
func (s *ObjectService) Get(ctx context.Context, objectID string) (Object, error) {
	row, err := s.findByID(ctx, objectID)
	if err != nil {
		return Object{}, err
	}
	if row.State != ObjectStateCompleted {
		return Object{}, ErrObjectNotFound.WithParam("id", objectID)
	}
	return *row, nil
}

// OpenContent opens the stored bytes of one completed object, returning
// them as a stream together with the row whose finalized metadata describes
// them (the MIME to serve, the size, the digest). The caller owns the
// returned reader and must close it.
//
// The object must be completed, with the same visibility rule as Get.
// Because only completed objects are reachable, a store that no longer
// holds the bytes of one is an anomaly -- the row says the bytes were
// stored and finalized -- and is reported as storage.store_error with the
// store's error as the cause, never as a plausible "not found".
func (s *ObjectService) OpenContent(ctx context.Context, objectID string) (Object, io.ReadCloser, error) {
	row, err := s.findByID(ctx, objectID)
	if err != nil {
		return Object{}, nil, err
	}
	if row.State != ObjectStateCompleted {
		return Object{}, nil, ErrObjectNotFound.WithParam("id", objectID)
	}
	st, err := s.requireStore()
	if err != nil {
		return Object{}, nil, err
	}
	rc, err := st.GetObject(ctx, row.Key)
	if err != nil {
		// A completed row is supposed to hold stored bytes: its metadata was
		// finalized from them at Complete. A store that no longer has them is
		// an anomaly -- a not-found answer from the store included -- so
		// report it as such, never as a plausible missing object.
		return Object{}, nil, ErrStoreError.WithCause(err)
	}
	return *row, rc, nil
}

// List returns completed objects of the caller's tenant, newest first (ties
// broken by id, so the order is total and stable across engines), in pages
// of at most limit rows -- defaultListPageSize when limit is zero or
// negative. Pass an empty beforeID for the first page and the last page's
// final row's id as the next page's beforeID: the cursor is a keyset, not
// an offset, so rows created between two fetches shift nothing.
//
// Only completed objects are listed -- uploading and deleting rows stay
// invisible, the same rule Get enforces. A beforeID naming a row that is
// not in this listing, including one that left the completed state since
// the caller's last page, reports storage.object_not_found, never a page
// silently resumed from the wrong place.
func (s *ObjectService) List(ctx context.Context, limit int, beforeID string) ([]Object, error) {
	if limit <= 0 {
		limit = defaultListPageSize
	}
	rows, err := s.objects.listPageState(ctx, ObjectStateCompleted, limit, beforeID)
	if err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return nil, ErrObjectNotFound.WithParam("id", beforeID)
		}
		return nil, err
	}
	return rows, nil
}

// findObjectByID loads one object of the caller's tenant through the
// repository and maps its errors onto the service vocabulary: a row that
// does not exist (or belongs to another tenant) becomes
// storage.object_not_found, everything else storage.internal_error with the
// repository's error as the cause. Every storage service that looks an
// object row up -- the transfer runtime and the derive worker alike --
// shares this one mapping, so a not-found answer means the same thing in
// both, and a lookup is never remapped differently depending on which
// service performed it.
func findObjectByID(ctx context.Context, objects *ObjectRepository, objectID string) (*Object, error) {
	row, err := objects.FindByID(ctx, objectID)
	if err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return nil, ErrObjectNotFound.WithParam("id", objectID)
		}
		return nil, ErrInternal.WithCause(err)
	}
	return row, nil
}

// findByID is ObjectService's view of findObjectByID, kept as a method so
// the transfer runtime's call sites stay as they were.
func (s *ObjectService) findByID(ctx context.Context, objectID string) (*Object, error) {
	return findObjectByID(ctx, s.objects, objectID)
}

// taskTypeDeriveThumbnail names the jobs queue task the completion pipeline
// enqueues for a completed image object. The task's payload is a
// deriveThumbnailTaskPayload; the worker that claims the type reads the
// object's row from the task's tenant and id and produces the thumbnail
// under the derivative key the grammar in key.go fixes. Module.Register
// registers the type's handler (derive.go's deriveHandler, backed by
// DeriveService); a host that wants registered handlers claimed drains
// reg.Jobs.Handlers() onto its jobs.Queue after Bootstrap, exactly as
// go/jobs documents.
const taskTypeDeriveThumbnail = "storage.object.derive.thumbnail"

// deriveThumbnailTaskPayload is the JSON payload of the thumbnail-derive
// task: just the object id. The tenant travels in jobs.Task.TenantID, the
// row (whose MIME and dimensions the worker needs) is read from the id, and
// the object-store key of the original bytes is derivable from tenant and
// id alone -- a payload that duplicated any of that would only be able to
// drift from the row it describes.
type deriveThumbnailTaskPayload struct {
	ObjectID string `json:"object_id"`
}

// thumbnailDeriveIdempotencyKey derives the jobs idempotency key of a
// thumbnail-derive task from the object id, per the rule that an
// idempotency key derives from the business operation, never random: two
// enqueues for the same object's thumbnail collapse into one job. The
// "storage.thumbnail:" prefix keeps the key inside the module's namespace
// within the shared queue store.
func thumbnailDeriveIdempotencyKey(objectID string) string {
	return "storage.thumbnail:" + objectID
}

// enqueueThumbnailDerive enqueues the thumbnail-derive task for a completed
// image object, with the task's tenant taken from the row (never from the
// context -- a queue worker must rebuild tenant context itself, and the
// enqueue side must not assume the caller's context outlives the call).
func (s *ObjectService) enqueueThumbnailDerive(ctx context.Context, obj *Object) error {
	if s.queue == nil {
		return errors.New("storage: no queue wired")
	}
	payload, err := json.Marshal(deriveThumbnailTaskPayload{ObjectID: obj.ID})
	if err != nil {
		// Two strings cannot fail to marshal; if this ever fires it is a
		// bug in this module, and the completion it would have followed
		// stands regardless.
		return err
	}
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeDeriveThumbnail,
		TenantID:       obj.GetTenantID(),
		Payload:        payload,
		IdempotencyKey: thumbnailDeriveIdempotencyKey(obj.ID),
	})
	return err
}

// countingReader counts the bytes a PutObject actually consumed, so the
// upload pipeline can reconcile the stored count against the declaration
// after the fact. Reads are bounded by the LimitReader the caller wraps;
// the count therefore saturates at declared+1 for an oversize body, which
// is exactly the evidence the reconciliation needs.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
