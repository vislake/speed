//go:build integration

package s3_test

// Integration tests for s3.NewObjectStore: each test drives the store
// through the public ObjectStore interface against a real MinIO, asserting
// the exact semantics pkgcore's local store's unit tests pin -- a missing
// key surfaces as pkgcore.ErrObjectNotFound, an empty object exists and is
// distinct from a missing one, overwrites replace the object whole,
// DeleteObject is idempotent -- plus the properties only a shared service
// can exercise: a multi-megabyte object streaming through the multipart
// machinery intact, a mid-upload cancellation leaving no object behind, a
// cancellation cutting an open reader's stream with the context's error, and
// a prefix-overlapping pair of keys -- accepted by the service where the
// local store refuses the clash -- showing that deleting the shorter key can
// take the longer key's object with it. Everything the tests assert is
// observable through the ObjectStore interface itself, so no raw client
// doubles as an oracle here.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/objectstoretest"
)

// readObject streams one object to the end and closes its reader, failing the
// test on any error. The caller owns the reader and must Close it on every
// path, so the helper is where both the read and the close happen.
func readObject(t *testing.T, store pkgcore.ObjectStore, ctx context.Context, key string) []byte {
	t.Helper()
	reader, err := store.GetObject(ctx, key)
	if err != nil {
		t.Fatalf("GetObject(%q) error = %v, want nil", key, err)
	}
	content, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("reading %q: %v", key, readErr)
	}
	if closeErr != nil {
		t.Errorf("closing %q: %v", key, closeErr)
	}
	return content
}

func TestObjectStore_PutGetDelete_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store := startMinioObjectStore(t, ctx)

	// A key that has never been stored reads as missing: the service's own
	// NoSuchKey must surface as the interface's ErrObjectNotFound, and the
	// request must fail before a reader exists, not on some later read.
	reader, err := store.GetObject(ctx, "exports/users/2024-09-02.csv")
	if !errors.Is(err, pkgcore.ErrObjectNotFound) {
		t.Fatalf("GetObject() of a missing key error = %v, want ErrObjectNotFound", err)
	}
	if reader != nil {
		reader.Close()
		t.Error("GetObject() returned a reader alongside ErrObjectNotFound, want nil")
	}

	// An empty object exists and is distinct from a missing key: storing it
	// turns the ErrObjectNotFound above into an empty read.
	const emptyKey = "exports/empty.csv"
	if err := store.PutObject(ctx, emptyKey, strings.NewReader("")); err != nil {
		t.Fatalf("PutObject() of an empty object error = %v, want nil", err)
	}
	if content := readObject(t, store, ctx, emptyKey); len(content) != 0 {
		t.Errorf("empty object read back as %d bytes, want 0", len(content))
	}

	// A stored object reads back byte for byte, through an overwrite in both
	// directions (grow then shrink) exactly as the local store's round trip
	// pins: an overwrite replaces the object whole, never leaving a mix.
	const key = "exports/users/2024-09-02.csv"
	payloads := []string{
		"id,name\n1,ada\n2,grace\n",
		"id,name\n1,ada\n2,grace\n" + strings.Repeat("audit,trail,", 500),
		"shrunk back",
	}
	for _, payload := range payloads {
		if err := store.PutObject(ctx, key, strings.NewReader(payload)); err != nil {
			t.Fatalf("PutObject(%q) error = %v, want nil", key, err)
		}
		if content := readObject(t, store, ctx, key); string(content) != payload {
			t.Errorf("GetObject() = %d bytes, want the %d stored bytes back verbatim", len(content), len(payload))
		}
	}

	// DeleteObject removes the object, and deleting again -- or deleting a key
	// that never existed -- is the same success, so a retried cleanup is safe.
	if err := store.DeleteObject(ctx, key); err != nil {
		t.Fatalf("DeleteObject(%q) error = %v, want nil", key, err)
	}
	if err := store.DeleteObject(ctx, key); err != nil {
		t.Errorf("DeleteObject() of a missing key error = %v, want nil", err)
	}
	reader, err = store.GetObject(ctx, key)
	if !errors.Is(err, pkgcore.ErrObjectNotFound) {
		t.Fatalf("GetObject() after DeleteObject error = %v, want ErrObjectNotFound", err)
	}
	if reader != nil {
		reader.Close()
		t.Error("GetObject() returned a reader alongside ErrObjectNotFound, want nil")
	}
}

