package pki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki/internal/testutil"
	"github.com/vislake/speed/go/pki/migrations"
)

// TestInit_RegistersSignerLocalOnTheSharedRegistry proves this package's
// init() lands "signer.local" on SignerRegistry with the capability
// LocalSigner actually has (none), mirroring
// go/pkgcore/objectstore/s3/register_test.go's identical assertion for its
// own built-in.
func TestInit_RegistersSignerLocalOnTheSharedRegistry(t *testing.T) {
	registerLocalKeySerializer()
	dsn := "file:signer_registry_init_test?mode=memory&cache=shared"
	keepAlive := openAndMigrate(t, dsn)
	defer func() { _ = keepAlive.Close() }()

	signer, caps, err := SignerRegistry.Build("signer.local", pkgcore.Config{
		"dialect": string(dbkit.DialectSQLite),
		"dsn":     dsn,
	})
	if err != nil {
		t.Fatalf(`Build("signer.local") error = %v, want nil`, err)
	}
	if signer == nil {
		t.Fatal(`Build("signer.local") returned a nil Signer`)
	}
	if caps != 0 {
		t.Errorf(`Build("signer.local") capabilities = %v, want none`, caps)
	}
}

// TestSignerRegistry_BuildSignerLocal_ProducesAWorkingSigner is the "pattern
// works end to end" proof: a Signer built purely from a name and a flat
// pkgcore.Config, with no Go construction code naming LocalSigner directly,
// generates a real key and signs with it.
func TestSignerRegistry_BuildSignerLocal_ProducesAWorkingSigner(t *testing.T) {
	registerLocalKeySerializer()
	dsn := "file:signer_registry_build_test?mode=memory&cache=shared"
	keepAlive := openAndMigrate(t, dsn)
	defer func() { _ = keepAlive.Close() }()

	signer, _, err := SignerRegistry.Build("signer.local", pkgcore.Config{
		"dialect": string(dbkit.DialectSQLite),
		"dsn":     dsn,
	})
	if err != nil {
		t.Fatalf(`Build("signer.local") error = %v, want nil`, err)
	}

	ctx := context.Background()
	keyRef, pub, err := signer.GenerateKey(ctx, AlgorithmEd25519)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	gotPub, err := signer.Public(ctx, keyRef)
	if err != nil {
		t.Fatalf("Public: %v", err)
	}
	if !bytes.Equal(gotPub.(ed25519.PublicKey), pub.(ed25519.PublicKey)) {
		t.Errorf("Public() = %x, want %x", gotPub, pub)
	}

	message := []byte("signer registry end-to-end")
	sig, err := signer.Sign(ctx, keyRef, message)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !ed25519.Verify(pub.(ed25519.PublicKey), message, sig) {
		t.Error("ed25519.Verify failed for the signature the registry-built signer produced")
	}
}

// TestLocalSignerFromConfig_MissingFieldReturnsErrMissingSeamConfig mirrors
// go/pkgcore/objectstore/s3's own per-field missing-config test.
func TestLocalSignerFromConfig_MissingFieldReturnsErrMissingSeamConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  pkgcore.Config
	}{
		{name: "missing everything", cfg: pkgcore.Config{}},
		{name: "missing dialect", cfg: pkgcore.Config{"dsn": "file::memory:?cache=shared"}},
		{name: "missing dsn", cfg: pkgcore.Config{"dialect": string(dbkit.DialectSQLite)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := localSignerFromConfig(tt.cfg)
			if !errors.Is(err, pkgcore.ErrMissingSeamConfig) {
				t.Fatalf("localSignerFromConfig(%v) error = %v, want it to wrap ErrMissingSeamConfig", tt.cfg, err)
			}
		})
	}
}

// TestLocalSignerFromConfig_UnknownDialect proves an unregistered/unknown
// dialect surfaces dbkit's own error rather than panicking -- this
// constructor never blank-imports a driver itself (see its doc comment).
func TestLocalSignerFromConfig_UnknownDialect(t *testing.T) {
	_, err := localSignerFromConfig(pkgcore.Config{
		"dialect": "does-not-exist",
		"dsn":     "irrelevant",
	})
	if err == nil {
		t.Fatal("localSignerFromConfig with an unknown dialect returned nil error, want one")
	}
}

