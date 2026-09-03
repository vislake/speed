package storage

import (
	"context"
	"embed"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// TestModule_Identity pins the module's identity and, in particular, its
// honest OpenAPISpec answer this round: nil. The module's HTTP surface --
// the api/ fragment, the generated handler and the apiPath prefix -- is
// owned by the HTTP round, which ships the spec first (spec-first is not
// negotiable) and mounts the routes only once that fragment exists.
func TestModule_Identity(t *testing.T) {
	m := NewModule(nil)

	if got := m.Name(); got != moduleName {
		t.Errorf("Name() = %q, want %q", got, moduleName)
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- storage depends on no other pkgcore.Module; the queue it needs is a host-wired seam, not a bootstrap dependency", got)
	}
	if got := m.OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() is non-nil (%d bytes); the HTTP round owns the api/ fragment, and this round must not ship one it cannot mount", len(got))
	}
}

// TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot pins the layout
// dbkit.MigrationRegistry.Apply requires: a postgres/ and a sqlite/
// subdirectory at the FS root, each holding the same file names. A migration
// added to one dialect and forgotten in the other fails here rather than at a
// deployment that happens to run the other engine.
func TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot(t *testing.T) {
	fs := NewModule(nil).Migrations()

	names := map[string][]string{}
	for _, dialect := range []string{"postgres", "sqlite"} {
		entries, err := fs.ReadDir(dialect)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dialect, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s/ holds no migration files", dialect)
		}
		for _, e := range entries {
			names[dialect] = append(names[dialect], e.Name())
		}
	}
	if len(names["postgres"]) != len(names["sqlite"]) {
		t.Fatalf("dialect file counts differ: postgres %v, sqlite %v", names["postgres"], names["sqlite"])
	}
	for i, name := range names["postgres"] {
		if names["sqlite"][i] != name {
			t.Errorf("migration %q exists in postgres/ but sqlite/ has %q at the same position", name, names["sqlite"][i])
		}
	}
}

// TestModule_Locales_ShipsBothLanguages pins the i18n rule at the module
// boundary: exactly the two languages the catalog serves, no more and no
// fewer, since Kernel.Bootstrap rejects a module that ships a file for a
// language the others do not.
func TestModule_Locales_ShipsBothLanguages(t *testing.T) {
	entries, err := NewModule(nil).Locales().ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"zh-CN.toml", "en-US.toml"} {
		if !got[want] {
			t.Errorf("locales are missing %s", want)
		}
	}
	if len(entries) != 2 {
		t.Errorf("locales hold %d files, want exactly zh-CN.toml and en-US.toml", len(entries))
	}
}

// TestModule_Register_DeclaresItsSurface bootstraps storage through the real
// kernel -- the same path a host takes -- and asserts every declaration
// arrives on the registry. Bootstrapping rather than calling Register against
// a hand-built Registry is deliberate: it also proves storage's locale files
// survive i18n.Builder.AddModule's parity validation, which only runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	reg := bootstrapTestModule(t)

	t.Run("permissions", func(t *testing.T) {
		assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionRead, PermissionWrite})
	})

	t.Run("audit actions", func(t *testing.T) {
		assertContainsAll(t, reg.AuditActions.Actions(), []string{
			AuditActionObjectCreate, AuditActionObjectComplete, AuditActionObjectDelete,
		})
	})

	t.Run("published events", func(t *testing.T) {
		var types []string
		for _, decl := range reg.Events.Published() {
			types = append(types, decl.Type)
			if decl.PayloadType == "" || decl.Description == "" {
				t.Errorf("event %q is declared without a payload type or description", decl.Type)
			}
		}
		assertContainsAll(t, types, []string{EventObjectCompleted, EventObjectDeleted})
	})

	t.Run("no routes are mounted", func(t *testing.T) {
		// The absence is asserted, not left implicit: until the HTTP round
		// ships the spec and the apiPath constant, mounting anything would
		// be a route with no spec-generated handler behind it.
		if routes := reg.Routes.Routes(); len(routes) != 0 {
			t.Errorf("Register mounted %d route(s); the HTTP round owns route mounting, this round ships no api/ fragment", len(routes))
		}
	})

	t.Run("no configuration schema is declared", func(t *testing.T) {
		// storage honours its bounds as package constants rather than
		// declaring a dynamic-config schema; declaring a schema nothing
		// reads would be a lying schema.
		if got := reg.Config.Items(); len(got) != 0 {
			t.Errorf("Register declared %d config item(s); storage declares none it cannot honour", len(got))
		}
	})
}