func TestObjectStore_StreamsAMultiMegabyteObjectIntact(t *testing.T) {
	ctx := context.Background()
	store := startMinioObjectStore(t, ctx)

	// Every unknown-length put goes through minio-go's multipart machinery,
	// whose memory use is bounded by its part buffer, never by the stream's
	// size. This body is larger than anything the local store's round trip
	// exercises, so the upload and the download both cross many network round
	// trips; the bytes must come back identical.
	const key = "exports/backups/2024-09-02-full.db"
	payload := strings.Repeat("0123456789abcdef", 512*1024) // 8 MiB
	if err := store.PutObject(ctx, key, strings.NewReader(payload)); err != nil {
		t.Fatalf("PutObject(%q) error = %v, want nil", key, err)
	}
	if content := readObject(t, store, ctx, key); !bytes.Equal(content, []byte(payload)) {
		t.Errorf("GetObject() = %d bytes, want the %d stored bytes back verbatim", len(content), len(payload))
	}
}

func TestObjectStore_CancelledMidUpload_LeavesNoObjectBehind(t *testing.T) {
	ctx := context.Background()
	store := startMinioObjectStore(t, ctx)

	// minio-go streams the source on the calling goroutine, so a reader that
	// parks after its first bytes makes the cancellation deterministic: the
	// upload cannot finish, and it cannot report anything, until the test
	// cancels the context.
	const key = "exports/streaming/hold.bin"
	uploadCtx, cancel := context.WithCancel(ctx)
	reader := &abortingReader{ctx: uploadCtx, started: make(chan struct{})}
	finished := make(chan error, 1)
	go func() { finished <- store.PutObject(uploadCtx, key, reader) }()

	// The first bytes have been handed to the upload; cancel it mid-stream.
	<-reader.started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("PutObject() cancelled mid-stream error = %v, want context.Canceled", err)
	}

	// The aborted upload must not have left an object behind: the key reads
	// as missing even though the upload got far enough to start.
	got, err := store.GetObject(ctx, key)
	if !errors.Is(err, pkgcore.ErrObjectNotFound) {
		t.Fatalf("GetObject() after the cancelled upload error = %v, want ErrObjectNotFound", err)
	}
	if got != nil {
		got.Close()
		t.Error("GetObject() returned a reader alongside ErrObjectNotFound, want nil")
	}

	// The store survives the aborted upload: a fresh put to the same key
	// works, and the object reads back whole.
	if err := store.PutObject(ctx, key, strings.NewReader("the retried upload")); err != nil {
		t.Fatalf("PutObject() after the aborted upload error = %v, want nil", err)
	}
	if content := readObject(t, store, ctx, key); string(content) != "the retried upload" {
		t.Errorf("GetObject() = %q, want the retried upload's bytes", content)
	}
}

func TestObjectStore_ReaderFailsItsReadsOnceTheContextIsCancelled(t *testing.T) {
	// The interface promises that a GetObject reader whose context is
	// cancelled fails its reads with the context's error, the property the
	// local store pins by checking its context before every file read.
	// minio-go keeps the same promise through the context captured when the
	// request began: a Read issued once that context is done fails up front
	// with the context's error, without touching the service again.
	ctx := context.Background()
	store := startMinioObjectStore(t, ctx)

	const key = "exports/streaming/cancel-this-read.bin"
	payload := strings.Repeat("0123456789abcdef", 256) // 4 KiB, far from exhausted by the reads below
	if err := store.PutObject(ctx, key, strings.NewReader(payload)); err != nil {
		t.Fatalf("PutObject(%q) error = %v, want nil", key, err)
	}

	readCtx, cancel := context.WithCancel(ctx)
	reader, err := store.GetObject(readCtx, key)
	if err != nil {
		t.Fatalf("GetObject() error = %v, want nil", err)
	}
	defer reader.Close()

	// The stream is live: a read while the context lives yields the object's
	// first bytes, so the failure that follows is the cancellation, not a
	// reader that never worked.
	buffer := make([]byte, 32)
	if n, err := reader.Read(buffer); err != nil || n == 0 {
		t.Fatalf("Read() before cancellation = (%d, %v), want bytes and no error", n, err)
	}

	// Cancelling the context kills the stream: the next read fails with the
	// context's error, exactly as the local store's reader does.
	cancel()
	if n, err := reader.Read(buffer); n != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("Read() after cancellation = (%d, %v), want (0, context.Canceled)", n, err)
	}

	// The cancelled read does not disturb the object: a fresh request with a
	// live context reads it whole.
	if content := readObject(t, store, ctx, key); string(content) != payload {
		t.Errorf("GetObject() = %d bytes, want the %d stored bytes back verbatim", len(content), len(payload))
	}
}

