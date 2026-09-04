// Package objectstoretest verifies that a pkgcore.ObjectStore implementation
// upholds the contract ObjectStore's own doc comment describes, independent
// of which backend implements it. It plays the same role for ObjectStore
// that go/tenancy/tenancytest.AssertIsolated plays for dbkit.Repository[T]
// and go/pkgcore/eventbustest.AssertConforms plays for EventBus: one suite
// every implementation — built-in (pkgcore.NewLocalObjectStore, the
// objectstore/s3 subpackage's NewObjectStore) or host-supplied through
// pkgcore.WithObjectStore — must pass, so drift between implementations is
// caught here once instead of pairwise (see
// docs/internal/03-deployment-modes.md).
package objectstoretest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// conformObjectSize is the size of the random payload the round-trip check
// puts and gets back. It is large enough to span several internal buffer
// fills (the local store copies in 32KiB chunks, see its PutObject) without
// making the suite slow.
const conformObjectSize = 100 * 1024

// AssertConforms verifies that the ObjectStore factory returns satisfies the
// contract documented on pkgcore.ObjectStore. Each subtest calls factory to
// get its own store handle and operates on a key it derives from the
// subtest name (see conformKey), so subtests sharing a long-lived backing
// store (as the S3/MinIO integration leg does, one container per test file)
// never collide on object keys.
//
// What AssertConforms checks, in order: GetObject on a key that was never
// put reports pkgcore.ErrObjectNotFound; PutObject followed by GetObject
// round-trips the exact bytes, unchanged, for a payload large enough to
// span multiple internal read/write chunks; PutObject followed by
// DeleteObject makes a subsequent GetObject report
// pkgcore.ErrObjectNotFound again; DeleteObject on a key that was never put
// is a success, not an error (idempotent delete); and PutObject followed by
// a second PutObject under the same key replaces the object, so GetObject
// returns the second payload, never a mix of the two.
func AssertConforms(t *testing.T, factory func() pkgcore.ObjectStore) {
	t.Helper()

	t.Run("get_object_on_a_never_put_key_reports_not_found", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "never-put")

		_, err := store.GetObject(context.Background(), key)
		if !errors.Is(err, pkgcore.ErrObjectNotFound) {
			t.Errorf("GetObject() error = %v, want errors.Is(err, pkgcore.ErrObjectNotFound)", err)
		}
	})

	t.Run("put_then_get_round_trips_the_exact_bytes_unchanged", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "round-trip")
		want := randomPayload(t, conformObjectSize)

		if err := store.PutObject(context.Background(), key, bytes.NewReader(want)); err != nil {
			t.Fatalf("PutObject() error = %v, want nil", err)
		}

		reader, err := store.GetObject(context.Background(), key)
		if err != nil {
			t.Fatalf("GetObject() error = %v, want nil", err)
		}
		defer func() { _ = reader.Close() }()

		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read the object body: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("object body of length %d does not match the %d bytes put", len(got), len(want))
		}
	})

	t.Run("delete_then_get_reports_not_found", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "delete")

		if err := store.PutObject(context.Background(), key, bytes.NewReader([]byte("gone soon"))); err != nil {
			t.Fatalf("PutObject() error = %v, want nil", err)
		}
		if err := store.DeleteObject(context.Background(), key); err != nil {
			t.Fatalf("DeleteObject() error = %v, want nil", err)
		}
		if _, err := store.GetObject(context.Background(), key); !errors.Is(err, pkgcore.ErrObjectNotFound) {
			t.Errorf("GetObject() after DeleteObject error = %v, want errors.Is(err, pkgcore.ErrObjectNotFound)", err)
		}
	})

	t.Run("delete_on_an_absent_key_is_a_success", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "absent-delete")

		if err := store.DeleteObject(context.Background(), key); err != nil {
			t.Errorf("DeleteObject() on a key that was never put error = %v, want nil", err)
		}
	})

	t.Run("a_second_put_replaces_the_object", func(t *testing.T) {
		t.Helper()
		store := factory()
		key := conformKey(t, "replace")

		if err := store.PutObject(context.Background(), key, bytes.NewReader([]byte("first version"))); err != nil {
			t.Fatalf("PutObject() first version error = %v, want nil", err)
		}
		if err := store.PutObject(context.Background(), key, bytes.NewReader([]byte("second version"))); err != nil {
			t.Fatalf("PutObject() second version error = %v, want nil", err)
		}

		reader, err := store.GetObject(context.Background(), key)
		if err != nil {
			t.Fatalf("GetObject() error = %v, want nil", err)
		}
		defer func() { _ = reader.Close() }()

		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read the object body: %v", err)
		}
		if string(got) != "second version" {
			t.Errorf("object body = %q, want %q", got, "second version")
		}
	})
}

// conformKey derives a key from t's name and suffix, so subtests sharing a
// long-lived backing store (the S3/MinIO integration leg starts one
// container per test file, not per case) never collide on object keys. The
// derived key stays within a single "/"-separated segment, which every
// pkgcore.ObjectStore implementation's key grammar accepts (see
// pkgcore.ErrInvalidObjectKey's doc comment on the segment-length limit).
func conformKey(t *testing.T, suffix string) string {
	t.Helper()
	return "objectstoretest-" + sanitizeKeySegment(t.Name()) + "-" + suffix
}

// sanitizeKeySegment replaces every character outside [A-Za-z0-9-] with '-',
// so a subtest's slash-separated t.Name() (e.g.
// "TestAssertConforms_LocalObjectStore/put_then_get") turns into a single
// valid key segment: pkgcore.ObjectStore's key grammar treats "/" as a
// path separator between segments, and a raw t.Name() would otherwise split
// the derived key at the subtest boundary and reach for a directory the
// object store must not create for a leaf key.
func sanitizeKeySegment(name string) string {
	out := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out[i] = c
		default:
			out[i] = '-'
		}
	}
	return string(out)
}

// randomPayload returns n random bytes for round-trip checks: random content
// makes a check that quietly compared two zero-filled buffers impossible.
func randomPayload(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate a random payload: %v", err)
	}
	return buf
}