// TestModule_Register_DoesNotDeclareForeignEvents guards the catalog
// collision that would surface once another module ships: storage declares
// exactly its own two events, each in the storage. namespace -- never a
// neighbour's event type, which would collide at bootstrap.
func TestModule_Register_DoesNotDeclareForeignEvents(t *testing.T) {
	reg := bootstrapTestModule(t)

	decls := reg.Events.Published()
	if len(decls) != 2 {
		t.Fatalf("storage declared %d events, want exactly 2", len(decls))
	}
	for _, decl := range decls {
		if !strings.HasPrefix(decl.Type, moduleName+".") {
			t.Errorf("storage declared event %q, which is not in its own %q namespace", decl.Type, moduleName+".")
		}
	}
}

// TestModule_Register_CoexistsWithAnotherModule proves storage bootstraps
// alongside a module that declares its own permissions, audit actions and
// events -- the real host shape -- rather than only in isolation.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), newWiredModule(t, nil), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionRead, "neighbour:read"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database, which
// is what pkgcore.Module's "it only declares" contract requires to be safe:
// any database call inside Register would panic here.
func TestModule_Register_PerformsNoIO(t *testing.T) {
	m := newWiredModule(t, nil)
	reg := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register against a nil database: %v", err)
	}
}

// TestModule_Register_RefusesAQueuelessBoot pins the ErrQueueRequired
// wiring rule: a storage module that could complete objects without any
// queue to finish their processing refuses to boot at Register time, the
// same shape org's indexer-required refusal takes.
func TestModule_Register_RefusesAQueuelessBoot(t *testing.T) {
	_, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), NewModule(nil))
	if !hasCode(err, ErrQueueRequired.Code) {
		t.Fatalf("Bootstrap without a queue error = %v, want storage.queue_required", err)
	}
}

// TestModule_Options_StoreTheirValues pins that each With* option writes the
// value its doc comment promises, into the field the enforcing rounds read.
func TestModule_Options_StoreTheirValues(t *testing.T) {
	m := NewModule(nil,
		WithMaxUploadBytes(1<<30),
		WithMaxImagePixels(123_456),
		WithDerivativeMaxEdge(640),
		WithUploadTTL(5*time.Minute),
		WithMaxObjectLifetime(10*24*time.Hour),
	)
	if m.maxUploadBytes != 1<<30 {
		t.Errorf("maxUploadBytes = %d, want %d", m.maxUploadBytes, 1<<30)
	}
	if m.maxImagePixels != 123_456 {
		t.Errorf("maxImagePixels = %d, want 123456", m.maxImagePixels)
	}
	if m.derivativeMaxEdge != 640 {
		t.Errorf("derivativeMaxEdge = %d, want 640", m.derivativeMaxEdge)
	}
	if m.uploadTTL != 5*time.Minute {
		t.Errorf("uploadTTL = %v, want 5m0s", m.uploadTTL)
	}
	if m.maxObjectLifetime != 10*24*time.Hour {
		t.Errorf("maxObjectLifetime = %v, want 240h0m0s", m.maxObjectLifetime)
	}
}

