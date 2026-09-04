package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newRootStore returns a fresh local store over a fresh private directory,
// along with that directory, for tests that inspect the file system below the
// store's root.
func newRootStore(t *testing.T) (string, ObjectStore) {
	t.Helper()
	root := t.TempDir()
	return root, NewLocalObjectStore(root)
}

// TestLocalObjectStore_CreatesItsRootWithoutWorldAccess pins the permission
// of a root directory the store creates itself: NewLocalObjectStore must not
// fall back to the 0o755 MkdirAll default, because the tree may hold PII
// objects and nothing outside the owning process should read it. A stricter
// umask may only remove bits, so the assert is one-sided.
func TestLocalObjectStore_CreatesItsRootWithoutWorldAccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	NewLocalObjectStore(root)

	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("store root permissions are %#o, want no access for other users", perm)
	}
}

// putObject stores body under key, failing the test on any error.
func putObject(t *testing.T, store ObjectStore, key, body string) {
	t.Helper()
	if err := store.PutObject(context.Background(), key, strings.NewReader(body)); err != nil {
		t.Fatalf("PutObject(%q) error = %v, want nil", key, err)
	}
}

// getObject reads the object under key back whole, failing the test on any
// error, and returns its content.
func getObject(t *testing.T, store ObjectStore, key string) string {
	t.Helper()
	reader, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("GetObject(%q) error = %v, want nil", key, err)
	}
	body, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("reading %q back failed: %v", key, err)
	}
	return string(body)
}

// TestValidateObjectKey pins the shared key grammar on its own, including the
// boundary lengths that a file system operation would refuse for other
// reasons (a path near PATH_MAX), so the grammar's own limits are tested
// without the file system's getting in the way.
func TestValidateObjectKey(t *testing.T) {
	segment := strings.Repeat("a", maxObjectSegmentLength)
	atTheLimit := strings.Join([]string{segment, segment, segment, segment}, "/") // 1023 bytes

	for _, key := range []string{
		"a",
		"object",
		"a/b/c",
		".hidden",
		"invoices/2026/1042.pdf",
		"日本語/キー", // any bytes are fine below the length limits
		segment,
		atTheLimit,
	} {
		if err := ValidateObjectKey(key); err != nil {
			t.Errorf("ValidateObjectKey(%q) error = %v, want nil", key, err)
		}
	}

	tooLong := strings.Repeat("x/", 512) + "x" // 1025 bytes
	for _, key := range []string{
		"",
		"/",
		"a/",
		"/a",
		"a//b",
		".",
		"..",
		"a/./b",
		"a/../b",
		"a\\b",
		"a\x00b",
		strings.Repeat("a", maxObjectSegmentLength+1),
		tooLong,
	} {
		if err := ValidateObjectKey(key); !errors.Is(err, ErrInvalidObjectKey) {
			t.Errorf("ValidateObjectKey(%q) error = %v, want it to wrap ErrInvalidObjectKey", key, err)
		}
	}
}

