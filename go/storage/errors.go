package storage

import (
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// moduleName is storage's module name (the value Name() answers). It is
// also the prefix of
// every error code declared below (<module>.<reason>, the convention this
// file's doc comment cites) and of the event types, permissions, audit
// actions and migration-registry keys module.go registers -- one namespace,
// one constant. It lives in errors.go rather than module.go because the
// error catalog and its tests are the first consumer: errors_test.go
// asserts that every code carries the prefix, and no test of module.go may
// be required for the errors' own suite to compile.
const moduleName = "storage"

// The apperr half of the storage error index. Every exported error below
// is an *apperr.Error builder whose Code follows the <module>.<reason>
// convention: match a decorated error with apperr.As(err) and compare its
// Code, never with == or errors.Is
// against the var below. WithParam and WithCause derive a NEW *apperr.Error
// rather than mutating the receiver, so the pointer a call returns is never
// the pointer declared here -- the same convention dbkit, tenancy and org
// already document. (ErrInvalidKey, declared in key.go, is the one plain
// error this package exports: it reports a programmer error in a key this
// module's own code built, which no API consumer can ever trigger, so it
// has no code, no status and no locale entry.)
//
// Every code in this file has a matching description entry in
// locales/{zh-CN,en-US}.toml, under the identical id. The API returns the
// code and its parameters; the text is resolved by the consumer.
var (
	// ErrObjectNotFound reports that no object with the requested id
	// exists in the caller's tenant. Like dbkit.ErrRecordNotFound it
	// deliberately does not distinguish "no such id anywhere" from "that
	// id belongs to another tenant": telling the two apart leaks the
	// existence of another tenant's object.
	ErrObjectNotFound = apperr.NotFound("storage.object_not_found")

	// ErrObjectNotUploading reports an operation that requires the object
	// to still be in ObjectStateUploading (completing an upload, say) on
	// an object that already completed or is being deleted.
	ErrObjectNotUploading = apperr.Conflict("storage.object_not_uploading")

	// ErrObjectUploading reports a deletion attempted on an object that is
	// still in ObjectStateUploading. Deleting rows never hit this error --
	// they resume the deletion protocol instead -- so it is the one state
	// a delete refuses: an upload in flight belongs to the transfer
	// runtime, and only the sweep reclaims uploading rows, once their
	// window closes. This is ErrObjectNotUploading's twin: that error says
	// "the object left uploading before the upload finished", this one
	// says "the object has not left uploading before the deletion asked
	// for it".
	ErrObjectUploading = apperr.Conflict("storage.object_uploading")

	// ErrObjectTooLarge reports a create whose declared size exceeds the
	// module's configured upload ceiling. The ceiling is the module's
	// limit, not the tenant's business choice: a tenant that wants to
	// store larger files configures the module's own maximum, it does not
	// get a per-object exemption.
	ErrObjectTooLarge = apperr.Invalid("storage.object_too_large")

	// ErrInvalidChecksum reports a checksum string that is not a well
	// formed SHA-256 digest (64 lowercase hex characters). It is refused
	// before any byte is uploaded, since comparing against a malformed
	// declared digest could only ever fail.
	ErrInvalidChecksum = apperr.Invalid("storage.invalid_checksum")

	// ErrChecksumMismatch reports a completed upload whose bytes do not
	// digest to the checksum the uploader declared. The upload is
	// rejected rather than stored under a claim it does not honour.
	ErrChecksumMismatch = apperr.Conflict("storage.checksum_mismatch")

	// ErrSizeMismatch reports a completed upload whose stored byte count
	// differs from the size the uploader declared at create time.
	ErrSizeMismatch = apperr.Conflict("storage.size_mismatch")

	// ErrContentLengthMismatch reports an upload request whose actual
	// Content-Length differs from the size its create declared. The
	// mismatch is detectable before the body is read, so the request is
	// refused rather than streamed.
	ErrContentLengthMismatch = apperr.Invalid("storage.content_length_mismatch")

	// ErrContentMissing reports a complete attempted on an object whose
	// bytes were never uploaded (or whose upload already expired).
	ErrContentMissing = apperr.Conflict("storage.content_missing")

	// ErrTypeNotAllowed reports a declared media type outside the
	// module's configured allowlist. Like ErrObjectTooLarge, the
	// allowlist is the module's own configured bound, not a per-object
	// judgement.
	ErrTypeNotAllowed = apperr.Invalid("storage.type_not_allowed")

	// ErrTypeMismatch reports a completed upload whose bytes probe to a
	// media type different from the one the uploader declared. A declared
	// image/jpeg that is really an HTML page is refused, not stored.
	ErrTypeMismatch = apperr.Invalid("storage.type_mismatch")

	// ErrPixelLimitExceeded reports an image whose pixel dimensions
	// exceed the module's configured pixel ceiling. The ceiling bounds
	// decode memory: an attacker's "small file" can declare a gigantic
	// pixel grid, so pixels are limited independently of bytes.
	ErrPixelLimitExceeded = apperr.Invalid("storage.pixel_limit_exceeded")

	// ErrImageUnreadable reports bytes that probe as an image but whose
	// header cannot be decoded far enough to establish dimensions or
	// integrity.
	ErrImageUnreadable = apperr.Invalid("storage.image_unreadable")

	// ErrInvalidExpiry reports a requested retention lifetime longer than
	// the module's configured maximum, or otherwise unsatisfiable -- a
	// never-expiring request that also names a finite retention, say.
	ErrInvalidExpiry = apperr.Invalid("storage.invalid_expiry")

	// ErrNoExpiryNotAllowed reports a CreateParams.NoExpiry request made of
	// a module that was not built with WithNoExpiryAllowed. The module's
	// maximum lifetime is the default life of an ordinary upload, and an
	// object that never expires would outlive that ceiling; only a host that
	// explicitly permits never-expiring objects (WithNoExpiryAllowed) may
	// have its service create them, so every other NoExpiry request is
	// refused here rather than silently honoured as a ceiling exception.
	ErrNoExpiryNotAllowed = apperr.Invalid("storage.no_expiry_not_allowed")

	// ErrSweepPartialFailure reports a LifecycleService.Sweep pass in which
	// at least one object could not be processed. The pass runs every row
	// regardless -- one object's failure never starves the rest of the
	// tenant's expiry work -- and this coded error is how a caller that
	// checks only "err != nil" still learns that the pass was not clean;
	// the failed rows were logged by id and stay in the state the next pass
	// resumes.
	ErrSweepPartialFailure = apperr.Internal("storage.sweep_partial_failure")

	// ErrAllowedTypeUnsupported reports a WithAllowedTypes entry for a media
	// type the module cannot admit safely. The whitelist may only admit a
	// type the module can apply its full safety envelope to -- pixel-check
	// the probed bytes (a registered decoder) AND strip their metadata
	// (sanitize.go's walkers, over the location/authorship carrier classes
	// those walkers enumerate for the type; the strip classifies carriers,
	// never payload bytes) -- because an admitted type is a promise that
	// every completed object of that type carries both protections, a
	// promise measured against the strip's declared scope rather than
	// against byte purity no structural walk could deliver, and the module
	// refuses configurations that would silently trade one away. It is a
	// wiring error, so Module.Register is where it surfaces.
	ErrAllowedTypeUnsupported = apperr.Internal("storage.allowed_type_unsupported")

	// ErrStoreUnavailable reports an object-store access refused before
	// any operation was attempted -- the host wired no ObjectStore at
	// all, or the one it wired reports itself unavailable. It is the
	// module's fail-closed answer to a missing component: an object whose
	// bytes cannot be stored is not pretended into existence.
	ErrStoreUnavailable = apperr.Internal("storage.store_unavailable")

	// ErrStoreError reports an object-store operation that failed after
	// it was attempted. The underlying error is carried as the cause so
	// the trace keeps it; the cause never reaches an API response body.
	ErrStoreError = apperr.Internal("storage.store_error")

	// ErrInvalidSize reports a create whose declared size is not a
	// positive integer -- zero or negative byte counts are not uploads.
	ErrInvalidSize = apperr.Invalid("storage.invalid_size")

	// ErrInternal reports a failure storage cannot classify -- a database
	// error, or a stored row that violates an invariant this module
	// maintains. It wraps the underlying error as its cause so the trace
	// carries it; the cause never reaches an API response body.
	ErrInternal = apperr.Internal("storage.internal_error")

	// ErrQueueRequired is returned by Module.Register when no queue was
	// injected with WithQueue.
	//
	// It mirrors org's ErrEmailIndexerRequired exactly: storage's
	// completed-object pipeline ends in a derived thumbnail whose
	// production is queued work, so a module instance with no queue
	// could complete objects it could never derive. Rather than starting
	// and silently degrading every completion, Register refuses to boot.
	ErrQueueRequired = apperr.Internal("storage.queue_required")
)

// The HTTP-transport half of the storage error index: codes the handlers
// serving the api/ fragment own, which no service method can return. They
// follow the same <module>.<reason> convention and carry the same locale
// entries as the codes above -- errors_test.go's errorCatalog extends over
// both var blocks, and its source-parsing completeness test spans this file
// as a whole.
var (
	// ErrInvalidRequestBody reports a request body the transport could not
	// decode as the operation's spec-generated JSON type -- the body was
	// absent, was not JSON, or did not have the shape the schema describes.
	// It is org's same-named error, and it carries no parameters: the schema
	// itself is the message, and no single field can be blamed for a body
	// that never parsed.
	ErrInvalidRequestBody = apperr.Invalid("storage.invalid_request_body")

	// ErrInvalidLimit reports a list request whose limit query parameter
	// falls outside the 1-200 bound the module's surface promises. The
	// parameters carry the offending value and the bound, so the rendered
	// message names all three.
	ErrInvalidLimit = apperr.Invalid("storage.invalid_limit")
)
