package pkgcore

import (
	"errors"
	"strings"
	"testing"
)

// TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName pins the two names
// docs/internal/03-deployment-modes.md and this round's plan document as
// pkgcore's built-in EventBus implementations, and the Capability each one
// must declare for Kernel.Bootstrap's validation to mean anything.
func TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		wantCaps Capability
	}{
		{name: "eventbus.memory", cfg: Config{}, wantCaps: 0},
		{name: "eventbus.redis", cfg: Config{}, wantCaps: MultiReplicaSafe | SurvivesRestart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			impl, caps, err := EventBusRegistry.Build(tt.name, tt.cfg)
			if err != nil {
				t.Fatalf("Build(%q) error = %v, want nil", tt.name, err)
			}
			if impl == nil {
				t.Errorf("Build(%q) returned a nil EventBus", tt.name)
			}
			if caps != tt.wantCaps {
				t.Errorf("Build(%q) capabilities = %v, want %v", tt.name, caps, tt.wantCaps)
			}
		})
	}
}

func TestBuiltinKVStoreRegistry_ResolvesEveryDocumentedName(t *testing.T) {
	tests := []struct {
		name     string
		wantCaps Capability
	}{
		{name: "kv.memory", wantCaps: 0},
		{name: "kv.redis", wantCaps: MultiReplicaSafe | SurvivesRestart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			impl, caps, err := KVStoreRegistry.Build(tt.name, Config{})
			if err != nil {
				t.Fatalf("Build(%q) error = %v, want nil", tt.name, err)
			}
			if impl == nil {
				t.Errorf("Build(%q) returned a nil KVStore", tt.name)
			}
			if caps != tt.wantCaps {
				t.Errorf("Build(%q) capabilities = %v, want %v", tt.name, caps, tt.wantCaps)
			}
		})
	}
}

func TestBuiltinMailerRegistry_ResolvesEveryDocumentedName(t *testing.T) {
	impl, caps, err := MailerRegistry.Build("mailer.console", Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "mailer.console", err)
	}
	if impl == nil {
		t.Error("Build(\"mailer.console\") returned a nil Mailer")
	}
	if caps != 0 {
		t.Errorf("Build(%q) capabilities = %v, want 0", "mailer.console", caps)
	}

	impl, caps, err = MailerRegistry.Build("mailer.smtp", Config{"host": "smtp.example.com"})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "mailer.smtp", err)
	}
	if impl == nil {
		t.Error("Build(\"mailer.smtp\") returned a nil Mailer")
	}
	if want := MultiReplicaSafe | SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "mailer.smtp", caps, want)
	}
}

func TestBuiltinObjectStoreRegistry_ResolvesEveryDocumentedName(t *testing.T) {
	impl, caps, err := ObjectStoreRegistry.Build("objectstore.local", Config{"directory": t.TempDir()})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "objectstore.local", err)
	}
	if impl == nil {
		t.Error("Build(\"objectstore.local\") returned a nil ObjectStore")
	}
	if caps != 0 {
		t.Errorf("Build(%q) capabilities = %v, want 0", "objectstore.local", caps)
	}

	s3Cfg := Config{"endpoint": "s3.example.com", "bucket": "objects", "access_key": "ak", "secret_key": "sk"}
	impl, caps, err = ObjectStoreRegistry.Build("objectstore.s3", s3Cfg)
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "objectstore.s3", err)
	}
	if impl == nil {
		t.Error("Build(\"objectstore.s3\") returned a nil ObjectStore")
	}
	if want := MultiReplicaSafe | SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "objectstore.s3", caps, want)
	}
}

// TestLocalObjectStoreFromConfig_EmptyDirectoryFallsBackToATemporaryOne pins
// the throwaway-by-default behaviour the pre-retrofit Kernel's
// DeploymentModeStandalone case had for its ObjectStore: an empty
// cfg["directory"] must not fail, and must still produce a usable store.
func TestLocalObjectStoreFromConfig_EmptyDirectoryFallsBackToATemporaryOne(t *testing.T) {
	store, err := localObjectStoreFromConfig(Config{})
	if err != nil {
		t.Fatalf("localObjectStoreFromConfig(Config{}) error = %v, want nil", err)
	}
	if store == nil {
		t.Fatal("localObjectStoreFromConfig(Config{}) returned a nil store")
	}
}