// TestEveryStoreRejectsAnInvalidKey pins the shared grammar at the interface
// level for the local store: the checks run before an operation touches the
// backend, so an invalid key is rejected without a single dial. No key value
// is echoed in the error text, so the message cannot leak key-shaped data.
// The objectstore/s3 subpackage runs the identical table against its own
// S3-backed store in its own test file, over ValidateObjectKey's shared
// grammar this test also proves for the local implementation -- it cannot be
// proven here too, because that subpackage imports this one and importing it
// back would be a cycle.
func TestEveryStoreRejectsAnInvalidKey(t *testing.T) {
	stores := map[string]ObjectStore{
		"local": NewLocalObjectStore(t.TempDir()),
	}
	operations := map[string]func(ObjectStore, string) error{
		"put": func(store ObjectStore, key string) error {
			return store.PutObject(context.Background(), key, strings.NewReader("x"))
		},
		"get": func(store ObjectStore, key string) error {
			_, err := store.GetObject(context.Background(), key)
			return err
		},
		"delete": func(store ObjectStore, key string) error { return store.DeleteObject(context.Background(), key) },
	}

	for _, key := range []string{
		"", "/", "a/", "/a", "a//b", ".", "..", "a/./b", "a/../b",
		"a\\b", "a\x00b", strings.Repeat("a", maxObjectSegmentLength+1),
	} {
		for storeName, store := range stores {
			for operationName, operation := range operations {
				err := operation(store, key)
				if !errors.Is(err, ErrInvalidObjectKey) {
					t.Errorf("%s store %s(%q) error = %v, want it to wrap ErrInvalidObjectKey", storeName, operationName, key, err)
				}
				// The error text must not repeat a caller-supplied key value.
				// The lone exceptions are keys that are themselves the
				// grammar's own metacharacters ("/", ".", ".."): the error
				// names the rule those quote, and quoting the rule's constant
				// is not echoing caller data. An empty key is a substring of
				// everything and is skipped too.
				switch key {
				case "", "/", ".", "..":
				default:
					if strings.Contains(err.Error(), key) {
						t.Errorf("%s store %s error text %q echoes the key", storeName, operationName, err)
					}
				}
			}
		}
	}
}

// TestLocalObjectStore_PutGetDelete_RoundTrip walks the whole lifecycle on
// one store: store, read back, overwrite (both shrinking and growing), and
// read back again.
func TestLocalObjectStore_PutGetDelete_RoundTrip(t *testing.T) {
	root, store := newRootStore(t)

	// An object may be empty; the store must not confuse it with a missing
	// object.
	putObject(t, store, "empty", "")
	if got := getObject(t, store, "empty"); got != "" {
		t.Errorf("empty object read back as %q, want \"\"", got)
	}

	// Bodies around the store's own 32 KiB copy buffer boundary.
	small := strings.Repeat("s", 32*1024)
	large := strings.Repeat("L", 32*1024+1)
	putObject(t, store, "scans/panoramic/2026-08-31", small)
	if got := getObject(t, store, "scans/panoramic/2026-08-31"); got != small {
		t.Errorf("round trip corrupted the object: got %d bytes, want %d", len(got), len(small))
	}

	// Overwrite replaces the previous object, whichever way the size moves.
	putObject(t, store, "scans/panoramic/2026-08-31", "replacement")
	if got := getObject(t, store, "scans/panoramic/2026-08-31"); got != "replacement" {
		t.Errorf("after a shrinking overwrite the object reads %q, want %q", got, "replacement")
	}
	putObject(t, store, "scans/panoramic/2026-08-31", large)
	if got := getObject(t, store, "scans/panoramic/2026-08-31"); got != large {
		t.Errorf("after a growing overwrite the object reads %d bytes, want %d", len(got), len(large))
	}

	// A second store over the same directory reads what the first wrote:
	// objects survive the store, only the process hosting it is throwaway,
	// which is how a standalone-mode host keeps objects across restarts.
	later := NewLocalObjectStore(root)
	if got := getObject(t, later, "scans/panoramic/2026-08-31"); got != large {
		t.Errorf("a second store over the same directory read %d bytes, want %d", len(got), len(large))
	}
}

// TestLocalObjectStore_GetObject_NotFound pins what GetObject reports for the
// many shapes a missing object can take: an absent key, a key below a
// missing directory, a key below an object (a parent that is a file), a
// directory (a trace of longer keys), and an object that was deleted.
func TestLocalObjectStore_GetObject_NotFound(t *testing.T) {
	_, store := newRootStore(t)

	putObject(t, store, "a/b/c", "deep")

	for _, key := range []string{
		"missing",
		"missing/sub",
		"a/b/c/more", // the parent path is an object, so nothing can sit below it
		"a",          // a directory is a trace of longer keys, never an object
		"a/b",
	} {
		reader, err := store.GetObject(context.Background(), key)
		if !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("GetObject(%q) error = %v, want ErrObjectNotFound", key, err)
		}
		if reader != nil {
			reader.Close()
			t.Errorf("GetObject(%q) returned a reader alongside the error, want nil", key)
		}
	}

	if err := store.DeleteObject(context.Background(), "a/b/c"); err != nil {
		t.Fatalf("DeleteObject() error = %v, want nil", err)
	}
	if _, err := store.GetObject(context.Background(), "a/b/c"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("GetObject() after delete error = %v, want ErrObjectNotFound", err)
	}
}

