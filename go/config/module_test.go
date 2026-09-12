package config

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// Tests for module.go's Module wiring: what Register declares (routes in
// mount order, the change event, the audit action, the audited system
// purpose), the Attach seam's guards (second attach, nil registry, nil
// database, Sensitive declarations without a cipher), and -- the
// consumer-side proof -- a componenttest.DeclareModules run over a fake
// host module that leaves a flag dependency unresolved, which must fail the
// declaration rather than start a host with a flag graph that can never
// resolve.

// fakeHostModule stands in for a consumer module that declares its own
// configuration items and feature flags beside the config module's. It is
// deliberately the smallest valid module: nothing to migrate, no
// locale resources, no spec fragment.
type fakeHostModule struct {
	name  string
	items []pkgcore.ConfigItem
	flags []pkgcore.FeatureFlag
}

func (f *fakeHostModule) Name() string        { return f.name }
func (f *fakeHostModule) DependsOn() []string { return nil }
func (f *fakeHostModule) Migrations() embed.FS {
	return embed.FS{}
}
func (f *fakeHostModule) Locales() embed.FS { return embed.FS{} }
func (f *fakeHostModule) OpenAPISpec() []byte {
	return nil
}

func (f *fakeHostModule) Register(reg *pkgcore.ComponentRegistry) error {
	if err := reg.ConfigSeat().Add(f.items...); err != nil {
		return err
	}
	return reg.FeaturesSeat().Add(f.flags...)
}

// moduleTestDBSeq numbers the in-memory SQLite databases this file's tests
// open, so parallel or repeated runs never share one.
var moduleTestDBSeq atomic.Int64

func openModuleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:config_module_%d?mode=memory&cache=shared", moduleTestDBSeq.Add(1))
	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(NewModule(db)); err != nil {
		t.Fatalf("registering the config migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("applying the config migrations: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// newPlainRegistry returns a registry built over throwaway in-memory seams,
// the way a host that never wired a real bus or KV store does.
func newPlainRegistry() *pkgcore.ComponentRegistry {
	return componenttest.NewRegistry()
}

// TestModule_OpenAPISpec_DeclaresBothEndpointPaths pins the fragment's
// binding to the module's exported path constants: api/openapi.yaml is what
// the generated api.ServerInterface and the application-wide merged
// contracts/speed.yaml derive from, so a path that moved in only one of the
// two places would ship a spec the endpoints do not serve.
func TestModule_OpenAPISpec_DeclaresBothEndpointPaths(t *testing.T) {
	spec := NewModule(nil).OpenAPISpec()
	if len(spec) == 0 {
		t.Fatal("OpenAPISpec() is empty; the module ships api/openapi.yaml")
	}
	for _, path := range []string{PathPublic, PathSystemFeatures} {
		if !bytes.Contains(spec, []byte(path)) {
			t.Errorf("OpenAPISpec() does not mention %q -- the fragment must declare every path the module mounts", path)
		}
	}
}

func TestModule_Register_MountsBothConfigRoutesInOrder(t *testing.T) {
	reg := newPlainRegistry()
	m := NewModule(nil)
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}

	routes := reg.RoutesSeat().Routes()
	if len(routes) != 2 {
		t.Fatalf("the module mounted %d routes, want 2", len(routes))
	}
	for i, path := range []string{PathPublic, PathSystemFeatures} {
		if routes[i].Path != path {
			t.Fatalf("route %d = %q, want %q (mount order is part of the wiring contract)", i, routes[i].Path, path)
		}
		if routes[i].Handler == nil {
			t.Fatalf("route %q has no handler", path)
		}
	}
}

func TestModule_Register_DeclaresTheChangeEventAndAuditAction(t *testing.T) {
	reg := newPlainRegistry()
	if err := componenttest.DeclareInto(reg, NewModule(nil)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	published := reg.EventsSeat().Published()
	if len(published) != 1 {
		t.Fatalf("the module declared %d events, want 1", len(published))
	}
	decl := published[0]
	if decl.Type != EventConfigItemChanged {
		t.Fatalf("declared event type = %q, want %q", decl.Type, EventConfigItemChanged)
	}
	if decl.PayloadType != eventConfigItemChangedPayloadType {
		t.Fatalf("declared payload type = %q, want %q", decl.PayloadType, eventConfigItemChangedPayloadType)
	}

	actions := reg.AuditActionsSeat().Actions()
	if len(actions) != 1 || actions[0] != AuditActionConfigSet {
		t.Fatalf("registered audit actions = %v, want exactly [%s]", actions, AuditActionConfigSet)
	}
}

func TestModule_Register_DeclaresTheSystemWritePurpose(t *testing.T) {
	// Attach's ScopeSystem entitlement asks the context for a system reason
	// whose purpose the module itself declared. Register must make that
	// purpose valid, so the entitlement is usable on a bootstrapped host.
	if err := componenttest.DeclareInto(newPlainRegistry(), NewModule(nil)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "ops-1",
		Purpose: SystemPurposeSystemWrite,
	}); err != nil {
		t.Fatalf("WithSystemContext with the module's own purpose: %v", err)
	}
}

func TestDeclareModules_FailsOnAnUnresolvedFlagDependency(t *testing.T) {
	// The consumer-side proof of the config module's schema contract: the
	// module's schema is assembled from the registry's combined item and
	// flag declarations at Attach time, so a host whose flag graph cannot
	// resolve must never start. A host module declaring a flag that depends
	// on a flag no module registers is exactly such a host.
	configModule := NewModule(nil)
	host := &fakeHostModule{
		name: "smile-host",
		flags: []pkgcore.FeatureFlag{
			{Key: "ai.dream_preview", Default: true, DependsOn: []string{"ai.smile_preview"}},
		},
	}

	_, err := componenttest.DeclareModules(configModule, host)
	if err == nil {
		t.Fatal("assembly succeeded with an unresolved flag dependency; the graph must fail closed")
	}
	if !errors.Is(err, pkgcore.ErrUnresolvedFeatureDependency) {
		t.Fatalf("assembly error = %v, want one wrapping %v", err, pkgcore.ErrUnresolvedFeatureDependency)
	}
}

func TestDeclareAll_AttachServesTheAssembledHostSchema(t *testing.T) {
	// The happy path of the same contract: a host module declaring items
	// and a flag chain, declared beside the config module. The schema
	// Attach folds is the registry's union -- items and flags owned by a
	// different module -- and the service serves defaults, tenant overrides
	// and dependency walks out of it.
	db := openModuleTestDB(t)
	configModule := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	host := &fakeHostModule{
		name: "smile-host",
		items: []pkgcore.ConfigItem{
			{Key: "brand.site_name", Type: "string", Default: "Smile Studio", Public: true, Description: "The tenant's display name", Group: "brand"},
			{Key: "ai.dream_model", Type: "string", Sensitive: true, Description: "The model the dream renders use"},
		},
		flags: []pkgcore.FeatureFlag{
			{Key: "ai.smile_preview", Default: false, Description: "Lets tenants try smile previews"},
			{Key: "ai.premium_upsell", Default: true, DependsOn: []string{"ai.smile_preview"}},
		},
	}

	reg := componenttest.NewRegistry()
	var svc *Service
	if err := componenttest.DeclareAll(reg, host.Register, configModule.Register, func(r *pkgcore.ComponentRegistry) error {
		attached, attachErr := configModule.Attach(r)
		svc = attached
		return attachErr
	}); err != nil {
		t.Fatalf("declare and attach: %v", err)
	}

	if name, err := GetTyped[string](svc, context.Background(), "brand.site_name"); err != nil || name != "Smile Studio" {
		t.Fatalf("GetTyped over the host's default = %q, %v", name, err)
	}

	// The host's flag chain resolves through the combined schema.
	if enabled, err := svc.IsEnabled(context.Background(), "ai.premium_upsell"); err != nil || enabled {
		t.Fatalf("IsEnabled(chain) = %v, %v; want false until its dependency turns on", enabled, err)
	}
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	if err := svc.Set(tenantCtx, ScopeTenant, "ai.smile_preview", Value{Data: true}, "alice"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if enabled, err := svc.IsEnabled(tenantCtx, "ai.premium_upsell"); err != nil || !enabled {
		t.Fatalf("IsEnabled(chain) after its dependency turned on = %v, %v", enabled, err)
	}
}

// declareTestSchema is the declaration step that folds the shared test
// schema into a registry's seats; the caller runs it inside an Init window,
// which is the only time the seats accept writes.
func declareTestSchema(r *pkgcore.ComponentRegistry) error {
	if err := r.ConfigSeat().Add(serviceTestSchemaItems...); err != nil {
		return err
	}
	return r.FeaturesSeat().Add(serviceTestSchemaFlags...)
}

// attachModule is the declaration step that runs m.Attach inside an Init
// window, storing the attached Service in out when out is non-nil.
func attachModule(m *Module, out **Service) func(*pkgcore.ComponentRegistry) error {
	return func(r *pkgcore.ComponentRegistry) error {
		svc, err := m.Attach(r)
		if err != nil {
			return err
		}
		if out != nil {
			*out = svc
		}
		return nil
	}
}

func TestModule_Attach_RejectsASecondCall(t *testing.T) {
	db := openModuleTestDB(t)
	reg := newPlainRegistry()
	m := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	if err := componenttest.DeclareAll(reg, declareTestSchema, attachModule(m, nil)); err != nil {
		t.Fatalf("declare and attach: %v", err)
	}

	_, err := m.Attach(reg)
	assertCode(t, err, ErrAlreadyAttached)
}

func TestModule_Attach_ExactlyOneCallSucceedsUnderConcurrentCallers(t *testing.T) {
	// Attach's "called exactly once" guard must hold under concurrency, not
	// only for sequential callers: two concurrent Attaches must not both
	// pass the m.service == nil check, each building its own Service,
	// subscribing it to the bus and starting its own poller -- the caller
	// whose Service lost the write to m.service would hold a Service whose
	// poller keeps running and whose bus subscription keeps firing, with no
	// way to learn it was orphaned. Each round below releases a batch of
	// callers against one fresh Module; exactly one must succeed and every
	// other call must fail with ErrAlreadyAttached.
	db := openModuleTestDB(t)
	for round := 0; round < 8; round++ {
		reg := newPlainRegistry()
		m := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))

		const callers = 16
		results := make(chan error, callers)
		// The concurrent batch runs inside the registry's one Init window:
		// Attach subscribes on the Events seat, and the seats accept writes
		// only during Init.
		if err := componenttest.DeclareAll(reg, declareTestSchema,
			func(r *pkgcore.ComponentRegistry) error {
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i := 0; i < callers; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						_, err := m.Attach(r)
						results <- err
					}()
				}
				close(start)
				wg.Wait()
				close(results)
				return nil
			},
		); err != nil {
			t.Fatalf("round %d: declare and attach: %v", round, err)
		}

		successes := 0
		for err := range results {
			if err == nil {
				successes++
				continue
			}
			assertCode(t, err, ErrAlreadyAttached)
		}
		if successes != 1 {
			t.Fatalf("round %d: %d of %d concurrent Attach calls succeeded, want exactly 1 (the losers' pollers and bus subscriptions would be orphaned)",
				round, successes, callers)
		}
	}
}

