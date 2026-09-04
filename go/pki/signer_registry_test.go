package pki

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
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
// works end to end" proof the round asks for: a Signer built purely from a
// name and a flat pkgcore.Config, with no Go construction code naming
// LocalSigner directly, generates a real key and signs with it.
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
