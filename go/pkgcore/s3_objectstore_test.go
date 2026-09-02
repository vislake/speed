package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// s3ConfigForTests names a bucket on the closed loopback port, so a test can
// build an S3 store that never reaches a service. The grammar and context
// checks run before an operation touches the client, so the hermetic tests
// below exercise those without a network; an operation that does reach the
// client fails on the refused connection instead of hanging.
func s3ConfigForTests() S3Config {
	return S3Config{
		Endpoint:  "127.0.0.1:1",
		Bucket:    "objects",
		AccessKey: "access",
		SecretKey: "secret",
	}
}

// TestNewS3ObjectStore_PanicsOnAnUnusableConfiguration mirrors the
// NewLocalObjectStore and NewSMTPMailer wiring-error panics: an S3 store
// whose configuration can never reach a bucket is an unrecoverable error at
// startup, and a panic there is where the wiring error is visible.
func TestNewS3ObjectStore_PanicsOnAnUnusableConfiguration(t *testing.T) {
	build := func(mutate func(*S3Config)) func() {
		return func() {
			cfg := s3ConfigForTests()
			mutate(&cfg)
			NewS3ObjectStore(cfg)
		}
	}
	tests := []struct {
		name   string
		panics func() // the constructors under test must all panic
	}{
		{"an empty endpoint", build(func(cfg *S3Config) { cfg.Endpoint = "" })},
		{"an empty bucket", build(func(cfg *S3Config) { cfg.Bucket = "" })},
		{"an empty access key", build(func(cfg *S3Config) { cfg.AccessKey = "" })},
		{"an empty secret key", build(func(cfg *S3Config) { cfg.SecretKey = "" })},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("NewS3ObjectStore did not panic, want a panic for an unusable configuration")
				}
			}()
			tt.panics()
		})
	}
}

// TestS3ObjectStore_CancelledContextDoesNotDial pins that an operation which
// begins on a cancelled context returns the context's error without touching
// the service, so a store whose service is down or absent still honours
// cancellation before any dial could fail or hang.
func TestS3ObjectStore_CancelledContextDoesNotDial(t *testing.T) {
	store := NewS3ObjectStore(s3ConfigForTests())
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

// TestS3ObjectStore_UnreachableService_ReportsAWrappedError pins what a store
// whose service cannot be reached reports: an ordinary error wrapped with the
// operation's context, never a misclassified ErrObjectNotFound and never a
// hang. GetObject must report the failure itself, eagerly, rather than
// returning a reader that fails on its first read.
func TestS3ObjectStore_UnreachableService_ReportsAWrappedError(t *testing.T) {
	store := NewS3ObjectStore(s3ConfigForTests())
	ctx := context.Background()

	err := store.PutObject(ctx, "k", strings.NewReader("x"))
	if err == nil {
		t.Fatal("PutObject() error = nil, want the refused connection wrapped")
	}
	if !strings.Contains(err.Error(), "s3 object store put") {
		t.Errorf("PutObject() error = %q, want it to name the operation", err)
	}
	if errors.Is(err, ErrObjectNotFound) || errors.Is(err, ErrInvalidObjectKey) {
		t.Errorf("PutObject() error = %v, want no sentinel for an unreachable service", err)
	}

	reader, err := store.GetObject(ctx, "k")
	if err == nil {
		t.Fatal("GetObject() error = nil, want the refused connection wrapped")
	}
	if !strings.Contains(err.Error(), "s3 object store get") {
		t.Errorf("GetObject() error = %q, want it to name the operation", err)
	}
	if errors.Is(err, ErrObjectNotFound) || errors.Is(err, ErrInvalidObjectKey) {
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
	if !strings.Contains(err.Error(), "s3 object store delete") {
		t.Errorf("DeleteObject() error = %q, want it to name the operation", err)
	}
	if errors.Is(err, ErrObjectNotFound) || errors.Is(err, ErrInvalidObjectKey) {
		t.Errorf("DeleteObject() error = %v, want no sentinel for an unreachable service", err)
	}
}

// TestIsObjectNotFound pins the error mapping that turns the S3 NoSuchKey
// error onto ErrObjectNotFound: minio-go can wrap the response, so the check
// must walk the chain, and no other S3 error may map onto the sentinel.
func TestIsObjectNotFound(t *testing.T) {
	noSuchKey := minio.ErrorResponse{Code: "NoSuchKey"}
	denied := minio.ErrorResponse{Code: "AccessDenied"}

	if !isObjectNotFound(noSuchKey) {
		t.Error("isObjectNotFound(NoSuchKey) = false, want true")
	}
	if !isObjectNotFound(fmt.Errorf("pkgcore: wrapper: %w", noSuchKey)) {
		t.Error("isObjectNotFound(wrapped NoSuchKey) = false, want it to walk the error chain")
	}
	if isObjectNotFound(denied) {
		t.Error("isObjectNotFound(AccessDenied) = true, want false")
	}
	if isObjectNotFound(errors.New("pkgcore: something else")) {
		t.Error("isObjectNotFound(plain error) = true, want false")
	}
}