func TestModule_Attach_GuardsItsDependencies(t *testing.T) {
	m := NewModule(openModuleTestDB(t), WithPollInterval(0))
	if _, err := m.Attach(nil); err == nil {
		t.Fatal("Attach with a nil registry must fail: the schema is folded from the registry, so there is nothing to attach to")
	}

	reg := newPlainRegistry()
	if _, err := NewModule(nil, WithPollInterval(0)).Attach(reg); err == nil {
		t.Fatal("Attach without a database must fail: reads and writes resolve rows from the configs table")
	}
}

func TestModule_Attach_RequiresACipherForSensitiveDeclarations(t *testing.T) {
	db := openModuleTestDB(t)
	reg := newPlainRegistry()
	m := NewModule(db, WithPollInterval(0))

	// serviceTestSchemaItems declares the Sensitive support.reply_email; a
	// host that hands the module no cipher could never write or read that
	// value without leaking it at rest, so Attach must refuse the pairing.
	err := componenttest.DeclareAll(reg, declareTestSchema, attachModule(m, nil))
	assertCode(t, err, ErrCipherRequired)
}

// TestModule_Attach_ReportsASchemaConflict pins Attach's fail-closed half
// for a declaration set that cannot fold: a flag declared under a key a
// configuration item already owns has no single schema to live in, and
// Attach must refuse the whole set rather than publish a snapshot where
// one layer silently loses to the other.
func TestModule_Attach_ReportsASchemaConflict(t *testing.T) {
	db := openModuleTestDB(t)
	reg := newPlainRegistry()
	m := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	colliding := pkgcore.FeatureFlag{Key: "brand.site_name", Default: true, Description: "declared under a key a configuration item already owns"}

	err := componenttest.DeclareAll(reg, declareTestSchema,
		func(r *pkgcore.ComponentRegistry) error { return r.FeaturesSeat().Add(colliding) },
		attachModule(m, nil),
	)
	assertCode(t, err, ErrSchemaConflict)
}