// TestModule_Options_IgnoreNonsenseValues pins the guard every scalar With*
// option carries: zero and negative values are nonsense for a bound (a
// ceiling of zero bytes would refuse every upload), so they are ignored and
// the package default stands. A value nobody can configure away by accident
// is a value an enforcing round can trust.
func TestModule_Options_IgnoreNonsenseValues(t *testing.T) {
	m := NewModule(nil,
		WithMaxUploadBytes(0),
		WithMaxImagePixels(-5),
		WithDerivativeMaxEdge(0),
		WithUploadTTL(-time.Minute),
		WithMaxObjectLifetime(0),
	)
	if m.maxUploadBytes != defaultMaxUploadBytes {
		t.Errorf("maxUploadBytes = %d, want the default %d", m.maxUploadBytes, defaultMaxUploadBytes)
	}
	if m.maxImagePixels != defaultMaxImagePixels {
		t.Errorf("maxImagePixels = %d, want the default %d", m.maxImagePixels, defaultMaxImagePixels)
	}
	if m.derivativeMaxEdge != defaultDerivativeMaxEdge {
		t.Errorf("derivativeMaxEdge = %d, want the default %d", m.derivativeMaxEdge, defaultDerivativeMaxEdge)
	}
	if m.uploadTTL != defaultUploadTTL {
		t.Errorf("uploadTTL = %v, want the default %v", m.uploadTTL, defaultUploadTTL)
	}
	if m.maxObjectLifetime != defaultMaxObjectLifetime {
		t.Errorf("maxObjectLifetime = %v, want the default %v", m.maxObjectLifetime, defaultMaxObjectLifetime)
	}
}

// TestModule_Options_AllowedTypes_ReplaceAndDefaultOpen pins the two
// promises of WithAllowedTypes: each call replaces the whole set (the last
// call wins, never a union), and the default is nil -- deliberately open,
// since no enforcer exists until the service round, and a restriction
// declared before an enforcer existed would only mislead (module.go records
// this).
func TestModule_Options_AllowedTypes_ReplaceAndDefaultOpen(t *testing.T) {
	if m := NewModule(nil); m.allowedTypes != nil {
		t.Errorf("default allowedTypes = %v, want nil (open until the service round enforces)", m.allowedTypes)
	}

	m := NewModule(nil, WithAllowedTypes("image/png"), WithAllowedTypes("application/pdf", "image/jpeg"))
	got := m.allowedTypes
	if len(got) != 2 || got[0] != "application/pdf" || got[1] != "image/jpeg" {
		t.Errorf("allowedTypes after two WithAllowedTypes calls = %v, want [application/pdf image/jpeg] -- the last call replaces", got)
	}

	// The option copies the caller's slice: mutating it afterwards must not
	// rewrite the module's configuration.
	types := []string{"image/png"}
	m2 := NewModule(nil, WithAllowedTypes(types...))
	types[0] = "image/gif"
	if len(m2.allowedTypes) != 1 || m2.allowedTypes[0] != "image/png" {
		t.Errorf("allowedTypes = %v, want [image/png] -- the option must copy, the caller's slice is not the module's config", m2.allowedTypes)
	}
}

// TestModule_Repositories_AreStablePinnedAccessors pins the repository
// accessors' contract: Objects() and Derivatives() return the same,
// non-nil repository on every call -- the instances the module constructed
// and whose connection it owns.
func TestModule_Repositories_AreStablePinnedAccessors(t *testing.T) {
	m := NewModule(nil)
	if m.Objects() == nil || m.Derivatives() == nil {
		t.Fatal("NewModule built nil repositories")
	}
	if first, second := m.Objects(), m.Objects(); first != second {
		t.Error("Objects() returns a different repository on each call; hosts hold onto it")
	}
	if first, second := m.Derivatives(), m.Derivatives(); first != second {
		t.Error("Derivatives() returns a different repository on each call; hosts hold onto it")
	}
}

