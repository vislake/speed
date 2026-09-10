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
	if got := m.OpenAPISpec(); len(got) == 0 {
		t.Errorf("OpenAPISpec() = %v, want the embedded api/openapi.yaml bytes -- the module's HTTP surface", got)
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
// Signer implementation -- the seam the vault/kmsaws provider subpackages
// use.
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
		assertContainsAll(t, reg.Permissions.Permissions(), []string{
			PermissionRead, PermissionIssue, PermissionRevokeSigningKey,
			PermissionRevokeCertificate, PermissionRotate,
		})
	})

	t.Run("audit actions", func(t *testing.T) {
		assertContainsAll(t, reg.AuditActions.Actions(), []string{
			AuditActionAuthorityCreate, AuditActionCertificateIssue,
			AuditActionKeyRevoke, AuditActionCertificateRevoke,
		})
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
			ConfigCRLDistributionPoint, ConfigCRLValidity,
		})
	})

	t.Run("HTTP surface is mounted", func(t *testing.T) {
		// pki mounts its HTTP surface at apiPath.
		routes := reg.Routes.Routes()
		if len(routes) != 1 {
			t.Fatalf("Register mounted %d route(s), want exactly 1 (apiPath)", len(routes))
		}
		if routes[0].Path != apiPath {
			t.Errorf("Register mounted path %q, want %q", routes[0].Path, apiPath)
		}
	})

	t.Run("events", func(t *testing.T) {
		// The five signing-key/certificate lifecycle events the expiry scan
		// and revocation drive -- see events.go's doc comment.
		var types []string
		for _, decl := range reg.Events.Published() {
			types = append(types, decl.Type)
		}
		assertContainsAll(t, types, []string{
			EventSigningKeyStaged, EventSigningKeyActivated, EventSigningKeyRetired,
			EventSigningKeyRevoked, EventCertificateRevoked,
		})
		if len(types) != 5 {
			t.Errorf("Register declared %d event(s), want exactly 5 this round: %v", len(types), types)
		}
	})

	t.Run("no jobs handler without a queue", func(t *testing.T) {
		// NewModule(db) above was built with no WithQueue -- Register must
		// not claim the expiry-scan or CRL-regenerate task handlers when
		// there is no queue to run them on.
		if _, ok := reg.Jobs.Handlers()[taskTypeExpiryScan]; ok {
			t.Errorf("Register claimed the expiry-scan job handler despite no WithQueue")
		}
		if _, ok := reg.Jobs.Handlers()[taskTypeCRLRegenerate]; ok {
			t.Errorf("Register claimed the CRL-regenerate job handler despite no WithQueue")
		}
	})
}

// TestModule_Register_WithQueue_ClaimsTheExpiryScanHandler proves the
// opposite of the "no jobs handler without a queue" case above: WithQueue
// makes Register claim taskTypeExpiryScan AND taskTypeCRLRegenerate on
// reg.Jobs, so a host draining reg.Jobs.Handlers() onto its jobs.Queue
// gets a worker for both.
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
	if _, ok := reg.Jobs.Handlers()[taskTypeCRLRegenerate]; !ok {
		t.Errorf("Register did not claim taskTypeCRLRegenerate despite WithQueue")
	}
}