func TestObjectStore_PrefixOverlap_DeletingTheShorterKeyTakesTheLongerKeyWithIt(t *testing.T) {
	// The interface's keyspace-tree rule (no key may be a proper prefix of
	// another stored key) is caller discipline: the local store refuses the
	// put that would create the clash, while the service accepts both keys.
	// This test pins what the service does with them, so nobody mistakes the
	// local store's refusal for pedantry: on this MinIO release, deleting the
	// shorter key removes the longer key's object as well, and the interface
	// deliberately makes no promise that a service will not. A caller that
	// lets a key and an extension of it exist at the same time hands its data
	// to whatever each backend happens to do with the overlap.
	ctx := context.Background()
	store := startMinioObjectStore(t, ctx)

	const (
		parentKey = "exports/orders/2024-09-02"
		childKey  = "exports/orders/2024-09-02/detail.json"
	)
	if err := store.PutObject(ctx, parentKey, strings.NewReader("order summary")); err != nil {
		t.Fatalf("PutObject(%q) error = %v, want nil", parentKey, err)
	}
	if err := store.PutObject(ctx, childKey, strings.NewReader(`{"id": 1042, "state": "paid"}`)); err != nil {
		t.Fatalf("PutObject(%q) error = %v, want nil", childKey, err)
	}

	// Both keys store and read back their own bytes: the overlap is accepted
	// by the service where the local store refuses the conflicting put.
	if content := readObject(t, store, ctx, parentKey); string(content) != "order summary" {
		t.Errorf("GetObject(%q) = %q, want \"order summary\"", parentKey, content)
	}
	if content := readObject(t, store, ctx, childKey); string(content) != `{"id": 1042, "state": "paid"}` {
		t.Errorf("GetObject(%q) = %q, want the child object's own bytes", childKey, content)
	}

	// Deleting the shorter key takes the longer key's object with it -- the
	// hazard the keyspace-tree rule exists to keep callers out of. Pinned
	// against this MinIO release rather than asserted as universal: a
	// different compatible service may behave differently, which is exactly
	// why the contract leaves the overlap outside it.
	if err := store.DeleteObject(ctx, parentKey); err != nil {
		t.Fatalf("DeleteObject(%q) error = %v, want nil", parentKey, err)
	}
	reader, err := store.GetObject(ctx, childKey)
	if !errors.Is(err, pkgcore.ErrObjectNotFound) {
		t.Fatalf("GetObject() of the longer key after deleting its prefix error = %v, want ErrObjectNotFound", err)
	}
	if reader != nil {
		reader.Close()
		t.Error("GetObject() returned a reader alongside ErrObjectNotFound, want nil")
	}
}

// abortingReader hands the upload a first chunk on its first read and then
// parks every later read until the upload's context is done, at which point
// reads fail with the context's error. PutObject streams the source on the
// calling goroutine and cannot finish while the reader is parked, so closing
// the context after the first read is a deterministic mid-upload
// cancellation.
type abortingReader struct {
	ctx     context.Context
	started chan struct{} // closed on the first read
	first   bool
}

func (r *abortingReader) Read(p []byte) (int, error) {
	if !r.first {
		r.first = true
		close(r.started)
		return copy(p, "bytes streaming to the service"), nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// TestObjectStore_ConformsToObjectStoreContract proves s3.NewObjectStore
// satisfies the shared contract objectstoretest.AssertConforms checks --
// the same suite go/pkgcore's own objectstore_conformance_test.go runs
// against pkgcore.NewLocalObjectStore -- against a real MinIO, so drift
// between the two ObjectStore implementations under the deployment-
// composition retrofit's N registered implementations per seam is caught
// here once instead of pairwise. Every subtest AssertConforms runs shares
// the one bucket startMinioObjectStore provisions for this test (one
// container per test file); this is safe because AssertConforms derives a
// distinct key per subtest from the subtest name (see its own conformKey),
// so concurrent subtests never collide on an object key.
func TestObjectStore_ConformsToObjectStoreContract(t *testing.T) {
	ctx := context.Background()
	store := startMinioObjectStore(t, ctx)

	objectstoretest.AssertConforms(t, func() pkgcore.ObjectStore {
		return store
	})
}
