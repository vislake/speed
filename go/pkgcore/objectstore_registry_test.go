package pkgcore

import (
	"strings"
	"testing"
)

// TestBuiltinObjectStoreRegistry_ResolvesEveryDocumentedName pins the name
// objectstore_registry.go registers directly, "objectstore.local".
// "objectstore.s3" is the objectstore/s3 subpackage's own concern, per the
// note on TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName.
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
// the throwaway-by-default behaviour: an empty cfg["directory"] must not
// fail, and must still produce a usable store.
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
