package config

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// Tests for module.go's Module wiring: what Register declares (routes in
// mount order, the change event, the audit action, the audited system
// purpose), the Attach seam's guards (second attach, nil registry, nil
// database, Sensitive declarations without a cipher), and -- the
// consumer-side proof -- a Kernel.Bootstrap over a fake host module that
// leaves a flag dependency unresolved, which must fail the bootstrap rather
// than start a host with a flag graph that can never resolve.

// fakeHostModule stands in for a consumer module that declares its own
// configuration items and feature flags beside the config module's. It is
// deliberately the smallest valid pkgcore.Module: nothing to migrate, no
// locale resources, no spec fragment.
type fakeHostModule struct {
	name  string
	items []pkgcore.ConfigItem
	flags []pkgcore.FeatureFlag
}

var _ pkgcore.Module = (*fakeHostModule)(nil)

func (f *fakeHostModule) Name() string        { return f.name }
func (f *fakeHostModule) DependsOn() []string { return nil }
func (f *fakeHostModule) Migrations() embed.FS {
	return embed.FS{}
}
func (f *fakeHostModule) Locales() embed.FS { return embed.FS{} }
func (f *fakeHostModule) OpenAPISpec() []byte {
	return nil
}

func (f *fakeHostModule) Register(reg *pkgcore.Registry) error {
	if err := reg.Config.Add(f.items...); err != nil {
		return err
	}
	return reg.Features.Add(f.flags...)
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
func newPlainRegistry() *pkgcore.Registry {
	return pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
}

func TestModule_Register_MountsBothConfigRoutesInOrder(t *testing.T) {
	reg := newPlainRegistry()
	m := NewModule(nil)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	routes := reg.Routes.Routes()
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
	if err := NewModule(nil).Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	published := reg.Events.Published()
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

	actions := reg.AuditActions.Actions()
	if len(actions) != 1 || actions[0] != AuditActionConfigSet {
		t.Fatalf("registered audit actions = %v, want exactly [%s]", actions, AuditActionConfigSet)
	}
}

func TestModule_Register_DeclaresTheSystemWritePurpose(t *testing.T) {
	// Attach's ScopeSystem entitlement asks the context for a system reason
	// whose purpose the module itself declared. Register must make that
	// purpose valid, so the entitlement is usable on a bootstrapped host.
	if err := NewModule(nil).Register(newPlainRegistry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "ops-1",
		Purpose: SystemPurposeSystemWrite,
	}); err != nil {
		t.Fatalf("WithSystemContext with the module's own purpose: %v", err)
	}
}

func TestKernelBootstrap_FailsOnAnUnresolvedFlagDependency(t *testing.T) {
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

	_, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).Bootstrap(context.Background(), configModule, host)
	if err == nil {
		t.Fatal("Bootstrap succeeded with an unresolved flag dependency; the graph must fail closed")
	}
	if !errors.Is(err, pkgcore.ErrUnresolvedFeatureDependency) {
		t.Fatalf("Bootstrap error = %v, want one wrapping %v", err, pkgcore.ErrUnresolvedFeatureDependency)
	}
}

func TestKernelBootstrap_AttachServesTheAssembledHostSchema(t *testing.T) {
	// The happy path of the same contract: a host module declaring items
	// and a flag chain, bootstrapped beside the config module. The schema
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

	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).Bootstrap(context.Background(), host, configModule)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	svc, err := configModule.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
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

func TestModule_Attach_RejectsASecondCall(t *testing.T) {
	db := openModuleTestDB(t)
	reg := newPlainRegistry()
	if err := reg.Config.Add(serviceTestSchemaItems...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}
	if err := reg.Features.Add(serviceTestSchemaFlags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	m := NewModule(db, WithCipher(buildTestCipher(t)), WithPollInterval(0))
	if _, err := m.Attach(reg); err != nil {
		t.Fatalf("first Attach: %v", err)
	}

	_, err := m.Attach(reg)
	assertCode(t, err, ErrAlreadyAttached)
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
	if err := reg.Config.Add(serviceTestSchemaItems...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}

	// serviceTestSchemaItems declares the Sensitive support.reply_email; a
	// host that hands the module no cipher could never write or read that
	// value without leaking it at rest, so Attach must refuse the pairing.
	_, err := NewModule(db, WithPollInterval(0)).Attach(reg)
	assertCode(t, err, ErrCipherRequired)
}
