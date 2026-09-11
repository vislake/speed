package pkgcore

// Unit tests for the Kernel's seam resource lifecycle -- Bootstrap closing
// what it already resolved on failure, and Shutdown closing what a
// successful Bootstrap resolved from a Preset (see Registration's
// resource-ownership contract and Kernel's own "Seam lifecycle" doc
// comment). The real-backend half of the same story -- dialed Redis and
// NATS connections actually released -- lives in the module's integration
// tier; here, closeable fake implementations stand in for them, because the
// mechanism under test is the Kernel's recording and closing, not any one
// implementation's Close.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// closeLog records which named seams were closed, in order, for the
// lifecycle assertions below.
type closeLog struct {
	mu    sync.Mutex
	names []string
}

func (l *closeLog) record(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *closeLog) closed() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.names...)
}

// lifecycleBus is a pkgcore.EventBus stand-in whose Close() error records
// itself on log -- the Registration-level contract's shape.
type lifecycleBus struct {
	log  *closeLog
	name string
}

func (b *lifecycleBus) Subscribe(string, EventHandler) {}
func (b *lifecycleBus) Publish(context.Context, Event) error {
	return nil
}

func (b *lifecycleBus) Close() error {
	b.log.record(b.name)
	return nil
}

// lifecycleKVStore mirrors lifecycleBus for the KVStore seam.
type lifecycleKVStore struct {
	log  *closeLog
	name string
}

