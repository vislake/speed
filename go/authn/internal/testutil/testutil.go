// Package testutil holds the fakes and fixtures authn's tests share.
//
// It deliberately does NOT import the authn package. Test files in package
// authn cannot import anything that imports authn -- that is an import cycle
// in the test binary -- so the membership fake here satisfies
// authn.MembershipReader structurally, by having the right method set, rather
// than by naming the interface. The compile-time proof that the two agree is
// the assignment at every call site.
package testutil

import (
	"context"
	"embed"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// serializerName mirrors authn.SerializerName. It is duplicated rather than
// imported for the no-cycle reason in the package comment; the pair is pinned
// by a test in the authn package itself.
const serializerName = "authn_pii"

// BlindIndexKey returns the fixed 32-byte HMAC key the tests compute blind
// indexes under. It is a test fixture and nothing else: a deployment injects
// a real secret.
func BlindIndexKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// CipherKey returns the fixed 32-byte field-encryption key the tests use. It
// is deliberately DIFFERENT from BlindIndexKey: an encryption key and a
// deterministic index key must never be the same secret, and a test fixture
// that shared one would quietly teach the wrong thing.
func CipherKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	return key
}

// Clock is a manually advanced time source, so a test can expire a token or a
// session without sleeping. It is safe for concurrent use, which matters
// because the refresh-rotation tests run under -race.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock reading start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now returns the current reading. The method value is what gets passed to
// authn.WithClock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Memberships is an in-memory stand-in for the org module's membership store,
// satisfying authn.MembershipReader structurally.
type Memberships struct {
	mu     sync.Mutex
	byUser map[string][]pkgcore.TenantID
	err    error
}

// NewMemberships returns an empty membership set.
func NewMemberships() *Memberships {
	return &Memberships{byUser: make(map[string][]pkgcore.TenantID)}
}

// Add records userID as an active member of tenants, in order.
func (m *Memberships) Add(userID string, tenants ...pkgcore.TenantID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byUser[userID] = append(m.byUser[userID], tenants...)
}

// Remove drops userID's membership of tenant, so a test can prove that
// removal takes effect on an existing session's next refresh.
func (m *Memberships) Remove(userID string, tenant pkgcore.TenantID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.byUser[userID][:0]
	for _, t := range m.byUser[userID] {
		if t != tenant {
			kept = append(kept, t)
		}
	}
	m.byUser[userID] = kept
}

// FailWith makes every lookup return err, so a test can prove that an
// unanswerable membership question refuses rather than defaults.
func (m *Memberships) FailWith(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// ActiveMembership implements the membership seam.
func (m *Memberships) ActiveMembership(_ context.Context, userID string, tenantID pkgcore.TenantID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	for _, t := range m.byUser[userID] {
		if t == tenantID {
			return true, nil
		}
	}
	return false, nil
}

// TenantsOf implements the membership seam.
func (m *Memberships) TenantsOf(_ context.Context, userID string) ([]pkgcore.TenantID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	out := make([]pkgcore.TenantID, len(m.byUser[userID]))
	copy(out, m.byUser[userID])
	return out, nil
}

// migrationModule is the minimal pkgcore.Module NewDB feeds to
// dbkit.MigrationRegistry, carrying authn's real migration files. It exists
// because the registry works in terms of modules, and building the real
// authn.Module here would import authn.
type migrationModule struct{}

func (migrationModule) Name() string                     { return "authn" }
func (migrationModule) DependsOn() []string              { return nil }
func (migrationModule) Migrations() embed.FS             { return migrations.FS }
func (migrationModule) Locales() embed.FS                { return migrations.FS }
func (migrationModule) OpenAPISpec() []byte              { return nil }
func (migrationModule) Register(*pkgcore.Registry) error { return nil }

// NewDB returns a fresh in-memory SQLite database with authn's PII serializer
// registered and every authn migration applied from zero.
//
// The serializer is registered BEFORE the handle is opened, which is the
// ordering the real host must follow too: GORM's serializer registry is
// process-global and is consulted while a model's schema is parsed.
func NewDB(t *testing.T) *gorm.DB {
	t.Helper()

	cipher, err := dbkit.NewCipher(CipherKey())
	if err != nil {
		t.Fatalf("build the test cipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(serializerName, cipher)

	db := dbtest.NewSQLite(t)

	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(migrationModule{}); err != nil {
		t.Fatalf("register authn's migrations: %v", err)
	}
	if err := registry.Apply(t.Context(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply authn's migrations from zero: %v", err)
	}
	return db
}

// EventRecorder collects the domain events published on a bus, so a test can
// assert that a security-relevant fact was announced rather than only that a
// row changed. It is safe for concurrent use, which the refresh-rotation race
// tests need.
type EventRecorder struct {
	mu     sync.Mutex
	events []pkgcore.Event
}

// NewEventRecorder returns an empty recorder.
func NewEventRecorder() *EventRecorder {
	return &EventRecorder{}
}

// Subscribe installs the recorder on bus for each of types.
func (r *EventRecorder) Subscribe(bus pkgcore.EventBus, types ...string) {
	for _, eventType := range types {
		bus.Subscribe(eventType, func(_ context.Context, evt pkgcore.Event) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, evt)
			return nil
		})
	}
}

// Events returns a snapshot of everything recorded so far.
func (r *EventRecorder) Events() []pkgcore.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]pkgcore.Event, len(r.events))
	copy(out, r.events)
	return out
}

// Count reports how many events of eventType were recorded.
func (r *EventRecorder) Count(eventType string) int {
	n := 0
	for _, evt := range r.Events() {
		if evt.Type == eventType {
			n++
		}
	}
	return n
}

// First returns the first recorded event of eventType.
func (r *EventRecorder) First(eventType string) (pkgcore.Event, bool) {
	for _, evt := range r.Events() {
		if evt.Type == eventType {
			return evt, true
		}
	}
	return pkgcore.Event{}, false
}

// ErrKVUnavailable is what FailingKVStore returns from every operation.
var ErrKVUnavailable = errors.New("testutil: the key-value store is unreachable")

// FailingKVStore is a pkgcore.KVStore whose every operation fails, so a test
// can prove that code depending on it fails CLOSED rather than treating an
// unreachable store as an empty one.
type FailingKVStore struct{}

// Get implements pkgcore.KVStore.
func (FailingKVStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, ErrKVUnavailable
}

// Set implements pkgcore.KVStore.
func (FailingKVStore) Set(context.Context, string, []byte, time.Duration) error {
	return ErrKVUnavailable
}

// Delete implements pkgcore.KVStore.
func (FailingKVStore) Delete(context.Context, string) error { return ErrKVUnavailable }

// IncrByFloat implements pkgcore.KVStore.
func (FailingKVStore) IncrByFloat(context.Context, string, float64) (float64, error) {
	return 0, ErrKVUnavailable
}

// CompareAndSwap implements pkgcore.KVStore.
func (FailingKVStore) CompareAndSwap(context.Context, string, []byte, []byte) (bool, error) {
	return false, ErrKVUnavailable
}

// compile-time check that FailingKVStore satisfies the seam it stands in for.
var _ pkgcore.KVStore = FailingKVStore{}
