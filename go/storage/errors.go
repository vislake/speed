package storage

import (
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// moduleName is storage's pkgcore.Module.Name(). It is also the prefix of
// every error code declared below (<module>.<reason>, the convention this
// file's doc comment cites) and of the event types, permissions, audit
// actions and migration-registry keys module.go registers -- one namespace,
// one constant. It lives in errors.go rather than module.go because the
// error catalog and its tests are the first consumer: errors_test.go
// asserts that every code carries the prefix, and no test of module.go may
// be required for the errors' own suite to compile.
const moduleName = "storage"

// hasCode reports whether err is, or wraps, an *apperr.Error with the given
// code. Codes are compared rather than pointers because WithParam and
// WithCause derive a new *apperr.Error every time.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}

// The apperr half of the storage error index. Every exported error below
// is an *apperr.Error builder whose Code follows the <module>.<reason>
// convention the backend coding standard requires: match a decorated error
// with apperr.As(err) and compare its Code, never with == or errors.Is
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
	// the module's configured maximum, or otherwise unsatisfiable.
	ErrInvalidExpiry = apperr.Invalid("storage.invalid_expiry")

	// ErrStoreUnavailable reports an object-store access refused before
	// any operation was attempted -- the host wired no ObjectStore at
	// all, or the one it wired reports itself unavailable. It is the
	// module's fail-closed answer to a missing seam: an object whose
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