func (s *lifecycleKVStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (s *lifecycleKVStore) Set(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (s *lifecycleKVStore) Delete(context.Context, string) error { return nil }
func (s *lifecycleKVStore) IncrByFloat(context.Context, string, float64) (float64, error) {
	return 0, nil
}

func (s *lifecycleKVStore) IncrByFloatWithTTL(context.Context, string, float64, time.Duration) (float64, error) {
	return 0, nil
}

func (s *lifecycleKVStore) CompareAndSwap(context.Context, string, []byte, []byte) (bool, error) {
	return false, nil
}

func (s *lifecycleKVStore) Close() error {
	s.log.record(s.name)
	return nil
}

// lifecycleMailer mirrors lifecycleBus for the Mailer seam.
type lifecycleMailer struct {
	log  *closeLog
	name string
}

func (m *lifecycleMailer) Send(context.Context, Mail) error { return nil }

func (m *lifecycleMailer) Close() error {
	m.log.record(m.name)
	return nil
}

// lifecycleObjectStore mirrors lifecycleBus for the ObjectStore seam.
type lifecycleObjectStore struct {
	log  *closeLog
	name string
}

func (s *lifecycleObjectStore) PutObject(context.Context, string, io.Reader) error {
	return nil
}

func (s *lifecycleObjectStore) GetObject(context.Context, string) (io.ReadCloser, error) {
	return nil, nil
}
func (s *lifecycleObjectStore) DeleteObject(context.Context, string) error { return nil }

func (s *lifecycleObjectStore) Close() error {
	s.log.record(s.name)
	return nil
}

// lifecyclePreset registers one closeable fake per seam, under
// lifecycle-tagged names, on the package-level registries, and returns the
// Preset naming them, in resolution order, together with the shared close
// log. The names are unique to this suite, so the registrations persist
// harmlessly for the rest of the test process (SeamRegistry has no
// unregister; nothing else resolves these names).
func lifecyclePreset(t *testing.T, log *closeLog) Preset {
	t.Helper()

	preset := Preset{}
	preset[presetKeyEventBus] = SeamPreset{Implementation: registerLifecycleBus(t, log)}
	preset[presetKeyKVStore] = SeamPreset{Implementation: registerLifecycleKVStore(t, log)}
	preset[presetKeyMailer] = SeamPreset{Implementation: registerLifecycleMailer(t, log)}
	preset[presetKeyObjectStore] = SeamPreset{Implementation: registerLifecycleObjectStore(t, log)}
	return preset
}

// lifecycleName makes a unique registration name per call.
var lifecycleNameCounter int

func lifecycleName(seam string) string {
	lifecycleNameCounter++
	return fmt.Sprintf("test.lifecycle.%s.%d", seam, lifecycleNameCounter)
}

func registerLifecycleBus(t *testing.T, log *closeLog) string {
	t.Helper()
	name := lifecycleName("bus")
	if err := EventBusRegistry.Register(Registration[EventBus]{
		Name: name,
		New: func(Config) (EventBus, error) {
			return &lifecycleBus{log: log, name: name}, nil
		},
	}); err != nil {
		t.Fatalf("register %q on EventBusRegistry: %v", name, err)
	}
	return name
}

func registerLifecycleKVStore(t *testing.T, log *closeLog) string {
	t.Helper()
	name := lifecycleName("kv")
	if err := KVStoreRegistry.Register(Registration[KVStore]{
		Name: name,
		New: func(Config) (KVStore, error) {
			return &lifecycleKVStore{log: log, name: name}, nil
		},
	}); err != nil {
		t.Fatalf("register %q on KVStoreRegistry: %v", name, err)
	}
	return name
}

func registerLifecycleMailer(t *testing.T, log *closeLog) string {
	t.Helper()
	name := lifecycleName("mailer")
	if err := MailerRegistry.Register(Registration[Mailer]{
		Name: name,
		New: func(Config) (Mailer, error) {
			return &lifecycleMailer{log: log, name: name}, nil
		},
	}); err != nil {
		t.Fatalf("register %q on MailerRegistry: %v", name, err)
	}
	return name
}

func registerLifecycleObjectStore(t *testing.T, log *closeLog) string {
	t.Helper()
	name := lifecycleName("objectstore")
	if err := ObjectStoreRegistry.Register(Registration[ObjectStore]{
		Name: name,
		New: func(Config) (ObjectStore, error) {
			return &lifecycleObjectStore{log: log, name: name}, nil
		},
	}); err != nil {
		t.Fatalf("register %q on ObjectStoreRegistry: %v", name, err)
	}
	return name
}

// TestKernel_ShutdownClosesEveryPresetResolvedSeam pins the happy path of
// the seam lifecycle: a successful Bootstrap transfers the preset-resolved
// implementations' closers to the Kernel, and Shutdown runs each of them
// exactly once, in reverse resolution order (the object-store seam, resolved
// last, closes first). Without the transferred closers, a preset-resolved
// implementation's own dialed connections would have no close path at all:
// nothing in the Kernel (or anywhere reachable from it) would ever release
// them.
func TestKernel_ShutdownClosesEveryPresetResolvedSeam(t *testing.T) {
	log := &closeLog{}
	preset := lifecyclePreset(t, log)
	kernel := NewKernel(WithPreset(preset))

	if _, err := kernel.Bootstrap(context.Background(), regTestModule{name: "lifecycle-app"}); err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}

	if err := kernel.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}

	// Resolution order is bus, kv, mailer, objectstore, so reverse
	// teardown order is objectstore, mailer, kv, bus.
	bus := preset[presetKeyEventBus].Implementation
	kv := preset[presetKeyKVStore].Implementation
	mailer := preset[presetKeyMailer].Implementation
	objectStore := preset[presetKeyObjectStore].Implementation
	got := log.closed()
	want := []string{objectStore, mailer, kv, bus}
	if len(got) != len(want) {
		t.Fatalf("closers run = %v, want every preset-resolved seam closed (%v)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("closers run in order %v, want %v (reverse resolution order)", got, want)
		}
	}

	// Shutdown is idempotent: a second call runs no closer twice.
	if err := kernel.Shutdown(); err != nil {
		t.Fatalf("second Shutdown() error = %v, want nil", err)
	}
	if again := log.closed(); len(again) != len(want) {
		t.Errorf("closers run after a second Shutdown = %d, want still %d: Shutdown must be idempotent", len(again), len(want))
	}
}

