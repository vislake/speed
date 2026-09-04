package billing

import (
	"context"
	"embed"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing/internal/testutil"
	"github.com/vislake/speed/go/billing/migrations"
)

// newTestDB returns a fresh, per-call SQLite *gorm.DB with this module's
// migrations applied from zero. Shared across this package's own test
// files -- never duplicated, and never importable from outside the
// package anyway, so a plain top-level helper here (rather than
// internal/testutil, which is for cross-package sharing) is the correct
// home, mirroring go/metering's identical newTestDB in repository_test.go.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

// stubUsage is a fixed-value UsageReader for tests that do not exercise
// EntitlementsService's Quota path in detail (entitlements_test.go has its
// own, more elaborate fake for that).
type stubUsage struct{ count float64 }

func (s stubUsage) RealtimeCount(tenantID, feature string, at time.Time) (float64, error) {
	return s.count, nil
}

func TestModule_Identity(t *testing.T) {
	m := NewModule(nil, stubUsage{})

	if got := m.Name(); got != moduleName {
		t.Errorf("Name() = %q, want %q", got, moduleName)
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- billing declares no cross-module pkgcore.Module dependency this round", got)
	}
	if got := m.OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() = %v, want nil -- billing has no HTTP surface this round", got)
	}
}

func TestModule_Accessors_ReturnWiredPieces(t *testing.T) {
	m := NewModule(nil, stubUsage{})

	if m.Plans() == nil {
		t.Error("Plans() = nil")
	}
	if m.Subscriptions() == nil {
		t.Error("Subscriptions() = nil")
	}
	if m.Invoices() == nil {
		t.Error("Invoices() = nil")
	}
	if m.Credits() == nil {
		t.Error("Credits() = nil")
	}
	if m.Entitlements() == nil {
		t.Error("Entitlements() = nil")
	}
	if m.PaymentEvents() == nil {
		t.Error("PaymentEvents() = nil")
	}
	if m.Polling() == nil {
		t.Error("Polling() = nil")
	}
}

// TestWithGateways_WiresThePollingServicesGatewayMap proves WithGateways'
// value reaches PollingService -- the identical WithSigner-style direct-
// injection seam gateway.go's own doc comment describes.
func TestWithGateways_WiresThePollingServicesGatewayMap(t *testing.T) {
	gw := &fakeRegistryGateway{}
	m := NewModule(nil, stubUsage{}, WithGateways(map[string]PaymentGateway{"stripe": gw}))

	if got := m.Polling().gateways["stripe"]; got != gw {
		t.Errorf("Polling().gateways[\"stripe\"] = %v, want the *fakeRegistryGateway WithGateways was given", got)
	}
}

// TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot pins the layout
// dbkit.MigrationRegistry.Apply requires: a postgres/ and a sqlite/
// subdirectory at the FS root, each holding the same file names. A
// migration added to one dialect and forgotten in the other fails here
// rather than at a deployment that happens to run the other engine.
func TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot(t *testing.T) {
	fs := NewModule(nil, stubUsage{}).Migrations()

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
	entries, err := NewModule(nil, stubUsage{}).Locales().ReadDir(".")
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

// TestModule_Register_DeclaresItsSurface bootstraps billing through the
// real kernel -- the same path a host takes -- and asserts every
// declaration arrives on the registry. Bootstrapping rather than calling
// Register against a hand-built Registry is deliberate: it also proves
// billing's locale files survive i18n.Builder.AddModule's parity
// validation, which only runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	m := NewModule(newTestDB(t), stubUsage{})
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	t.Run("permissions", func(t *testing.T) {
		assertContainsAll(t, reg.Permissions.Permissions(), []string{
			PermissionPlanManage,
			PermissionSubscriptionRead,
			PermissionSubscriptionManage,
			PermissionCreditRead,
			PermissionCreditManage,
		})
	})

	t.Run("audit actions", func(t *testing.T) {
		assertContainsAll(t, reg.AuditActions.Actions(), []string{
			AuditActionCreditGrant,
			AuditActionCreditDeductReserve,
			AuditActionCreditDeductConfirm,
			AuditActionCreditRefund,
			AuditActionCreditExpire,
		})
	})

	t.Run("published events", func(t *testing.T) {
		var types []string
		for _, decl := range reg.Events.Published() {
			types = append(types, decl.Type)
			if decl.Description == "" {
				t.Errorf("event %q is declared without a description", decl.Type)
			}
		}
		assertContainsAll(t, types, []string{EventPlanChanged, EventSubscriptionStatusChanged})
	})

	t.Run("no routes are mounted", func(t *testing.T) {
		if got := reg.Routes.Routes(); len(got) != 0 {
			t.Errorf("Register mounted %d route(s), want 0 -- billing has no HTTP surface this round", len(got))
		}
	})

	t.Run("no config items are declared", func(t *testing.T) {
		// No round-1 code path reads a live config value -- see AGENTS.md's
		// design-choice section for why inventing one speculatively is the
		// exact thing this repo's round-boundary discipline forbids.
		if got := reg.Config.Items(); len(got) != 0 {
			t.Errorf("Register declared %d config item(s), want 0 this round", len(got))
		}
	})

	t.Run("services' EventBus is wired", func(t *testing.T) {
		if m.subService.events == nil {
			t.Error("subService.events is nil after Register; a status transition could never publish")
		}
		if m.planService.events == nil {
			t.Error("planService.events is nil after Register; a plan write could never publish")
		}
	})

	t.Run("no jobs handler without a queue", func(t *testing.T) {
		// NewModule(db, usage) above was built with no WithQueue -- Register
		// must not claim the poll task handler when there is no queue to run
		// it on -- see go/pki's identical proof for its own expiry-scan task.
		if _, ok := reg.Jobs.Handlers()[taskTypePoll]; ok {
			t.Errorf("Register claimed the poll job handler despite no WithQueue")
		}
	})
}

// TestModule_Register_WithQueue_ClaimsThePollHandler proves the opposite of
// the "no jobs handler without a queue" case above: WithQueue makes
// Register claim taskTypePoll on reg.Jobs, so a host draining
// reg.Jobs.Handlers() onto its jobs.Queue gets a worker for the
// active-polling fallback.
func TestModule_Register_WithQueue_ClaimsThePollHandler(t *testing.T) {
	m := NewModule(newTestDB(t), stubUsage{}, WithQueue(stubQueue{}))
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, ok := reg.Jobs.Handlers()[taskTypePoll]; !ok {
		t.Errorf("Register did not claim taskTypePoll despite WithQueue")
	}
}

// stubQueue is a no-op jobs.Queue, sufficient for WithQueue tests that only
// need Register to see a non-nil queue -- it is never actually enqueued to
// or drained by these tests.
type stubQueue struct{}

func (stubQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	return "", nil
}
func (stubQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (stubQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

var _ jobs.Queue = stubQueue{}

// TestModule_Register_CoexistsWithAnotherModule proves billing bootstraps
// alongside a module that declares its own permission -- the real host
// shape -- rather than only in isolation.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), NewModule(newTestDB(t), stubUsage{}), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionPlanManage, "neighbour:read"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database,
// which is what pkgcore.Module's "it only declares" contract requires to
// be safe: any database call inside Register would panic here.
func TestModule_Register_PerformsNoIO(t *testing.T) {
	m := NewModule(nil, stubUsage{})
	reg := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register against a nil database: %v", err)
	}
}

// assertContainsAll fails the test for every want element missing from
// got.
func assertContainsAll(t *testing.T, got []string, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, s := range got {
		set[s] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing %q; got %v", w, got)
		}
	}
}

// neighbourModule is a minimal second pkgcore.Module, used only to prove
// billing bootstraps correctly alongside another module's own
// declarations.
type neighbourModule struct{}

func (neighbourModule) Name() string         { return "neighbour" }
func (neighbourModule) DependsOn() []string  { return nil }
func (neighbourModule) Migrations() embed.FS { return embed.FS{} }
func (neighbourModule) Locales() embed.FS    { return embed.FS{} }
func (neighbourModule) OpenAPISpec() []byte  { return nil }
func (neighbourModule) Register(reg *pkgcore.Registry) error {
	return reg.Permissions.Add("neighbour:read")
}

var _ pkgcore.Module = neighbourModule{}