// TestLocalObjectStore_DeleteObject pins DeleteObject's idempotence: deleting
// a missing key, a key below nothing, a directory trace and the same object
// twice are all successes, and deleting an ancestor of stored keys succeeds
// without deleting the keys below it.
func TestLocalObjectStore_DeleteObject(t *testing.T) {
	root, store := newRootStore(t)

	if err := store.DeleteObject(context.Background(), "never-stored"); err != nil {
		t.Errorf("DeleteObject(missing) error = %v, want nil", err)
	}
	if err := store.DeleteObject(context.Background(), "never/stored/deep"); err != nil {
		t.Errorf("DeleteObject(below a missing directory) error = %v, want nil", err)
	}

	putObject(t, store, "a/b/c", "deep")
	putObject(t, store, "a/x", "x")

	// Deleting an ancestor of stored keys must not delete the keys below it:
	// the key being deleted never was an object, and neither was the
	// directory that holds the longer keys.
	if err := store.DeleteObject(context.Background(), "a/b"); err != nil {
		t.Errorf("DeleteObject(a directory that still holds objects) error = %v, want nil", err)
	}
	if got := getObject(t, store, "a/b/c"); got != "deep" {
		t.Errorf("the object below the deleted directory reads %q, want %q", got, "deep")
	}

	// Deleting the deeper keys leaves empty directory traces behind; deleting
	// those is a success and removes them.
	for _, key := range []string{"a/b/c", "a/b/c", "a/b", "a/x", "a"} {
		if err := store.DeleteObject(context.Background(), key); err != nil {
			t.Errorf("DeleteObject(%q) error = %v, want nil (idempotent delete)", key, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the store root still holds %v after every object was deleted, want it empty", entries)
	}
}

// TestLocalObjectStore_OverwriteIsAtomic pins the overwrite contract from the
// reader's side: an open reader keeps streaming the object as it was when the
// request started, untouched by an overwrite that lands mid-read.
func TestLocalObjectStore_OverwriteIsAtomic(t *testing.T) {
	_, store := newRootStore(t)

	original := strings.Repeat("original object bytes; ", 64*1024)
	replacement := strings.Repeat("replacement bytes; ", 1024)
	putObject(t, store, "scans/panoramic/2026-08-31", original)

	reader, err := store.GetObject(context.Background(), "scans/panoramic/2026-08-31")
	if err != nil {
		t.Fatalf("GetObject() error = %v, want nil", err)
	}
	// The object is now open. Overwrite it twice; neither write may disturb
	// the stream already underway.
	putObject(t, store, "scans/panoramic/2026-08-31", replacement)
	putObject(t, store, "scans/panoramic/2026-08-31", "even newer")

	body, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("reading the open object failed: %v", err)
	}
	if string(body) != original {
		t.Errorf("an open reader saw %d bytes of overwritten content, want the %d bytes the object held when the read started", len(body), len(original))
	}

	// A fresh read sees the final object, whole.
	if got := getObject(t, store, "scans/panoramic/2026-08-31"); got != "even newer" {
		t.Errorf("a fresh read after the overwrites got %q, want %q", got, "even newer")
	}
}

// TestLocalObjectStore_PutObject_FailureLeavesNoTrace pins the atomic-write
// contract from the writer's side: a PutObject whose source fails midway
// stores nothing and leaves no temporary or partial file behind.
func TestLocalObjectStore_PutObject_FailureLeavesNoTrace(t *testing.T) {
	root, store := newRootStore(t)

	sourceErr := errors.New("the source stream broke")
	err := store.PutObject(context.Background(), "a/b/c", &failingReader{remaining: 100, failWith: sourceErr})
	if !errors.Is(err, sourceErr) {
		t.Fatalf("PutObject() error = %v, want it to wrap the source's error", err)
	}
	// getErr: the PutObject error above stays live for the directory-listing
	// check below, which reuses err.
	if _, getErr := store.GetObject(context.Background(), "a/b/c"); !errors.Is(getErr, ErrObjectNotFound) {
		t.Errorf("GetObject() after a failed put error = %v, want ErrObjectNotFound", getErr)
	}
	// The failed upload's temporary file was cleaned up, and no partial
	// object file was published.
	parent := filepath.Join(root, "a", "b")
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the failed put left %v behind in the object's directory, want nothing", entries)
	}
}