// TestModule_Attach_RequiresAnEventBus pins the one seam Attach cannot
// synthesize: the item-changed subscription lands on the assembled
// pkgcore.EventBus value, so an assembly carrying none must fail the
// attach loudly rather than unwind into a Service whose change
// notifications could never reach anyone.
func TestModule_Attach_RequiresAnEventBus(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	m := NewModule(openModuleTestDB(t), WithCipher(buildTestCipher(t)), WithPollInterval(0))

	err := componenttest.DeclareAll(reg, declareTestSchema, attachModule(m, nil))
	if err == nil {
		t.Fatal("Attach succeeded without an assembled EventBus value; the item-changed subscription would have no bus to land on")
	}
	if !strings.Contains(err.Error(), "EventBus") {
		t.Fatalf("Attach error = %v, want the missing-bus refusal naming the EventBus", err)
	}
}

// TestModule_CompleteSnapshot_SupersedesAnEarlierHostSnapshot pins the
// host-attached flow the descriptor's Start callback completes: a host
// consumer that needed the Service during the Init stage attached and
// published it early, a declaration landed later in the same window, and
// CompleteSnapshot re-folds the schema to the complete set over the very
// Service the host already holds -- the earlier, partial snapshot is
// superseded, never left serving.
func TestModule_CompleteSnapshot_SupersedesAnEarlierHostSnapshot(t *testing.T) {
	db := openModuleTestDB(t)
	reg := newPlainRegistry()
	m := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	late := pkgcore.ConfigItem{Key: "late.declared_key", Type: "string", Default: "late", Description: "declared after the host attached"}

	var hostSvc *Service
	if err := componenttest.DeclareAll(reg, declareTestSchema,
		attachModule(m, &hostSvc),
		func(r *pkgcore.ComponentRegistry) error { return r.ConfigSeat().Add(late) },
	); err != nil {
		t.Fatalf("declare, attach and declare again: %v", err)
	}

	svc, attached, err := m.CompleteSnapshot(reg)
	if err != nil {
		t.Fatalf("CompleteSnapshot: %v", err)
	}
	if attached {
		t.Error("CompleteSnapshot reported a fresh attach although the host had already published a Service")
	}
	if svc != hostSvc {
		t.Error("CompleteSnapshot did not return the Service the host already holds")
	}
	if got, err := GetTyped[string](svc, context.Background(), late.Key); err != nil || got != "late" {
		t.Errorf("the completed snapshot does not serve the declaration made after the host's attach: %q, %v", got, err)
	}
}

// TestModule_CompleteSnapshot_ReportsALateSchemaConflict pins the
// completion's fail-closed half: a declaration landing after the host's
// attach that cannot fold -- here a flag under a key an item already owns
// -- fails the completion, so the schema left published stays the last one
// that was sound.
func TestModule_CompleteSnapshot_ReportsALateSchemaConflict(t *testing.T) {
	db := openModuleTestDB(t)
	reg := newPlainRegistry()
	m := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	colliding := pkgcore.FeatureFlag{Key: "brand.site_name", Default: true, Description: "declared under a key a configuration item already owns"}

	if err := componenttest.DeclareAll(reg, declareTestSchema,
		attachModule(m, nil),
		func(r *pkgcore.ComponentRegistry) error { return r.FeaturesSeat().Add(colliding) },
	); err != nil {
		t.Fatalf("declare, attach and declare again: %v", err)
	}

	_, _, err := m.CompleteSnapshot(reg)
	assertCode(t, err, ErrSchemaConflict)
}

