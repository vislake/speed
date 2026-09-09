package pkgcore

import (
	"testing"
)

// TestBuiltinEventBusRegistry_ResolvesEveryDocumentedName pins the name
// eventbus_builtins.go registers directly, "eventbus.memory", and the
// Capability it must declare for Kernel.Bootstrap's validation to mean
// anything. "eventbus.redis" is registered by the eventbus/redis
// subpackage's own init(), not there; that subpackage's own register_test.go
// proves its name resolves the same way, and this test binary's
// example_test.go blank-imports it so the shared EventBusRegistry the
// built-in registration files populate carries it too by the time any test in
// this package runs.
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