// failingReader supplies bytes until its budget runs out, then fails with the
// error it was built with, standing in for a stream that breaks mid-upload.
type failingReader struct {
	remaining int
	failWith  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, r.failWith
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= len(p)
	return len(p), nil
}

// TestLocalObjectStore_PrefixClash_IsRefused pins the keyspace-tree rule on
// the local backend: a key must not be a proper prefix of a stored key, and
// the file system cannot represent such an overlap, so the store refuses the
// PutObject that would create one, leaving everything stored so far intact.
func TestLocalObjectStore_PrefixClash_IsRefused(t *testing.T) {
	_, store := newRootStore(t)

	putObject(t, store, "a/b/c", "deep")

	// Storing a key that would need a directory already occupied by objects.
	if err := store.PutObject(context.Background(), "a", strings.NewReader("x")); err == nil {
		t.Error("PutObject(\"a\") succeeded under a stored \"a/b/c\", want a refusal")
	}
	if err := store.PutObject(context.Background(), "a/b", strings.NewReader("x")); err == nil {
		t.Error("PutObject(\"a/b\") succeeded under a stored \"a/b/c\", want a refusal")
	}
	// And the other way around: storing a key below an existing object.
	putObject(t, store, "leaf", "leaf")
	if err := store.PutObject(context.Background(), "leaf/twig", strings.NewReader("x")); err == nil {
		t.Error("PutObject(\"leaf/twig\") succeeded below the stored \"leaf\", want a refusal")
	}
	if got := getObject(t, store, "a/b/c"); got != "deep" {
		t.Errorf("after the refused puts the stored object reads %q, want %q", got, "deep")
	}
	if got := getObject(t, store, "leaf"); got != "leaf" {
		t.Errorf("after the refused put the stored object reads %q, want %q", got, "leaf")
	}
}

// TestLocalObjectStore_PutAfterDeepDelete_ReclaimsTheEmptyTraces pins the
// complement of the prefix-clash rule. Deleting a deep key leaves the
// directories that led to it behind, empty once the object is gone, and an
// empty directory is only a trace: no object any longer claims the tree below
// the key, so storing the key itself or a shorter prefix of the deleted key is
// legal and must succeed, the traces clearing as the put lands. Until they
// did, such a put failed against the leftover directory while the S3 backend
// accepted the same call sequence, so the two implementations diverged on a
// legal one.
func TestLocalObjectStore_PutAfterDeepDelete_ReclaimsTheEmptyTraces(t *testing.T) {
	root, store := newRootStore(t)

	// A deep key, then its deletion: only the empty directory traces "a" and
	// "a/b" remain below the root.
	putObject(t, store, "a/b/c", "deep")
	if err := store.DeleteObject(context.Background(), "a/b/c"); err != nil {
		t.Fatalf("DeleteObject() error = %v, want nil", err)
	}

	// Storing the deleted key's shallowest prefix lands where its trace was:
	// the empty directories give way to the object.
	putObject(t, store, "a", "the trace became a leaf")
	if got := getObject(t, store, "a"); got != "the trace became a leaf" {
		t.Errorf("the object over the reclaimed trace reads %q, want %q", got, "the trace became a leaf")
	}
	// The trace is gone, so nothing of the deleted key is left to read.
	if _, err := store.GetObject(context.Background(), "a/b/c"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("GetObject() below the reclaimed trace error = %v, want ErrObjectNotFound", err)
	}

	// The same reclamation one level down, where the put descends into the
	// surviving trace "x" before landing on the trace "x/y".
	putObject(t, store, "x/y/z", "deep again")
	if err := store.DeleteObject(context.Background(), "x/y/z"); err != nil {
		t.Fatalf("DeleteObject() error = %v, want nil", err)
	}
	putObject(t, store, "x/y", "a middle leaf")
	if got := getObject(t, store, "x/y"); got != "a middle leaf" {
		t.Errorf("the object over the deeper reclaimed trace reads %q, want %q", got, "a middle leaf")
	}
	if _, err := store.GetObject(context.Background(), "x"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("GetObject(%q) error = %v, want ErrObjectNotFound", "x", err)
	}

	// The file system below the root holds exactly the two objects: the "a"
	// file and the "x" directory that the "x/y" object lives in, and no trace
	// of the deleted keys.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the store root holds %v after the reclaimed puts, want the two objects' entries only", entries)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "a":
			if entry.IsDir() {
				t.Error("the reclaimed key \"a\" is still a directory trace, want the object file")
			}
		case "x":
			if !entry.IsDir() {
				t.Error("the surviving directory \"x\" became a file, want the directory holding \"x/y\"")
			}
		default:
			t.Errorf("the store root holds an unexpected entry %q", entry.Name())
		}
	}
}