// TestModule_CompleteSnapshot_RefusesALateSensitiveItemWithoutACipher pins
// the cipher re-check the completion carries: a declaration landing after
// the host's attach that grows the schema into a Sensitive item must be
// refused without a cipher to seal it, exactly as Attach would have
// refused the same complete set.
func TestModule_CompleteSnapshot_RefusesALateSensitiveItemWithoutACipher(t *testing.T) {
	db := openModuleTestDB(t)
	reg := newPlainRegistry()
	// No WithCipher: the attach-time schema declares no Sensitive item, so
	// the early attach is legal and the late Sensitive declaration is the
	// first thing that needs a cipher.
	m := NewModule(db, WithPollInterval(0))
	plain := pkgcore.ConfigItem{Key: "late.plain_key", Type: "string", Default: "plain", Description: "not Sensitive, so the early attach needs no cipher"}
	secret := pkgcore.ConfigItem{Key: "late.secret_key", Type: "string", Sensitive: true, Description: "declared after the host attached, and needs a cipher"}

	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.ConfigSeat().Add(plain) },
		attachModule(m, nil),
		func(r *pkgcore.ComponentRegistry) error { return r.ConfigSeat().Add(secret) },
	); err != nil {
		t.Fatalf("declare, attach and declare again: %v", err)
	}

	_, _, err := m.CompleteSnapshot(reg)
	assertCode(t, err, ErrCipherRequired)
}

// TestModule_Register_ReportsSeatRefusals pins Register's propagation of a
// seat refusal: a duplicate event type or audit action means this module's
// own declaration is already occupied by another writer, and Register must
// surface the refusal rather than swallow half its declaration set.
func TestModule_Register_ReportsSeatRefusals(t *testing.T) {
	t.Run("duplicate event type", func(t *testing.T) {
		reg := newPlainRegistry()
		err := componenttest.DeclareAll(reg,
			func(r *pkgcore.ComponentRegistry) error { return r.EventsSeat().Publishes(eventDecl) },
			NewModule(nil).Register,
		)
		if !errors.Is(err, pkgcore.ErrDuplicateEventType) {
			t.Fatalf("Register error = %v, want it to wrap %v", err, pkgcore.ErrDuplicateEventType)
		}
	})

	t.Run("duplicate audit action", func(t *testing.T) {
		reg := newPlainRegistry()
		err := componenttest.DeclareAll(reg,
			func(r *pkgcore.ComponentRegistry) error { return r.AuditActionsSeat().Add(AuditActionConfigSet) },
			NewModule(nil).Register,
		)
		if !errors.Is(err, pkgcore.ErrDuplicateAuditAction) {
			t.Fatalf("Register error = %v, want it to wrap %v", err, pkgcore.ErrDuplicateAuditAction)
		}
	})
}

// TestComponent_SchemaDeclaresItsCipherKey pins the one process-start key
// this module's contract names: the cipher key behind the cipher that seals
// Sensitive values, declared as a derive-tagged []byte field resolving at
// exactly the platform key path the module exports -- so a schema rename
// cannot silently rotate every sealed value's key. Key material must stay
// off the runtime layer precisely because the key that encrypts the configs
// table cannot be a row in it, so the runtime schema must stay free of the
// identifier.
func TestComponent_SchemaDeclaresItsCipherKey(t *testing.T) {
	reg := newPlainRegistry()
	if err := componenttest.DeclareInto(reg, NewModule(nil)); err != nil {
		t.Fatalf("Register: %v", err)
	}

	descriptors, err := pkgcore.DescribeComponentSchema(moduleName, component().ConfigSchema)
	if err != nil {
		t.Fatalf("DescribeComponentSchema() error = %v", err)
	}
	byKey := make(map[string]pkgcore.FieldDescriptor, len(descriptors))
	for _, d := range descriptors {
		byKey[d.Key] = d
	}
	field, ok := byKey[CipherKeyPath]
	if !ok {
		t.Fatalf("the config component did not declare key material at %q", CipherKeyPath)
	}
	if !field.Derive || field.Type != "[]byte" || !field.Sensitive {
		t.Errorf("field = %+v, want a Sensitive derive-tagged []byte key", field)
	}
	if field.Group != moduleName {
		t.Errorf("field group = %q, want the module name %q", field.Group, moduleName)
	}
	if field.Doc.Default == "" || field.Doc.Description == "" || field.Doc.Example != "" {
		t.Errorf("field doc = %+v, want a documented fallback and contract text, and no suggested value", field.Doc)
	}
	for _, item := range reg.ConfigSeat().Items() {
		if item.Key == CipherKeyPath {
			t.Errorf("key %q is declared on both the key-material layer and the runtime schema", item.Key)
		}
	}
}