// TestModule_Register_RevokePermissionsAreSplitByDataDomain pins the
// permission split: no single permission may cover both the signing-key
// revoke (a platform-level
// operation on pki_signing_keys, whose permission must be evaluated in the
// platform domain -- rbac.SystemDomain -- never in a request tenant's
// domain) and the certificate revoke (a tenant-level operation on
// pki_certificates, whose permission is evaluated in the request tenant's
// own domain). One name spanning the two leaves half of it wrong in
// whichever domain it is evaluated in: a tenant-domain holder of the
// platform half could stop the whole deployment's token issuance. The
// declared catalog is therefore exactly the five names below: no spanning
// name exists in it, so a grant of one would be refused by rbac's own
// attach-time catalog freeze rather than half meaning something. The names
// are literal strings rather than the module constants, so the assertion
// does not share its data with the catalog it checks.
func TestModule_Register_RevokePermissionsAreSplitByDataDomain(t *testing.T) {
	db := newTestDB(t)
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), NewModule(db))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	got := make(map[string]bool)
	for _, permission := range reg.Permissions.Permissions() {
		got[permission] = true
	}
	want := []string{
		"pki:read",
		"pki:issue",
		"pki:revoke_signing_key",
		"pki:revoke_certificate",
		"pki:rotate",
	}
	if len(got) != len(want) {
		t.Fatalf("declared permission set = %v, want exactly %v (regression: one spanning pki:revoke covered both a platform and a tenant operation)", got, want)
	}
	for _, permission := range want {
		if !got[permission] {
			t.Errorf("declared permission set = %v, missing %q", got, permission)
		}
	}
	if got["pki:revoke"] {
		t.Errorf("declared permission set still contains the spanning %q; a grant of it would cover both a platform and a tenant operation in whichever single domain it is evaluated in", "pki:revoke")
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

// TestModule_Register_DeclaresBothPeriodicSchedules pins the module's
// periodic declarations: a queue-wired Register puts exactly the
// expiry-scan and CRL-regeneration schedules on the registry's Schedules
// seat -- declaring means scheduled, so a host running a jobs.Scheduler
// over the finished registry scans for expiry and regenerates CRLs at the
// tasks' own windows. A queue-less module declares neither, the same gate
// its handler registration uses: no declaration may outlive its executor.
func TestModule_Register_DeclaresBothPeriodicSchedules(t *testing.T) {
	reg := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	m := NewModule(newTestDB(t), WithQueue(&recordingQueue{}))
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	want := []pkgcore.PeriodicTask{m.service.expiryScanSchedule(), m.ca.crlRegenerateSchedule()}
	decls := reg.Schedules.Declarations()
	if len(decls) != len(want) {
		t.Fatalf("Register declared %d schedules (%+v), want exactly the scan and the CRL regeneration", len(decls), decls)
	}
	for i := range want {
		if decls[i] != want[i] {
			t.Errorf("declaration %d = %+v, want %+v", i, decls[i], want[i])
		}
	}

	// A queue-less module registers no handlers, so it declares nothing.
	queueless := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	if err := NewModule(newTestDB(t)).Register(queueless); err != nil {
		t.Fatalf("Register without a queue: %v", err)
	}
	if decls := queueless.Schedules.Declarations(); len(decls) != 0 {
		t.Errorf("a queue-less Register declared %+v, want none -- no declaration may outlive its executor", decls)
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

// TestModule_Register_DeclaresItsBootstrapKey pins the one process-start key
// pki's contract names: the AES key sealing the LocalSigner key column, a
// Sensitive hex key separate from every other module's material, and disjoint
// from the runtime schema's own validity items.
func TestModule_Register_DeclaresItsBootstrapKey(t *testing.T) {
	t.Parallel()

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := NewModule(newTestDB(t)).Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	declared := reg.Bootstrap.Keys()
	if len(declared) != 1 {
		t.Fatalf("Register declared %d bootstrap keys (%v), want exactly the local-key cipher key", len(declared), declared)
	}
	key := declared[0]
	if key.Key != "pki.local_key_cipher_key" {
		t.Errorf("declared key = %q, want pki.local_key_cipher_key", key.Key)
	}
	if key.Format != "hexkey" || !key.Sensitive {
		t.Errorf("declaration = %+v, want a Sensitive hexkey", key)
	}
	if key.Group != moduleName {
		t.Errorf("declaration group = %q, want the module name %q", key.Group, moduleName)
	}
	if key.Default == "" || key.Description == "" || key.Example != "" {
		t.Errorf("declaration = %+v, want a documented fallback and contract text, and no suggested value", key)
	}
	for _, item := range reg.Config.Items() {
		if item.Key == key.Key {
			t.Errorf("key %q is declared on both the bootstrap seat and the runtime schema", item.Key)
		}
	}
}