// openAndMigrate opens dsn (SQLite) and applies this module's migrations,
// returning the underlying *sql.DB the caller must keep open for the
// duration of the test -- an in-memory, shared-cache SQLite database
// disappears once its last connection closes, and SignerRegistry.Build
// opens its own, second connection to the same dsn.
func openAndMigrate(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	testutil.Migrate(t, db, dbkit.DialectSQLite, moduleName, migrations.FS)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	return sqlDB
}

// The four tests below cover BuildSignerRequiring, the pki-local
// capability-comparison helper added to close the round-4 record that
// pkgcore.KeyNeverLeavesBoundary was declared but never compared against
// anything a caller wanted (go/pki/AGENTS.md's Known limitations). The two
// names the tests resolve are registered on the package-level SignerRegistry
// from this file, exactly the way go/pki/signer/vault and
// go/pki/signer/kmsaws register their own names from init(): a host that
// wants "signer.test.boundary"-like capability declarations registers its
// own Registration with the capability its implementation honestly has.
// fakeSigner (module_test.go) is only ever CONSTRUCTED by these tests'
// registrations -- BuildSignerRequiring never calls a method on the value
// it resolves, so the double's do-nothing bodies are never exercised.
var (
	registerSignerCapabilityTestNamesOnce sync.Once
	signerCapabilityTestNamesErr          error
)

// registerSignerCapabilityTestNames registers the two capability-test
// names on SignerRegistry exactly once per test binary.
func registerSignerCapabilityTestNames() error {
	registerSignerCapabilityTestNamesOnce.Do(func() {
		signerCapabilityTestNamesErr = errors.Join(
			SignerRegistry.Register(pkgcore.Registration[Signer]{
				Name:         "signer.test.boundary",
				Capabilities: pkgcore.KeyNeverLeavesBoundary,
				New: func(pkgcore.Config) (Signer, error) {
					return &fakeSigner{}, nil
				},
			}),
			SignerRegistry.Register(pkgcore.Registration[Signer]{
				Name:         "signer.test.unbound",
				Capabilities: 0,
				New: func(pkgcore.Config) (Signer, error) {
					return &fakeSigner{}, nil
				},
			}),
		)
	})
	return signerCapabilityTestNamesErr
}

// TestBuildSignerRequiring_RefusesARegistrationLackingTheRequiredCapability
// pins the refusal half of the capability comparison: resolving a name whose
// registration declares nothing (the honest declaration of LocalSigner and
// of the vault/kmsaws envelope names) under a KeyNeverLeavesBoundary
// requirement must fail, wrapping pkgcore.ErrCapabilityUnsatisfied with an
// error that names the signer and the missing capability -- the shape
// pkgcore.Kernel.Bootstrap's own validateSeamCapability refusal takes for
// its four built-in seams.
func TestBuildSignerRequiring_RefusesARegistrationLackingTheRequiredCapability(t *testing.T) {
	if err := registerSignerCapabilityTestNames(); err != nil {
		t.Fatalf("register capability-test names: %v", err)
	}

	signer, err := BuildSignerRequiring("signer.test.unbound", nil, pkgcore.KeyNeverLeavesBoundary)
	if signer != nil {
		t.Errorf("BuildSignerRequiring(...) returned a non-nil signer for a refused resolution, want nil (the constructed value must never be returned on refusal)")
	}
	if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
		t.Fatalf("BuildSignerRequiring(...) error = %v, want it to wrap pkgcore.ErrCapabilityUnsatisfied", err)
	}
	for _, want := range []string{"signer.test.unbound", "KeyNeverLeavesBoundary"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("BuildSignerRequiring(...) error = %q, want it to name %q", err, want)
		}
	}
}