// TestLocalObjectStoreFromConfig_DirectoryIsHonoured pins the other half:
// a host that names a persistent directory gets a store over that exact
// directory, so objects survive a restart -- the whole point of setting it.
func TestLocalObjectStoreFromConfig_DirectoryIsHonoured(t *testing.T) {
	dir := t.TempDir()
	store, err := localObjectStoreFromConfig(Config{"directory": dir})
	if err != nil {
		t.Fatalf("localObjectStoreFromConfig() error = %v, want nil", err)
	}

	if putErr := store.PutObject(t.Context(), "k", strings.NewReader("v")); putErr != nil {
		t.Fatalf("PutObject() error = %v, want nil", putErr)
	}
	// A second store opened over the same directory must read back what the
	// first one wrote: proof the directory was actually honoured, not routed
	// somewhere else (a fresh temporary directory, for instance).
	second, err := localObjectStoreFromConfig(Config{"directory": dir})
	if err != nil {
		t.Fatalf("localObjectStoreFromConfig() (second open) error = %v, want nil", err)
	}
	reader, err := second.GetObject(t.Context(), "k")
	if err != nil {
		t.Fatalf("GetObject() error = %v, want nil", err)
	}
	defer reader.Close()
}

func TestSMTPMailerFromConfig_MissingHostReturnsErrMissingSeamConfig(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{})
	if !errors.Is(err, ErrMissingSeamConfig) {
		t.Fatalf("smtpMailerFromConfig(Config{}) error = %v, want it to wrap ErrMissingSeamConfig", err)
	}
}

func TestSMTPMailerFromConfig_InvalidTLSModeReturnsError(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{"host": "smtp.example.com", "tls_mode": "quantum"})
	if err == nil {
		t.Fatal("smtpMailerFromConfig() with an invalid tls_mode succeeded, want an error")
	}
}

func TestSMTPMailerFromConfig_InvalidPortReturnsError(t *testing.T) {
	_, err := smtpMailerFromConfig(Config{"host": "smtp.example.com", "port": "not-a-number"})
	if err == nil {
		t.Fatal("smtpMailerFromConfig() with an invalid port succeeded, want an error")
	}
}

func TestS3ObjectStoreFromConfig_MissingFieldReturnsErrMissingSeamConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "missing everything", cfg: Config{}},
		{name: "missing bucket", cfg: Config{"endpoint": "s3.example.com", "access_key": "ak", "secret_key": "sk"}},
		{name: "missing access_key", cfg: Config{"endpoint": "s3.example.com", "bucket": "objects", "secret_key": "sk"}},
		{name: "missing secret_key", cfg: Config{"endpoint": "s3.example.com", "bucket": "objects", "access_key": "ak"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s3ObjectStoreFromConfig(tt.cfg)
			if !errors.Is(err, ErrMissingSeamConfig) {
				t.Fatalf("s3ObjectStoreFromConfig(%v) error = %v, want it to wrap ErrMissingSeamConfig", tt.cfg, err)
			}
		})
	}
}

func TestRedisClientFromConfig_DefaultsAddrWhenUnset(t *testing.T) {
	client, err := redisClientFromConfig(Config{})
	if err != nil {
		t.Fatalf("redisClientFromConfig(Config{}) error = %v, want nil", err)
	}
	if got := client.Options().Addr; got != "localhost:6379" {
		t.Errorf("client.Options().Addr = %q, want %q", got, "localhost:6379")
	}
}

func TestRedisClientFromConfig_InvalidDBReturnsError(t *testing.T) {
	_, err := redisClientFromConfig(Config{"db": "not-a-number"})
	if err == nil {
		t.Fatal("redisClientFromConfig() with an invalid db succeeded, want an error")
	}
}
