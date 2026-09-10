package s3

import (
	"context"
	"errors"
	"testing"
)

// TestFromConfig_BuildsTheStoreAndDeclaresTheCapabilities drives the
// one-step contract of the bare-injection path: FromConfig alone yields a
// working store together with this package's declared capabilities, so the
// injection pair pkgcore.WithObjectStore takes comes out of one call. The
// store's own context check runs before any request, so a cancelled context
// fails an operation with the context's error -- the usable-store proof
// that needs no service, matching the store's own hermetic unit tests.
func TestFromConfig_BuildsTheStoreAndDeclaresTheCapabilities(t *testing.T) {
	store, caps, err := FromConfig(configForTests())
	if err != nil {
		t.Fatalf("FromConfig(%+v) error = %v, want nil", configForTests(), err)
	}
	if store == nil {
		t.Fatal("FromConfig returned a nil store")
	}
	if caps != Capabilities {
		t.Errorf("FromConfig capabilities = %v, want the exported Capabilities constant %v", caps, Capabilities)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.GetObject(ctx, "some/key")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("GetObject on a cancelled context error = %v, want context.Canceled", err)
	}
}

// TestFromConfig_MissingFieldReturnsError pins each individually-missing
// field as an error, never the panic NewObjectStore's own unusable-wiring
// convention would raise for the same configuration: the caller assembling
// a Kernel's options can still abandon them, so the judgment comes back as
// a value. The failure path reports no store and no capability, so a caller
// that ignored the error cannot wire a half-built pair.
func TestFromConfig_MissingFieldReturnsError(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing everything", mutate: func(cfg *Config) { *cfg = Config{} }},
		{name: "missing endpoint", mutate: func(cfg *Config) { cfg.Endpoint = "" }},
		{name: "missing bucket", mutate: func(cfg *Config) { cfg.Bucket = "" }},
		{name: "missing access key", mutate: func(cfg *Config) { cfg.AccessKey = "" }},
		{name: "missing secret key", mutate: func(cfg *Config) { cfg.SecretKey = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := configForTests()
			tt.mutate(&cfg)

			store, caps, err := FromConfig(cfg)
			if err == nil {
				t.Fatal("FromConfig() error = nil, want an error for the missing field")
			}
			if store != nil {
				t.Errorf("FromConfig() store = %v, want nil on the failure path", store)
			}
			if caps != 0 {
				t.Errorf("FromConfig() capabilities = %v, want 0 on the failure path", caps)
			}
		})
	}
}

// TestFromConfig_EndpointMinioRejectsReturnsError pins the second unusable
// shape as an error too: an endpoint minio-go itself rejects at
// construction -- a scheme-carrying address, where this package's
// convention is a scheme-less host or host:port with UseSSL deciding the
// transport. The same configuration still panics NewObjectStore, whose
// contract stays the unrecoverable-wiring panic; FromConfig's error is the
// point of its own shape, not a change to that one.
func TestFromConfig_EndpointMinioRejectsReturnsError(t *testing.T) {
	cfg := configForTests()
	cfg.Endpoint = "https://minio.example.com"

	store, caps, err := FromConfig(cfg)
	if err == nil {
		t.Fatal("FromConfig() with a scheme-carrying endpoint error = nil, want the minio-go rejection back as an error")
	}
	if store != nil {
		t.Errorf("FromConfig() store = %v, want nil on the failure path", store)
	}
	if caps != 0 {
		t.Errorf("FromConfig() capabilities = %v, want 0 on the failure path", caps)
	}

	defer func() {
		if recover() == nil {
			t.Error("NewObjectStore did not panic on the configuration FromConfig rejects, want its documented panic preserved")
		}
	}()
	NewObjectStore(cfg)
}
