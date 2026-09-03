package pkgcore

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// seamRegistryTestSeam is a minimal interface exercised by these tests, kept
// distinct from any of pkgcore's real seams so the tests stay independent of
// EventBus/KVStore/Mailer/ObjectStore's own evolution.
type seamRegistryTestSeam interface {
	id() string
}

type seamRegistryTestImpl struct{ name string }

func (s seamRegistryTestImpl) id() string { return s.name }

func TestSeamRegistry_Register_DuplicateNameReturnsError(t *testing.T) {
	r := NewSeamRegistry[seamRegistryTestSeam]()

	first := Registration[seamRegistryTestSeam]{
		Name: "seam.a",
		New:  func(Config) (seamRegistryTestSeam, error) { return seamRegistryTestImpl{name: "first"}, nil },
	}
	if err := r.Register(first); err != nil {
		t.Fatalf("first Register() error = %v, want nil", err)
	}

	second := Registration[seamRegistryTestSeam]{
		Name: "seam.a",
		New:  func(Config) (seamRegistryTestSeam, error) { return seamRegistryTestImpl{name: "second"}, nil },
	}
	err := r.Register(second)
	if !errors.Is(err, ErrDuplicateImplementation) {
		t.Fatalf("second Register() error = %v, want it to wrap ErrDuplicateImplementation", err)
	}
	if !strings.Contains(err.Error(), "seam.a") {
		t.Errorf("error = %q, want it to name %q", err, "seam.a")
	}

	// The original registration must survive the rejected second call.
	impl, _, buildErr := r.Build("seam.a", Config{})
	if buildErr != nil {
		t.Fatalf("Build() error = %v, want nil", buildErr)
	}
	if impl.id() != "first" {
		t.Errorf("Build() returned %q, want the original registration's %q", impl.id(), "first")
	}
}

func TestSeamRegistry_Build_UnknownNameReturnsError(t *testing.T) {
	r := NewSeamRegistry[seamRegistryTestSeam]()

	_, caps, err := r.Build("seam.nonexistent", Config{})
	if !errors.Is(err, ErrUnknownImplementation) {
		t.Fatalf("Build() error = %v, want it to wrap ErrUnknownImplementation", err)
	}
	if !strings.Contains(err.Error(), "seam.nonexistent") {
		t.Errorf("error = %q, want it to name %q", err, "seam.nonexistent")
	}
	if caps != 0 {
		t.Errorf("Build() capabilities = %v, want 0 on failure", caps)
	}
}

func TestSeamRegistry_Build_ReturnsTheDeclaredCapabilities(t *testing.T) {
	r := NewSeamRegistry[seamRegistryTestSeam]()
	if err := r.Register(Registration[seamRegistryTestSeam]{
		Name:         "seam.durable",
		Capabilities: MultiReplicaSafe | SurvivesRestart,
		New:          func(Config) (seamRegistryTestSeam, error) { return seamRegistryTestImpl{name: "durable"}, nil },
	}); err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}

	impl, caps, err := r.Build("seam.durable", Config{})
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if impl.id() != "durable" {
		t.Errorf("Build() impl = %q, want %q", impl.id(), "durable")
	}
	if caps != MultiReplicaSafe|SurvivesRestart {
		t.Errorf("Build() capabilities = %v, want %v", caps, MultiReplicaSafe|SurvivesRestart)
	}
}

func TestSeamRegistry_Build_PassesConfigThrough(t *testing.T) {
	r := NewSeamRegistry[seamRegistryTestSeam]()
	if err := r.Register(Registration[seamRegistryTestSeam]{
		Name: "seam.configurable",
		New: func(cfg Config) (seamRegistryTestSeam, error) {
			return seamRegistryTestImpl{name: cfg["name"]}, nil
		},
	}); err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}

	impl, _, err := r.Build("seam.configurable", Config{"name": "from-config"})
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if impl.id() != "from-config" {
		t.Errorf("Build() impl = %q, want %q", impl.id(), "from-config")
	}
}

func TestSeamRegistry_Build_PropagatesConstructorError(t *testing.T) {
	r := NewSeamRegistry[seamRegistryTestSeam]()
	constructorErr := errors.New("construction failed")
	if err := r.Register(Registration[seamRegistryTestSeam]{
		Name:         "seam.broken",
		Capabilities: MultiReplicaSafe,
		New:          func(Config) (seamRegistryTestSeam, error) { return nil, constructorErr },
	}); err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}

	impl, caps, err := r.Build("seam.broken", Config{})
	if !errors.Is(err, constructorErr) {
		t.Fatalf("Build() error = %v, want it to wrap the constructor's error", err)
	}
	if impl != nil {
		t.Errorf("Build() impl = %v, want nil on a constructor failure", impl)
	}
	if caps != 0 {
		t.Errorf("Build() capabilities = %v, want 0 on a constructor failure", caps)
	}
}

// TestSeamRegistry_ConcurrentRegisterAndBuild_IsRaceFree pins the "safe for
// concurrent Register and Build calls" contract SeamRegistry's own doc
// comment makes: builtin_implementations.go builds every package-level
// registry once at init time, but a host is free to Register its own
// implementation at any point afterwards, concurrently with Kernel.Bootstrap
// calls already resolving other names on the same registry.
func TestSeamRegistry_ConcurrentRegisterAndBuild_IsRaceFree(t *testing.T) {
	r := NewSeamRegistry[seamRegistryTestSeam]()
	if err := r.Register(Registration[seamRegistryTestSeam]{
		Name: "seam.stable",
		New:  func(Config) (seamRegistryTestSeam, error) { return seamRegistryTestImpl{name: "stable"}, nil },
	}); err != nil {
		t.Fatalf("Register() error = %v, want nil", err)
	}

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("seam.dynamic-%d", i)
			if err := r.Register(Registration[seamRegistryTestSeam]{
				Name: name,
				New:  func(Config) (seamRegistryTestSeam, error) { return seamRegistryTestImpl{name: name}, nil },
			}); err != nil {
				t.Errorf("Register(%q) error = %v, want nil", name, err)
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, _, err := r.Build("seam.stable", Config{}); err != nil {
				t.Errorf("Build(\"seam.stable\") error = %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	for i := range goroutines {
		name := fmt.Sprintf("seam.dynamic-%d", i)
		if _, _, err := r.Build(name, Config{}); err != nil {
			t.Errorf("Build(%q) error = %v, want nil", name, err)
		}
	}
}
