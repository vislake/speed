package pkgcore

import (
	"testing"
)

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