// TestBootstrap_FailureAfterResolvingSeams_ClosesWhatItResolved pins the
// failure half of the seam lifecycle: a Bootstrap that fails after resolving
// one or more preset seams closes the implementations it already resolved
// before returning the error, so a misconfigured composition never leaks
// dialed connections to a Shutdown the host will never call on a failed
// kernel.
func TestBootstrap_FailureAfterResolvingSeams_ClosesWhatItResolved(t *testing.T) {
	t.Run("a later resolution error closes the earlier seams", func(t *testing.T) {
		log := &closeLog{}
		preset := Preset{
			presetKeyEventBus: SeamPreset{Implementation: registerLifecycleBus(t, log)},
			presetKeyKVStore:  SeamPreset{Implementation: registerLifecycleKVStore(t, log)},
			presetKeyMailer:   SeamPreset{Implementation: "test.lifecycle.no.such.mailer"}, // resolves after bus and kv
		}
		kernel := NewKernel(WithPreset(preset))

		if _, err := kernel.Bootstrap(context.Background()); err == nil {
			t.Fatal("Bootstrap() succeeded, want the unknown mailer name to fail it")
		} else if !errors.Is(err, ErrUnknownImplementation) {
			t.Fatalf("Bootstrap() error = %v, want one wrapping ErrUnknownImplementation", err)
		}

		if got := log.closed(); len(got) != 2 {
			t.Errorf("seams closed after the failed bootstrap = %v, want the resolved bus and kv closed", got)
		}
	})

	t.Run("a register error closes all four resolved seams", func(t *testing.T) {
		log := &closeLog{}
		kernel := NewKernel(WithPreset(lifecyclePreset(t, log)))

		// Every seam resolves; the failure lands in module registration,
		// after all four were resolved.
		_, err := kernel.Bootstrap(context.Background(), regTestModule{
			name: "register-fails",
			register: func(Registrar) error {
				return errors.New("boom")
			},
		})
		if err == nil {
			t.Fatal("Bootstrap() succeeded, want the failing module to fail it")
		}

		if got := log.closed(); len(got) != 4 {
			t.Errorf("seams closed after the failed bootstrap = %v, want all four resolved seams closed", got)
		}
	})
}

// TestKernel_ShutdownNeverClosesAnInjectedSeam pins the ownership boundary
// of the seam lifecycle: a seam the host injected with WithEventBus or one
// of its siblings is the host's to close, so Shutdown must not touch it,
// even when the other three seams resolve through the Preset and are
// closed.
func TestKernel_ShutdownNeverClosesAnInjectedSeam(t *testing.T) {
	log := &closeLog{}
	preset := lifecyclePreset(t, log)

	// The injected bus is a lifecycleBus too -- same Close contract -- but
	// it is handed to the Kernel by the host, which keeps owning it.
	injected := &lifecycleBus{log: log, name: "injected-host-bus"}
	kernel := NewKernel(WithPreset(preset), WithEventBus(injected, 0))

	reg, err := kernel.Bootstrap(context.Background(), regTestModule{name: "lifecycle-app"})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}
	if reg.EventBus() != EventBus(injected) {
		t.Fatalf("EventBus() = %v, want the injected bus wired in", reg.EventBus())
	}

	if err := kernel.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}

	for _, name := range log.closed() {
		if name == "injected-host-bus" {
			t.Error("Shutdown closed the injected bus, want only preset-resolved seams closed")
		}
	}
	// The other three preset-resolved seams were closed.
	if got := log.closed(); len(got) != 3 {
		t.Errorf("seams closed = %v, want exactly the three preset-resolved seams (kv, mailer, objectstore)", got)
	}
}

// TestKernel_ShutdownAfterStandaloneBootstrap_IsANoOp pins that the default
// standalone composition has nothing to close: the in-process seams own no
// resources, so Bootstrap records no closers and Shutdown succeeds without
// running any.
func TestKernel_ShutdownAfterStandaloneBootstrap_IsANoOp(t *testing.T) {
	kernel := NewKernel()
	if _, err := kernel.Bootstrap(context.Background(), regTestModule{name: "standalone-app"}); err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}
	if err := kernel.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
}

// TestSeamCloserOf_PinsTheShapeOfTheContract verifies the interface
// assertion seamCloserOf performs: only a value implementing Close() error
// yields a closer, and anything else yields nil (never a panic).
func TestSeamCloserOf_PinsTheShapeOfTheContract(t *testing.T) {
	log := &closeLog{}
	closer := seamCloserOf(&lifecycleBus{log: log, name: "closable"})
	if closer == nil {
		t.Fatal("seamCloserOf() on a Close() error value = nil, want the value's Close")
	}
	if err := closer(); err != nil {
		t.Fatalf("closer() error = %v, want nil", err)
	}

	if closer := seamCloserOf(NewMemoryEventBus()); closer != nil {
		t.Error("seamCloserOf() on the in-memory bus (no Close) = non-nil, want nil")
	}
	if closer := seamCloserOf(NewMemoryKVStore()); closer != nil {
		t.Error("seamCloserOf() on the in-memory store (no Close) = non-nil, want nil")
	}
}
