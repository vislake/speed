package pkgcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
