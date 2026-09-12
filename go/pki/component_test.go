package pki

import (
	"context"
	"crypto/x509/pkix"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/config"
	configmigrations "github.com/vislake/speed/go/config/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/pki/internal/testutil"
	"github.com/vislake/speed/go/pki/migrations"
)

// testDBComponent is a stand-in for the database component a real assembly
// selects: it declares the *gorm.DB product and constructs the test's
// migrated handle, so the descriptors' declared database dependency resolves
// exactly as it will against the real db component.
func testDBComponent(db *gorm.DB) pkgcore.Component {
	return pkgcore.Component{
		Name:     "test.db",
		Module:   "test",
		Provides: []any{(*gorm.DB)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return db, nil
		},
	}
}

// TestComponentWellFormed pins the descriptor's contract: the naming
// convention, the relation between the component name and the module it
// implements, the configuration schema's decodability and the token shapes,
// exactly as componenttest asserts them for a component package.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, component())
}

// TestComponentAssemblesThroughRegistry drives the registered descriptor
// through the assembly's stages the way a host would: selection from a
// composition configuration (every schema field set), construction from the
// database and queue products in the by-type context, the closing
// validation, and the shutdown sequence. It proves the declared
// dependencies, assets and configuration schema line up with what New
// actually consumes.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	db := testutil.NewSQLite(t, moduleName, migrations.FS)
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(db)); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	reg.Put(jobs.NewStandaloneQueue(db))
	// The Init callback installs the service's own cache-invalidation
	// subscriptions during the assembly's Init stage, so the assembly
	// carries the bus they land on.
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"components": map[string]any{
			"pki": map[string]any{
				"propagation_window": "5m",
				"renewal_lead_time":  "24h",
				"expiry_scan_window": "1h",
				"cache_ttl":          "30s",
			},
			"test.db": nil,
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	} {
		if err := stage.run(ctx); err != nil {
			t.Fatalf("%s: %v", stage.name, err)
		}
	}

	module, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembled product is not reachable: %v", err)
	}
	if module == nil {
		t.Fatal("the assembled product is nil")
	}
	if names := pkgcore.MemberNames(reg, moduleName); len(names) != 1 || names[0] != moduleName {
		t.Fatalf("MemberNames = %v, want [%s]", names, moduleName)
	}
	if assets := pkgcore.Assets(reg); len(assets) != 1 || assets[0].Name != moduleName {
		t.Fatalf("Assets = %v, want exactly the %s entry", assets, moduleName)
	}

	if err := reg.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so every declaration lands in the assembly's
// own seats and the module's handler is built over the assembly's own
// values.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	db := testutil.NewSQLite(t, moduleName, migrations.FS)
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, component(), db, pkgcore.NewMemoryEventBus()); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{
		PermissionRead, PermissionIssue, PermissionRevokeSigningKey,
		PermissionRevokeCertificate, PermissionRotate,
	})
	assertContainsAll(t, reg.AuditActions.Actions(), []string{
		AuditActionAuthorityCreate, AuditActionCertificateIssue,
		AuditActionKeyRevoke, AuditActionCertificateRevoke,
	})
	keys := make([]string, 0, len(reg.Config.Items()))
	for _, item := range reg.Config.Items() {
		keys = append(keys, item.Key)
	}
	assertContainsAll(t, keys, []string{ConfigCADefaultValidity, ConfigPropagationWindow, ConfigCRLValidity})

	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	assertContainsAll(t, types, []string{EventSigningKeyStaged, EventSigningKeyActivated, EventSigningKeyRevoked, EventSigningKeyRetired, EventCertificateRevoked})

	routes := reg.Routes.Routes()
	if len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}
	// The module was constructed without a queue: Init must not claim the
	// expiry-scan or CRL-regenerate handlers when there is none.
	if _, ok := reg.Jobs.Handlers()[taskTypeExpiryScan]; ok {
		t.Errorf("Init claimed the expiry-scan job handler without a queue")
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
}

// TestComponentConstructionRefusesBadInputs pins New's failure paths: an
// undeclared configuration key, a missing database product and an ambiguous
// optional queue each fail the construction with an error rather than a
// half-built module.
func TestComponentConstructionRefusesBadInputs(t *testing.T) {
	ctx := context.Background()

	t.Run("undeclared configuration key", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(testutil.NewSQLite(t, moduleName, migrations.FS))); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(map[string]any{"bogus": "x"}))
		if err == nil {
			t.Fatal("construction accepted an undeclared configuration key")
		}
	})

	t.Run("missing database", func(t *testing.T) {
		_, err := component().New(ctx, pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
		if err == nil {
			t.Fatal("construction proceeded without a database product")
		}
	})

	t.Run("ambiguous queue", func(t *testing.T) {
		db := testutil.NewSQLite(t, moduleName, migrations.FS)
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(db)); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		reg.Put(jobs.NewStandaloneQueue(db))
		reg.Put(jobs.NewStandaloneQueue(db))
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(nil))
		if err == nil {
			t.Fatal("construction picked one of two queue values instead of refusing the ambiguity")
		}
	})
}

