package pki

import (
	"context"
	"crypto"
	"embed"
	"testing"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

func TestModule_Identity(t *testing.T) {
	m := NewModule(nil)

	if got := m.Name(); got != moduleName {
		t.Errorf("Name() = %q, want %q", got, moduleName)
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- pki depends on no other pkgcore.Module in the bootstrap set", got)
	}
	if got := m.OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() = %v, want nil -- pki has no HTTP surface this round", got)
	}
}

// TestModule_DefaultsToLocalSigner proves NewModule wires LocalSigner
// without a WithSigner option -- the zero-external-dependency default
// "task dev" and every unit test in this module rely on.
func TestModule_DefaultsToLocalSigner(t *testing.T) {
	m := NewModule(newTestDB(t))
	if _, ok := m.Signer().(*LocalSigner); !ok {
		t.Errorf("Signer() = %T, want *LocalSigner by default", m.Signer())
	}
}

// TestModule_WithSigner_Overrides proves a caller can swap in a different
// Signer implementation -- the seam round 4's vault/kmsaws provider
// subpackages will use.
func TestModule_WithSigner_Overrides(t *testing.T) {
	fake := &fakeSigner{}
	m := NewModule(newTestDB(t), WithSigner("fake", fake))
	if m.Signer() != Signer(fake) {
		t.Errorf("Signer() did not return the injected fake")
	}
}

// TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot pins the layout
// dbkit.MigrationRegistry.Apply requires: a postgres/ and a sqlite/
// subdirectory at the FS root, each holding the same file names. A
// migration added to one dialect and forgotten in the other fails here
// rather than at a deployment that happens to run the other engine.
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

// TestModule_Register_DeclaresItsSurface bootstraps pki through the real
// kernel -- the same path a host takes -- and asserts every declaration
// arrives on the registry. Bootstrapping rather than calling Register
// against a hand-built Registry is deliberate: it also proves pki's locale
// files survive i18n.Builder.AddModule's parity validation, which only
// runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	db := newTestDB(t)
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), NewModule(db))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	t.Run("permissions", func(t *testing.T) {
		assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionRead, PermissionIssue})
	})

	t.Run("audit actions", func(t *testing.T) {
		assertContainsAll(t, reg.AuditActions.Actions(), []string{AuditActionAuthorityCreate, AuditActionCertificateIssue})
	})

	t.Run("config items", func(t *testing.T) {
		var keys []string
		for _, item := range reg.Config.Items() {
			keys = append(keys, item.Key)
			if item.Description == "" {
				t.Errorf("config item %q is declared without a description", item.Key)
			}
			if item.Sensitive {
				t.Errorf("config item %q is marked Sensitive; pki declares no sensitive items -- private keys never pass through config", item.Key)
			}
		}
		assertContainsAll(t, keys, []string{
			ConfigCADefaultValidity, ConfigCAMaxValidity,
			ConfigCertificateDefaultValidity, ConfigCertificateMaxValidity,
			ConfigPropagationWindow, ConfigRenewalLeadTime,
		})
	})

	t.Run("no routes are mounted", func(t *testing.T) {
		// pki has no HTTP surface this round -- see AGENTS.md's Known
		// limitations. Mounting nothing is asserted rather than left
		// implicit, so a stray Routes.Mount call in a future edit is
		// caught here.
		if got := reg.Routes.Routes(); len(got) != 0 {
			t.Errorf("Register mounted %d route(s), want 0", len(got))
		}
	})

	t.Run("events", func(t *testing.T) {
		// The three signing-key lifecycle events this round's expiry scan
		// drives -- see events.go's doc comment for why there is no fourth
		// (.revoked) or any pki.certificate.*/pki.authority.* event yet.
		var types []string
		for _, decl := range reg.Events.Published() {
			types = append(types, decl.Type)
		}
		assertContainsAll(t, types, []string{
			EventSigningKeyStaged, EventSigningKeyActivated, EventSigningKeyRetired,
		})
		if len(types) != 3 {
			t.Errorf("Register declared %d event(s), want exactly 3 this round: %v", len(types), types)
		}
	})

	t.Run("no jobs handler without a queue", func(t *testing.T) {
		// NewModule(db) above was built with no WithQueue -- Register must
		// not claim the expiry-scan task handler when there is no queue to
		// run it on.
		if _, ok := reg.Jobs.Handlers()[taskTypeExpiryScan]; ok {
			t.Errorf("Register claimed the expiry-scan job handler despite no WithQueue")
		}
	})
}

// TestModule_Register_WithQueue_ClaimsTheExpiryScanHandler proves the
// opposite of the "no jobs handler without a queue" case above: WithQueue
// makes Register claim taskTypeExpiryScan on reg.Jobs, so a host draining
// reg.Jobs.Handlers() onto its jobs.Queue gets a worker for it.
func TestModule_Register_WithQueue_ClaimsTheExpiryScanHandler(t *testing.T) {
	db := newTestDB(t)
	m := NewModule(db, WithQueue(stubQueue{}))
	t.Cleanup(func() { _ = m.Close() })

	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, ok := reg.Jobs.Handlers()[taskTypeExpiryScan]; !ok {
		t.Errorf("Register did not claim taskTypeExpiryScan despite WithQueue")
	}
}

// TestModule_Register_CoexistsWithAnotherModule proves pki bootstraps
// alongside a module that declares its own permissions and audit actions --
// the real host shape -- rather than only in isolation.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), NewModule(newTestDB(t)), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionRead, "neighbour:read"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database,
// which is what pkgcore.Module's "it only declares" contract requires to be
// safe: any database call inside Register would panic here.
func TestModule_Register_PerformsNoIO(t *testing.T) {
	m := NewModule(nil)
	reg := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register against a nil database: %v", err)
	}
}

// assertContainsAll fails the test for every want element missing from got.
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
// pki bootstraps correctly alongside another module's own declarations.
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

// fakeSigner is a minimal Signer double used only to prove WithSigner wires
// through.
type fakeSigner struct{}

func (*fakeSigner) GenerateKey(ctx context.Context, algorithm string) (string, crypto.PublicKey, error) {
	return "", nil, nil
}

func (*fakeSigner) Sign(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	return nil, nil
}

func (*fakeSigner) Public(ctx context.Context, keyRef string) (crypto.PublicKey, error) {
	return nil, nil
}
func (*fakeSigner) Destroy(ctx context.Context, keyRef string) error { return nil }

var _ Signer = (*fakeSigner)(nil)

// stubQueue is a do-nothing jobs.Queue, used only to prove Register claims
// (or does not claim) the expiry-scan task handler -- it is never actually
// drained in this file, mirroring storage's identical module_test.go
// stubQueue.
type stubQueue struct{}

func (stubQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	return "", nil
}
func (stubQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (stubQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

// compile-time check that stubQueue satisfies jobs.Queue.
var _ jobs.Queue = stubQueue{}