// TestBuildSignerRequiring_SatisfiedRequirementReturnsTheSigner pins the
// acceptance half: a registration that declares the required capability
// resolves normally, and so does a zero requirement -- the shape
// DeploymentModeStandalone takes for pkgcore's own seams, where nothing is
// required of any implementation.
func TestBuildSignerRequiring_SatisfiedRequirementReturnsTheSigner(t *testing.T) {
	if err := registerSignerCapabilityTestNames(); err != nil {
		t.Fatalf("register capability-test names: %v", err)
	}

	signer, err := BuildSignerRequiring("signer.test.boundary", nil, pkgcore.KeyNeverLeavesBoundary)
	if err != nil {
		t.Fatalf("BuildSignerRequiring(boundary, KeyNeverLeavesBoundary) error = %v, want nil", err)
	}
	if signer == nil {
		t.Fatal("BuildSignerRequiring(boundary, KeyNeverLeavesBoundary) returned a nil signer, want the resolved one")
	}

	signer, err = BuildSignerRequiring("signer.test.unbound", nil, 0)
	if err != nil {
		t.Fatalf("BuildSignerRequiring(unbound, no requirement) error = %v, want nil", err)
	}
	if signer == nil {
		t.Fatal("BuildSignerRequiring(unbound, no requirement) returned a nil signer, want the resolved one")
	}
}

// TestBuildSignerRequiring_UnknownNameDelegatesToTheRegistry pins that
// BuildSignerRequiring is a wrapper, not a second registry: an unknown name
// surfaces pkgcore.ErrUnknownImplementation exactly as SignerRegistry.Build
// itself would, with no capability error invented on top.
func TestBuildSignerRequiring_UnknownNameDelegatesToTheRegistry(t *testing.T) {
	_, err := BuildSignerRequiring("signer.test.no-such-provider", nil, pkgcore.KeyNeverLeavesBoundary)
	if !errors.Is(err, pkgcore.ErrUnknownImplementation) {
		t.Errorf("BuildSignerRequiring(unknown name) error = %v, want it to wrap pkgcore.ErrUnknownImplementation", err)
	}
}

// TestBuildSignerRequiring_SignerLocalIsRefusedForKeyNeverLeavesBoundary
// resolves the registry's REAL "signer.local" entry -- the name every
// existing host resolves, whose registration declares no capability because
// LocalSigner genuinely decrypts key material into process memory to sign --
// under a KeyNeverLeavesBoundary requirement, and pins that it is refused.
// This is the end-to-end shape of the wiring mistake a mixed-up name
// resolution makes: a host that meant to wire a direct-sign name
// (signer.vault-direct, signer.aws-kms-direct) but resolved the envelope
// or local name instead gets an error at the resolution point, not a
// silently weaker composition.
func TestBuildSignerRequiring_SignerLocalIsRefusedForKeyNeverLeavesBoundary(t *testing.T) {
	registerLocalKeySerializer()
	dsn := "file:signer_registry_requiring_test?mode=memory&cache=shared"
	keepAlive := openAndMigrate(t, dsn)
	defer func() { _ = keepAlive.Close() }()
	cfg := pkgcore.Config{
		"dialect": string(dbkit.DialectSQLite),
		"dsn":     dsn,
	}

	signer, err := BuildSignerRequiring("signer.local", cfg, pkgcore.KeyNeverLeavesBoundary)
	if signer != nil {
		t.Error("BuildSignerRequiring(\"signer.local\", KeyNeverLeavesBoundary) returned a non-nil signer, want nil")
	}
	if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
		t.Errorf("BuildSignerRequiring(\"signer.local\", KeyNeverLeavesBoundary) error = %v, want it to wrap pkgcore.ErrCapabilityUnsatisfied", err)
	}

	// The same name under no requirement resolves normally -- the wrapper
	// adds a check, never a second policy.
	signer, err = BuildSignerRequiring("signer.local", cfg, 0)
	if err != nil {
		t.Fatalf("BuildSignerRequiring(\"signer.local\", no requirement) error = %v, want nil", err)
	}
	if signer == nil {
		t.Fatal("BuildSignerRequiring(\"signer.local\", no requirement) returned a nil signer")
	}
}