// TestLocalObjectStore_SymlinksAreNeverFollowed pins the local backend's
// integrity rule: a symbolic link anywhere on a key's path is treated as a
// breach of the store's root rather than as routing. Getting through one is
// reported as not found (the key is not addressable in the store's tree);
// putting or deleting through one is refused outright; a link at the final
// path is replaced by a put and left alone by a delete, never written
// through.
func TestLocalObjectStore_SymlinksAreNeverFollowed(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	writeVictim := func() {
		t.Helper()
		if err := os.WriteFile(victim, []byte("outside secret"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	readVictim := func() string {
		t.Helper()
		body, err := os.ReadFile(victim)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	t.Run("a link at the final path is not read through", func(t *testing.T) {
		writeVictim()
		root, store := newRootStore(t)
		if err := os.Symlink(victim, filepath.Join(root, "linked")); err != nil {
			t.Fatal(err)
		}

		if _, err := store.GetObject(context.Background(), "linked"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("GetObject() through a link error = %v, want ErrObjectNotFound", err)
		}
		if got := readVictim(); got != "outside secret" {
			t.Errorf("the link target was read through: victim reads %q", got)
		}
	})

	t.Run("a put replaces a final link instead of writing through it", func(t *testing.T) {
		writeVictim()
		root, store := newRootStore(t)
		if err := os.Symlink(victim, filepath.Join(root, "linked")); err != nil {
			t.Fatal(err)
		}

		putObject(t, store, "linked", "replacement")
		if got := readVictim(); got != "outside secret" {
			t.Errorf("the put wrote through the link: victim reads %q", got)
		}
		if got := getObject(t, store, "linked"); got != "replacement" {
			t.Errorf("after the put the key reads %q, want %q", got, "replacement")
		}
	})

	t.Run("a delete leaves a final link alone", func(t *testing.T) {
		writeVictim()
		root, store := newRootStore(t)
		if err := os.Symlink(victim, filepath.Join(root, "linked")); err != nil {
			t.Fatal(err)
		}

		if err := store.DeleteObject(context.Background(), "linked"); err != nil {
			t.Errorf("DeleteObject() over a final link error = %v, want nil", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "linked")); err != nil {
			t.Errorf("the link was removed: %v", err)
		}
		if got := readVictim(); got != "outside secret" {
			t.Errorf("the delete wrote through the link: victim reads %q", got)
		}
	})

	t.Run("a link in the middle is never followed", func(t *testing.T) {
		writeVictim()
		root, store := newRootStore(t)
		// A directory holds an object; an attacker then swaps the directory
		// for a link to the outside directory that holds the victim.
		putObject(t, store, "dir/obj", "original")
		if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
			t.Fatal(err)
		}

		if _, err := store.GetObject(context.Background(), "dir/obj"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("GetObject() through a middle link error = %v, want ErrObjectNotFound", err)
		}
		err := store.PutObject(context.Background(), "dir/obj", strings.NewReader("x"))
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("PutObject() through a middle link error = %v, want a refusal naming the symbolic link", err)
		}
		err = store.DeleteObject(context.Background(), "dir/obj")
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("DeleteObject() through a middle link error = %v, want a refusal naming the symbolic link", err)
		}
		if got := readVictim(); got != "outside secret" {
			t.Errorf("the operations reached outside the root: victim reads %q", got)
		}
	})
}

// TestLocalObjectStore_ContextCancellation pins the context contract on
// every operation: one that begins on a cancelled context does nothing but
// return the context's error, a PutObject cancelled mid-stream stores
// nothing, and a GetObject reader whose context is cancelled fails its reads.
func TestLocalObjectStore_ContextCancellation(t *testing.T) {
	_, store := newRootStore(t)

	t.Run("an operation that begins cancelled returns the context's error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := store.PutObject(ctx, "k", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
			t.Errorf("PutObject() error = %v, want context.Canceled", err)
		}
		reader, err := store.GetObject(ctx, "k")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("GetObject() error = %v, want context.Canceled", err)
		}
		if reader != nil {
			reader.Close()
			t.Error("GetObject() returned a reader alongside the error, want nil")
		}
		if err := store.DeleteObject(ctx, "k"); !errors.Is(err, context.Canceled) {
			t.Errorf("DeleteObject() error = %v, want context.Canceled", err)
		}
		if _, err := store.GetObject(context.Background(), "k"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("the cancelled operations stored something: GetObject() error = %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("a put cancelled mid-stream stores nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancelAfterRead{cancel: cancel}

		err := store.PutObject(ctx, "k", reader)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PutObject() error = %v, want context.Canceled", err)
		}
		if _, err := store.GetObject(context.Background(), "k"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("a cancelled put left an object behind: GetObject() error = %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("a reader fails its reads once its context is cancelled", func(t *testing.T) {
		putObject(t, store, "k", "payload")
		ctx, cancel := context.WithCancel(context.Background())
		reader, err := store.GetObject(ctx, "k")
		if err != nil {
			t.Fatalf("GetObject() error = %v, want nil", err)
		}
		defer reader.Close()

		cancel()
		buffer := make([]byte, 8)
		if _, err := reader.Read(buffer); !errors.Is(err, context.Canceled) {
			t.Errorf("Read() after cancellation error = %v, want context.Canceled", err)
		}
		// The cancelled read does not disturb the object: a fresh request
		// with a live context reads it whole.
		if got := getObject(t, store, "k"); got != "payload" {
			t.Errorf("the object reads %q, want %q", got, "payload")
		}
	})
}

// cancelAfterRead cancels the context the upload runs under after its first
// chunk, then keeps supplying data, so the copy loop notices the cancellation
// at its next check rather than through a failing read.
type cancelAfterRead struct {
	cancel context.CancelFunc
	done   bool
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		r.cancel()
	}
	return copy(p, "payload"), nil
}

