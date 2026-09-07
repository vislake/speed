package storage

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
)

// Key grammar for the object store.
//
// Every byte an Object or an ObjectDerivative names lives in the host's
// ObjectStore under a key this package builds and validates. The keys are
// deterministic -- the same tenant, object and derivative kind always map
// to the same key -- and they are an internal implementation detail: they
// are never exposed through any API surface (no response carries one, no
// request accepts one), so nothing outside this module may depend on their
// shape, and the grammar may evolve without a schema change.
//
// The grammar itself is the pkgcore ObjectStore grammar:
//
//   - a key is "/"-joined segments;
//   - every segment is at most keyMaxSegmentLen bytes;
//   - no segment is empty, "." or "..";
//   - no segment contains a backslash or a NUL byte;
//   - the whole key is at most keyMaxLen bytes.
//
// Both Object.Key and ObjectDerivative.Key are VARCHAR(512) columns and
// keyMaxLen == 512, so the grammar and the schema cannot drift apart: a key
// the grammar accepts always fits the column, on both dialects. SQLite does
// not enforce VARCHAR(n) under type affinity, which is why the length check
// is enforced here in Go rather than left to the database.
//
// The two key shapes this module builds:
//
//	<tenantID>/<objectID>/original                  -- the object's own bytes
//	<tenantID>/<objectID>/derivatives/<kind>        -- one derivative's bytes
//
// The shapes are fixed: an object key is exactly three "/"-joined segments
// and a derivative key exactly four, and the builders enforce the count
// (ObjectKey and DerivativeKey check their own joined result) on top of the
// per-segment rules. The count is what keeps the boundary between the
// components honest if an id alphabet ever stops being tame: a tenant id or
// object id that smuggles in a "/" would fabricate extra segments -- a key
// that still satisfies every per-segment rule while blurring where one
// component ends and the next begins, and, at the extreme, an object key
// of one pair that reads as a derivative key of another. Today the ids are
// the module's own uuid-shaped alphabet (lowercase hex and hyphens, no
// "/"), so the count check can never fire on them; it exists so the day an
// id alphabet changes, the violation is a loud builder error at the single
// create/derive call site, never a silently reshaped key. Every segment is
// still validated through the same validator a key from the wild would go
// through, per-segment rules included.
const (
	// keyMaxLen is the maximum total length of an object-store key in
	// bytes. It equals the VARCHAR(512) width of the key columns in
	// migrations 0001 and 0002 by construction -- see the migrations'
	// headers.
	keyMaxLen = 512

	// keyMaxSegmentLen is the maximum length of one key segment in bytes,
	// the pkgcore ObjectStore grammar's own segment bound.
	keyMaxSegmentLen = 255

	// keyOriginalSegment is the fixed final segment of an object's own
	// content key.
	keyOriginalSegment = "original"

	// keyDerivativesSegment is the fixed penultimate segment of a
	// derivative's content key.
	keyDerivativesSegment = "derivatives"
)

// ErrInvalidKey is returned by the key builder and validator when a
// segment violates the grammar above. It is a plain error, not an apperr:
// key violations are programmer errors reachable only by this module's own
// code (callers never supply raw keys), so there is no user-facing code for
// one and no locale entry to pair it with.
var ErrInvalidKey = errors.New("storage: invalid object-store key")

// ObjectKey returns the deterministic object-store key of object's content:
// "<tenantID>/<objectID>/original" -- exactly three "/"-joined segments.
// The shape's segment count is enforced here (see the grammar doc above):
// a tenant or object id containing a "/" makes four segments and is
// refused, so the key's fixed shape cannot silently stretch.
func ObjectKey(tenant pkgcore.TenantID, objectID string) (string, error) {
	key := string(tenant) + "/" + objectID + "/" + keyOriginalSegment
	if err := validateKey(key); err != nil {
		return "", err
	}
	if n := strings.Count(key, "/"); n != 2 {
		return "", fmt.Errorf("%w: object key %q has %d segment separators, want exactly 2 (%s/%s/%s)",
			ErrInvalidKey, key, n, "<tenantID>", "<objectID>", keyOriginalSegment)
	}
	return key, nil
}

// DerivativeKey returns the deterministic object-store key of one
// derivative of an object: "<tenantID>/<objectID>/derivatives/<kind>" --
// exactly four "/"-joined segments, count-enforced the same way ObjectKey
// enforces its own shape.
func DerivativeKey(tenant pkgcore.TenantID, objectID, kind string) (string, error) {
	key := string(tenant) + "/" + objectID + "/" + keyDerivativesSegment + "/" + kind
	if err := validateKey(key); err != nil {
		return "", err
	}
	if n := strings.Count(key, "/"); n != 3 {
		return "", fmt.Errorf("%w: derivative key %q has %d segment separators, want exactly 3 (%s/%s/%s/%s)",
			ErrInvalidKey, key, n, "<tenantID>", "<objectID>", keyDerivativesSegment, "<kind>")
	}
	return key, nil
}

// validateKey reports whether key obeys the grammar documented above. The
// key argument is a string of segments already joined with "/"; each
// segment is checked against every rule, so a malformed segment is rejected
// with the segment named in the error.
func validateKey(key string) error {
	if len(key) == 0 {
		return fmt.Errorf("%w: empty key", ErrInvalidKey)
	}
	if len(key) > keyMaxLen {
		return fmt.Errorf("%w: key is %d bytes, over the %d-byte maximum", ErrInvalidKey, len(key), keyMaxLen)
	}
	for _, segment := range strings.Split(key, "/") {
		if err := validateSegment(segment); err != nil {
			return fmt.Errorf("%w: segment %q: %w", ErrInvalidKey, segment, err)
		}
	}
	return nil
}

// validateSegment checks one "/"-separated segment of a key against the
// segment rules of the grammar.
func validateSegment(segment string) error {
	if segment == "" {
		return errors.New("empty segment")
	}
	if segment == "." || segment == ".." {
		return fmt.Errorf("dot segment %q is not allowed", segment)
	}
	if len(segment) > keyMaxSegmentLen {
		return fmt.Errorf("segment is %d bytes, over the %d-byte maximum", len(segment), keyMaxSegmentLen)
	}
	if strings.ContainsAny(segment, "\\\x00") {
		return errors.New("segment contains a backslash or NUL byte")
	}
	return nil
}
