package s3

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
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
		{"an unknown bucket lookup", build(func(cfg *Config) { cfg.BucketLookup = BucketLookupType(99) })},
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

// TestMinioBucketLookup_MapsEveryEnumValue pins the enum-to-minio mapping
// newObjectStore addresses buckets through: auto and path keep their
// meaning, virtual host maps onto minio-go's DNS constant (that client's
// name for the virtual-hosted style), and a value outside the enum reports
// not-ok rather than silently selecting a style. The zero-value check is
// the compatibility half: a Config that never sets the field keeps the
// endpoint-derived behavior.
func TestMinioBucketLookup_MapsEveryEnumValue(t *testing.T) {
	if got := (Config{}).BucketLookup; got != BucketLookupAuto {
		t.Errorf("the zero Config.BucketLookup = %d, want BucketLookupAuto", got)
	}

	tests := []struct {
		in     BucketLookupType
		want   minio.BucketLookupType
		wantOK bool
	}{
		{BucketLookupAuto, minio.BucketLookupAuto, true},
		{BucketLookupPath, minio.BucketLookupPath, true},
		{BucketLookupVirtualHost, minio.BucketLookupDNS, true},
		{BucketLookupType(-1), minio.BucketLookupAuto, false},
		{BucketLookupType(99), minio.BucketLookupAuto, false},
	}
	for _, tt := range tests {
		got, ok := minioBucketLookup(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("minioBucketLookup(%d) = (%d, %t), want (%d, %t)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// captureTransport is the stand-in http.RoundTripper the bucket-lookup
// line-shape test injects through newObjectStore's transport seam: it
// records the URL of every request it is handed and then aborts the request
// with abortRequest, so no dial is ever attempted.
type captureTransport struct {
	urls []string
}

func (tr *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.urls = append(tr.urls, req.URL.String())
	return nil, abortRequest
}

// abortRequest is the sentinel captureTransport returns: deliberately an
// x509.UnknownAuthorityError, the one transport error minio-go classifies
// as non-retryable (retry.go's isRequestErrorRetryable checks this type),
// so an operation issues exactly one request -- the URL under test --
// instead of the ten with back-off a generic error draws.
var abortRequest = x509.UnknownAuthorityError{}

// TestObjectStore_AddressesTheBucketInTheConfiguredLookupStyle pins the
// wire shape each BucketLookup mode produces, as the URL of the request the
// store actually issues: a path segment under BucketLookupAuto and
// BucketLookupPath (host/bucket/key), the host's first label under
// BucketLookupVirtualHost (bucket.host/key). The endpoint is a
// self-hosted shape no service-specific detection recognizes, which is what
// makes auto's path-style outcome assertable, and the dotted-bucket-on-HTTPS
// row pins the other half of the auto rule. GetObject issues its request
// eagerly, so one call is one captured URL -- asserted to be exactly one, so
// a retry classification change cannot quietly turn this into a
// ten-request, back-off-slowed test.
func TestObjectStore_AddressesTheBucketInTheConfiguredLookupStyle(t *testing.T) {
	tests := []struct {
		name    string
		lookup  BucketLookupType
		useSSL  bool
		bucket  string
		wantURL string
	}{
		{
			name:    "auto on a self-hosted endpoint is path style",
			lookup:  BucketLookupAuto,
			bucket:  "objects",
			wantURL: "http://s3.example.com:9000/objects/some/key",
		},
		{
			name:    "auto is path style for a dotted bucket on HTTPS",
			lookup:  BucketLookupAuto,
			useSSL:  true,
			bucket:  "objects.data",
			wantURL: "https://s3.example.com:9000/objects.data/some/key",
		},
		{
			name:    "path addresses the bucket as a path segment",
			lookup:  BucketLookupPath,
			bucket:  "objects",
			wantURL: "http://s3.example.com:9000/objects/some/key",
		},
		{
			name:    "virtual host addresses the bucket as the host's first label",
			lookup:  BucketLookupVirtualHost,
			bucket:  "objects",
			wantURL: "http://objects.s3.example.com:9000/some/key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := configForTests()
			cfg.Endpoint = "s3.example.com:9000"
			cfg.Bucket = tt.bucket
			cfg.UseSSL = tt.useSSL
			cfg.BucketLookup = tt.lookup
			// A region-pinned client skips minio-go's bucket-location probe,
			// so the one request the operation issues is the object request
			// whose URL is under test rather than a location query before it.
			cfg.Region = "us-east-1"

			transport := &captureTransport{}
			store, err := newObjectStore(cfg, transport)
			if err != nil {
				t.Fatalf("newObjectStore(%+v) error = %v, want nil", cfg, err)
			}

			// The abort error is the expected outcome: reaching the transport
			// with the URL under test is the point, and the store reporting
			// the failure eagerly is the behavior its own tests pin.
			if _, err := store.GetObject(context.Background(), "some/key"); err == nil {
				t.Fatal("GetObject() error = nil, want the transport's abort error")
			}
			if len(transport.urls) != 1 {
				t.Fatalf("one GetObject issued %d requests %v, want exactly 1", len(transport.urls), transport.urls)
			}
			if got := transport.urls[0]; got != tt.wantURL {
				t.Errorf("request URL = %q, want %q", got, tt.wantURL)
			}
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
