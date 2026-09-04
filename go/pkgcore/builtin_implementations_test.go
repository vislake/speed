package pkgcore

import (
	"errors"
	"strings"
	"testing"
)

// TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName pins the name this
// file registers directly, "eventbus.memory", and the Capability it must
// declare for Kernel.Bootstrap's validation to mean anything.
// "eventbus.redis" is registered by the eventbus/redis subpackage's own
// init(), not here; that subpackage's own register_test.go proves its name
// resolves the same way, and this test binary's example_test.go blank-imports
// it so the shared EventBusRegistry this file also populates carries it too
// by the time any test in this package runs.
func TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName(t *testing.T) {
	impl, caps, err := EventBusRegistry.Build("eventbus.memory", Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "eventbus.memory", err)
	}
	if impl == nil {
		t.Error("Build(\"eventbus.memory\") returned a nil EventBus")
	}
	if caps != 0 {
		t.Errorf("Build(%q) capabilities = %v, want 0", "eventbus.memory", caps)
	}
}

// TestBuiltinKVStoreRegistry_ResolvesEveryDocumentedName mirrors
// TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName for "kv.memory";
// "kv.redis" is the kv/redis subpackage's own concern, per the same note.
func TestBuiltinKVStoreRegistry_ResolvesEveryDocumentedName(t *testing.T) {
	impl, caps, err := KVStoreRegistry.Build("kv.memory", Config{})
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "kv.memory", err)
	}
	if impl == nil {
		t.Error("Build(\"kv.memory\") returned a nil KVStore")
	}
	if caps != 0 {
		t.Errorf("Build(%q) capabilities = %v, want 0", "kv.memory", caps)
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
	if caps != Stateless {
		t.Errorf("Build(%q) capabilities = %v, want Stateless", "mailer.console", caps)
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

// TestBuiltinObjectStoreRegistry_ResolvesEveryDocumentedName pins the name
// this file registers directly, "objectstore.local". "objectstore.s3" is the
// objectstore/s3 subpackage's own concern, per the note on
// TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName.
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