// TestModule_Objects_ReturnsAUsableRepository drives the repository the
// module hands out against a real, migrated database: seed through the
// module's own accessor and read the row back -- the same journey the
// service round's first consumer takes.
func TestModule_Objects_ReturnsAUsableRepository(t *testing.T) {
	m := newWiredModule(t, newTestDB(t))
	ctx := tenantCtx("tenant-a")
	upload := newUpload("obj-1", "tenant-a", time.Now())

	if err := m.Objects().Create(ctx, &upload); err != nil {
		t.Fatalf("Create through the module's objects repository: %v", err)
	}
	got, err := m.Objects().FindByID(ctx, "obj-1")
	if err != nil {
		t.Fatalf("FindByID through the module's objects repository: %v", err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("state = %q, want %q", got.State, ObjectStateUploading)
	}
}

// TestModule_EventPayloads_JSONTags pins the wire shape of the two event
// payloads: snake_case keys, so the JSON a subscriber receives and the
// attributes a logger extracts use the project-wide attribute vocabulary,
// not Go field names.
func TestModule_EventPayloads_JSONTags(t *testing.T) {
	completed := ObjectCompletedPayload{ObjectID: "obj-1", Size: 2048, MIME: "image/png"}
	raw, err := json.Marshal(completed)
	if err != nil {
		t.Fatalf("marshal ObjectCompletedPayload: %v", err)
	}
	if got := string(raw); got != `{"object_id":"obj-1","size":2048,"mime":"image/png"}` {
		t.Errorf("ObjectCompletedPayload JSON = %s", got)
	}

	deleted := ObjectDeletedPayload{ObjectID: "obj-1"}
	raw, err = json.Marshal(deleted)
	if err != nil {
		t.Fatalf("marshal ObjectDeletedPayload: %v", err)
	}
	if got := string(raw); got != `{"object_id":"obj-1"}` {
		t.Errorf("ObjectDeletedPayload JSON = %s", got)
	}
}

// stubQueue is the do-nothing jobs.Queue every wired module in this file
// gets. Register only requires that a queue EXISTS; nothing in this round
// enqueues, so the stub records nothing and always succeeds.
type stubQueue struct{}

func (stubQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	return "", nil
}
func (stubQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (stubQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

// compile-time check that stubQueue satisfies jobs.Queue.
var _ jobs.Queue = stubQueue{}

// neighbourModule stands in for any other module a host bootstraps next to
// storage. It declares a disjoint surface, so a collision would mean storage
// claimed a name outside its namespace.
type neighbourModule struct{}

func (neighbourModule) Name() string         { return "neighbour" }
func (neighbourModule) DependsOn() []string  { return nil }
func (neighbourModule) Migrations() embed.FS { return embed.FS{} }
func (neighbourModule) Locales() embed.FS    { return embed.FS{} }
func (neighbourModule) OpenAPISpec() []byte  { return nil }
func (neighbourModule) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add("neighbour:read"); err != nil {
		return err
	}
	return reg.AuditActions.Add("neighbour.thing.do")
}

var _ pkgcore.Module = neighbourModule{}

// assertContainsAll fails t unless got holds every entry in want.
func assertContainsAll(t *testing.T, got, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, v := range got {
		set[v] = true
	}
	for _, v := range want {
		if !set[v] {
			t.Errorf("%q is missing from %v", v, got)
		}
	}
}

// newWiredModule builds a storage Module with the wiring Register requires --
// a queue -- plus whatever the caller adds. Every test that bootstraps or
// registers goes through it, so the required-queue rule is asserted in
// exactly one place (TestModule_Register_RefusesAQueuelessBoot) rather than
// re-tested by accident everywhere else.
func newWiredModule(t *testing.T, db *gorm.DB, opts ...Option) *Module {
	t.Helper()
	wired := append([]Option{WithQueue(stubQueue{})}, opts...)
	return NewModule(db, wired...)
}

// bootstrapTestModule bootstraps a fully wired storage module through the
// real kernel and returns the registry it registered against. Going through
// Bootstrap rather than calling Register on a hand-built Registry is
// deliberate: it is the only path that also merges the locale files and so
// proves they survive i18n.Builder.AddModule's parity validation.
func bootstrapTestModule(t *testing.T, opts ...Option) *pkgcore.Registry {
	t.Helper()
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), newWiredModule(t, nil, opts...))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return reg
}
