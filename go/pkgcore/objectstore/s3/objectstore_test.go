package s3

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/vislake/speed/go/pkgcore"
)

// configForTests names a bucket on the closed loopback port, so a test can
// build an S3 store that never reaches a service. The grammar and context
// checks run before an operation touches the client, so the hermetic tests
// below exercise those without a network; an operation that does reach the
// client fails on the refused connection instead of hanging.
func configForTests() Config {
	return Config{
		Endpoint:  "127.0.0.1:1",
		Bucket:    "objects",
		AccessKey: "access",
		SecretKey: "secret",
	}
}

// TestNewObjectStore_PanicsOnAnUnusableConfiguration mirrors pkgcore's
// NewLocalObjectStore and NewSMTPMailer wiring-error panics: an S3 store
// whose configuration can never reach a bucket is an unrecoverable error at
// startup, and a panic there is where the wiring error is visible.
func TestNewObjectStore_PanicsOnAnUnusableConfiguration(t *testing.T) {
	build := func(mutate func(*Config)) func() {
		return func() {
			cfg := configForTests()
			mutate(&cfg)
			NewObjectStore(cfg)
		}
	}
	tests := []struct {
		name   string
		panics func() // the constructors under test must all panic
	}{
		{"an empty endpoint", build(func(cfg *Config) { cfg.Endpoint = "" })},
		{"an empty bucket", build(func(cfg *Config) { cfg.Bucket = "" })},
		{"an empty access key", build(func(cfg *Config) { cfg.AccessKey = "" })},
		{"an empty secret key", build(func(cfg *Config) { cfg.SecretKey = "" })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("NewObjectStore did not panic, want a panic for an unusable configuration")
				}
			}()
			tt.panics()
		})
	}
}

// TestObjectStore_CancelledContextDoesNotDial pins that an operation which
// begins on a cancelled context returns the context's error without touching
// the service, so a store whose service is down or absent still honours
// cancellation before any dial could fail or hang.
func TestObjectStore_CancelledContextDoesNotDial(t *testing.T) {
	store := NewObjectStore(configForTests())
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
}

// TestObjectStore_UnreachableService_ReportsAWrappedError pins what a store
// whose service cannot be reached reports: an ordinary error wrapped with the
// operation's context, never a misclassified pkgcore.ErrObjectNotFound and
// never a hang. GetObject must report the failure itself, eagerly, rather
// than returning a reader that fails on its first read.
func TestObjectStore_UnreachableService_ReportsAWrappedError(t *testing.T) {
	store := NewObjectStore(configForTests())
	ctx := context.Background()

	err := store.PutObject(ctx, "k", strings.NewReader("x"))
	if err == nil {
		t.Fatal("PutObject() error = nil, want the refused connection wrapped")
	}
	if !strings.Contains(err.Error(), "pkgcore/objectstore/s3: put") {
		t.Errorf("PutObject() error = %q, want it to name the operation", err)
	}
	if errors.Is(err, pkgcore.ErrObjectNotFound) || errors.Is(err, pkgcore.ErrInvalidObjectKey) {
		t.Errorf("PutObject() error = %v, want no sentinel for an unreachable service", err)
	}

	reader, err := store.GetObject(ctx, "k")
	if err == nil {
		t.Fatal("GetObject() error = nil, want the refused connection wrapped")
	}
	if !strings.Contains(err.Error(), "pkgcore/objectstore/s3: get") {
		t.Errorf("GetObject() error = %q, want it to name the operation", err)
	}
	if errors.Is(err, pkgcore.ErrObjectNotFound) || errors.Is(err, pkgcore.ErrInvalidObjectKey) {
		t.Errorf("GetObject() error = %v, want no sentinel for an unreachable service", err)
	}
	if reader != nil {
		reader.Close()
		t.Error("GetObject() returned a reader alongside the error, want nil")
	}

	err = store.DeleteObject(ctx, "k")
	if err == nil {
		t.Fatal("DeleteObject() error = nil, want the refused connection wrapped")
	}
	if !strings.Contains(err.Error(), "pkgcore/objectstore/s3: delete") {
		t.Errorf("DeleteObject() error = %q, want it to name the operation", err)
	}
	if errors.Is(err, pkgcore.ErrObjectNotFound) || errors.Is(err, pkgcore.ErrInvalidObjectKey) {
		t.Errorf("DeleteObject() error = %v, want no sentinel for an unreachable service", err)
	}
}

// TestObjectStore_RejectsAnInvalidKeyWithoutDialing pins the shared grammar
// pkgcore.ValidateObjectKey enforces, at this store's interface level:
// pkgcore's own objectstore_test.go pins the identical table against the
// local store, and cannot pin it against this store too without importing
// this package back into pkgcore, which would cycle. The checks run before
// an operation touches the backend, so an invalid key is rejected against a
// closed port without a single dial. No key value is echoed in the error
// text, so the message cannot leak key-shaped data.
func TestObjectStore_RejectsAnInvalidKeyWithoutDialing(t *testing.T) {
	store := NewObjectStore(configForTests())
	operations := map[string]func(pkgcore.ObjectStore, string) error{
		"put": func(store pkgcore.ObjectStore, key string) error {
			return store.PutObject(context.Background(), key, strings.NewReader("x"))
		},
		"get": func(store pkgcore.ObjectStore, key string) error {
			_, err := store.GetObject(context.Background(), key)
			return err
		},
		"delete": func(store pkgcore.ObjectStore, key string) error {
			return store.DeleteObject(context.Background(), key)
		},
	}

	for _, key := range []string{
		"", "/", "a/", "/a", "a//b", ".", "..", "a/./b", "a/../b",
		"a\\b", "a\x00b", strings.Repeat("a", 256),
	} {
		for operationName, operation := range operations {
			err := operation(store, key)
			if !errors.Is(err, pkgcore.ErrInvalidObjectKey) {
				t.Errorf("%s(%q) error = %v, want it to wrap ErrInvalidObjectKey", operationName, key, err)
			}
			switch key {
			case "", "/", ".", "..":
			default:
				if strings.Contains(err.Error(), key) {
					t.Errorf("%s error text %q echoes the key", operationName, err)
				}
			}
		}
	}
}

// TestIsObjectNotFound pins the error mapping that turns the S3 NoSuchKey
// error onto pkgcore.ErrObjectNotFound: minio-go can wrap the response, so
// the check must walk the chain, and no other S3 error may map onto the
// sentinel.
func TestIsObjectNotFound(t *testing.T) {
	noSuchKey := minio.ErrorResponse{Code: "NoSuchKey"}
	denied := minio.ErrorResponse{Code: "AccessDenied"}

	if !isObjectNotFound(noSuchKey) {
		t.Error("isObjectNotFound(NoSuchKey) = false, want true")
	}
	if !isObjectNotFound(fmt.Errorf("pkgcore/objectstore/s3: wrapper: %w", noSuchKey)) {
		t.Error("isObjectNotFound(wrapped NoSuchKey) = false, want it to walk the error chain")
	}
	if isObjectNotFound(denied) {
		t.Error("isObjectNotFound(AccessDenied) = true, want false")
	}
	if isObjectNotFound(errors.New("pkgcore/objectstore/s3: something else")) {
		t.Error("isObjectNotFound(plain error) = true, want false")
	}
}