// TestComponent_SelfWiresTheSettingsReaderFromAConfigComponent drives the
// pki and config descriptors through one real assembly: pki declares the
// config module as an optional requirement, so with config selected the
// descriptor wires the config handle as the module's SettingsReader on its
// own -- the test never calls WithSettingsReader -- and a system row
// written through the assembled config service governs issuance through
// the assembled pki module.
func TestComponent_SelfWiresTheSettingsReaderFromAConfigComponent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	testutil.Migrate(t, db, dbkit.DialectSQLite, "config", configmigrations.FS)

	cipher, cipherErr := dbkit.NewCipher([]byte(testLocalKeyCipherKey))
	if cipherErr != nil {
		t.Fatalf("dbkit.NewCipher: %v", cipherErr)
	}

	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(db)); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	// The cipher config's descriptor takes as an optional dependency, Put
	// before the assembly runs -- the shape a host's own component hands it
	// over.
	reg.Put(cipher)
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"pki":     nil,
			"config":  nil,
			"test.db": nil,
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	} {
		if err := stage.run(ctx); err != nil {
			t.Fatalf("%s: %v", stage.name, err)
		}
	}

	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembled pki product is not reachable: %v", err)
	}
	cfgSvc, err := pkgcore.Get[*config.Service](reg)
	if err != nil {
		t.Fatalf("the assembled config service is not reachable: %v", err)
	}

	// The config component's descriptor carries the system-write purpose,
	// which the assembly registered at the Init stage's entry; the write is
	// the operator's own path.
	const rowValidity = 45 * 24 * time.Hour
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "ops-1",
		Purpose: config.SystemPurposeSystemWrite,
		Ticket:  "ticket-42",
	})
	if err != nil {
		t.Fatalf("pkgcore.WithSystemContext: %v", err)
	}
	if setErr := cfgSvc.Set(sysCtx, config.ScopeSystem, ConfigCADefaultValidity, config.Value{Data: rowValidity}, "ops-1"); setErr != nil {
		t.Fatalf("Set %s: %v", ConfigCADefaultValidity, setErr)
	}

	// Issuance through the descriptor-constructed module must honor the row
	// without any host-side wiring of the settings seam.
	authority, err := m.CA().CreateRootCA(ctx, CAParams{Subject: pkix.Name{CommonName: "speed Root CA"}})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}
	if got := authority.NotAfter.Sub(authority.NotBefore); (got - rowValidity).Abs() > 5*time.Second {
		t.Fatalf("issued validity span = %v; want the assembled config row %v (the descriptor must wire the settings reader itself)", got, rowValidity)
	}
}

// TestSignerLocalComponent_WellFormed runs the descriptor through the
// component contract: the naming convention, the typed tokens (no schema --
// the shared connection replaces the flat adapter's dialect/dsn pair).
func TestSignerLocalComponent_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, signerLocalComponent)
}

// TestSignerLocalComponent_DeclaresCapabilities pins the declaration: 0
// bits, the same non-declaration the seam registration records -- LocalSigner
// decrypts the private key into this process's memory for the duration of a
// signing call, so it must not claim KeyNeverLeavesBoundary.
func TestSignerLocalComponent_DeclaresCapabilities(t *testing.T) {
	t.Parallel()
	if signerLocalComponent.Capabilities != 0 {
		t.Errorf("signer.local capabilities = %v, want 0 (the documented non-declaration)", signerLocalComponent.Capabilities)
	}
	if signerLocalComponent.Capabilities.Has(pkgcore.KeyNeverLeavesBoundary) {
		t.Error("signer.local declares KeyNeverLeavesBoundary, which LocalSigner's in-process decryption does not earn")
	}
}

// TestSignerLocalComponent_ConstructsOverTheSharedConnection drives the
// component through a real assembly: it resolves the *gorm.DB the database
// provider put, builds the LocalSigner over that very connection -- not the
// second connection the flat seam adapter must open for itself -- and the
// signer's writes land in the shared database.
func TestSignerLocalComponent_ConstructsOverTheSharedConnection(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(db)); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"test.db":      nil,
			"signer.local": nil,
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}

	signer, err := pkgcore.Get[Signer](reg)
	if err != nil {
		t.Fatalf("Get[Signer] error = %v, want the constructed signer", err)
	}

	// The signer writes through the shared connection: a key it generates is
	// visible to a repository built on the same *gorm.DB.
	keyRef, _, err := signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	if _, err := NewLocalKeyRepository(db).FindByKeyRef(ctx, keyRef); err != nil {
		t.Errorf("FindByKeyRef(%q) error = %v: the signer must write through the shared connection", keyRef, err)
	}
}