// TestLocalObjectStore_ConcurrentUse_IsRaceFree hammers one store from many
// goroutines -- writers on their own keys, a pair of writers and a deleter on
// a shared key -- so the race detector can attest that the store is safe for
// concurrent use, and checks that every object reads back whole afterwards.
func TestLocalObjectStore_ConcurrentUse_IsRaceFree(t *testing.T) {
	root, store := newRootStore(t)
	ctx := context.Background()
	const writers = 8

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := fmt.Sprintf("writer/%d/blob", w)
			body := strings.Repeat(fmt.Sprintf("body of writer %d; ", w), 64)
			for range 25 {
				if err := store.PutObject(ctx, key, strings.NewReader(body)); err != nil {
					t.Errorf("PutObject(%q) error = %v, want nil", key, err)
					return
				}
			}
		}(w)
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.Repeat("shared; ", 32)
			for range 25 {
				if err := store.PutObject(ctx, "shared", strings.NewReader(body)); err != nil {
					t.Errorf("PutObject(shared) error = %v, want nil", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 25 {
			// Deleting a key concurrent writers are putting is exercised for
			// the race detector; DeleteObject must never fail on the way.
			if err := store.DeleteObject(ctx, "shared"); err != nil {
				t.Errorf("DeleteObject(shared) error = %v, want nil", err)
				return
			}
		}
	}()
	wg.Wait()

	for w := range writers {
		want := strings.Repeat(fmt.Sprintf("body of writer %d; ", w), 64)
		if got := getObject(t, store, fmt.Sprintf("writer/%d/blob", w)); got != want {
			t.Errorf("writer %d's object read back %d bytes, want %d", w, len(got), len(want))
		}
	}
	// Whatever the racing writers and deleter left behind, one final put and
	// read settles the key.
	final := strings.Repeat("final; ", 8)
	putObject(t, store, "shared", final)
	if got := getObject(t, store, "shared"); got != final {
		t.Errorf("shared object read back %q, want %q", got, final)
	}

	// No upload ever left a temporary file behind.
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), ".pkgcore-object-") {
			t.Errorf("a temporary file survived at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLocalObjectStore_ConcurrentPut_AllKeysUnderOneFreshPrefix is the
// regression test for the creation race inside PutObject's directory walk.
// Writers are released by a barrier and put distinct keys under one prefix
// that does not exist yet, so their Mkdir of the shared prefix segment
// collides: exactly one Mkdir wins, and every loser sees EEXIST from its own.
// A loser must then re-inspect the path and descend into the directory the
// winner created. A PutObject that instead kept walking from the stale parent
// stores the loser's object under the wrong key -- the raced prefix segment
// silently dropped from the path -- while returning nil, and the requested
// key reads back ErrObjectNotFound afterwards. A fresh prefix per round makes
// every round re-enter the race, and every object must read back whole under
// its requested key once the round's writers are done.
func TestLocalObjectStore_ConcurrentPut_AllKeysUnderOneFreshPrefix(t *testing.T) {
	_, store := newRootStore(t)
	ctx := context.Background()
	const writers = 16
	const rounds = 8

	for round := range rounds {
		prefix := fmt.Sprintf("burst-%d", round)
		start := make(chan struct{})
		bodies := make([]string, writers)

		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				body := strings.Repeat(fmt.Sprintf("body %d; ", w), 64)
				bodies[w] = body
				if err := store.PutObject(ctx, fmt.Sprintf("%s/w-%d/blob", prefix, w), strings.NewReader(body)); err != nil {
					t.Errorf("round %d: PutObject(w-%d) error = %v, want nil", round, w, err)
				}
			}(w)
		}
		close(start)
		wg.Wait()

		for w := range writers {
			if got := getObject(t, store, fmt.Sprintf("%s/w-%d/blob", prefix, w)); got != bodies[w] {
				t.Errorf("round %d: writer %d's object read back %d bytes, want %d", round, w, len(got), len(bodies[w]))
			}
		}
	}
}

// TestNewLocalObjectStore_PanicsOnAnUnusableDirectory mirrors the
// NewSMTPMailer wiring-error panic: an object store whose root can never work
// is an unrecoverable error at startup, and a panic there is where the wiring
// error is visible.
func TestNewLocalObjectStore_PanicsOnAnUnusableDirectory(t *testing.T) {
	t.Run("an empty directory", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("NewLocalObjectStore(\"\") did not panic, want a panic for an unusable root")
			}
		}()
		NewLocalObjectStore("")
	})

	t.Run("a path that is a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if recover() == nil {
				t.Error("NewLocalObjectStore(file) did not panic, want a panic for an unusable root")
			}
		}()
		NewLocalObjectStore(path)
	})
}
