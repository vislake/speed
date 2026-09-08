// service_test.go exercises the Service's observation and gate paths
// against a real pki stack -- pki's sqlite migrations applied from zero,
// real key generation and signing through LocalSigner, real certificate
// and authority rows -- with only the content opener faked, because the
// number of content opens is exactly the cost the short-circuit under
// test must avoid. The composed wire-level journey stays in
// cmd/server/attestation_flow_test.go.
package attestation

import (
	"bytes"
	"context"
	"crypto/x509/pkix"
	"embed"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/pki/migrations"
)

// testPKICipherKey is the fixed 32-byte fixture key this file registers
// pki's local-key serializer under, mirroring go/pki's own test
// registration (go/pki/repository_test.go): the host performs the same
// registration at bootstrap with a real key, and the serializer registry
// is process-global, so a test process registers once under a fixture
// key.
const testPKICipherKey = "0123456789abcdef0123456789abcdef"

var registerTestPKISerializerOnce sync.Once

// registerTestPKISerializer installs pki's pki_local_key_enc gorm
// serializer once per test process, before any LocalSigner writes a key
// row. NewCipher can only fail on key length, and the fixture key above
// is fixed at 32 bytes, so the panic branch is unreachable by
// construction.
func registerTestPKISerializer() {
	registerTestPKISerializerOnce.Do(func() {
		cipher, err := dbkit.NewCipher([]byte(testPKICipherKey))
		if err != nil {
			panic(fmt.Sprintf("attestation test: NewCipher on the fixed 32-byte fixture key: %v", err))
		}
		if err := pki.RegisterLocalKeySerializer(cipher); err != nil {
			panic(fmt.Sprintf("attestation test: RegisterLocalKeySerializer: %v", err))
		}
	})
}

// pkiMigrationModule adapts pki's embedded migration files to
// dbkit.MigrationRegistry's pkgcore.Module input -- the minimal stand-in
// for the real pki module, which this test binary cannot bootstrap.
type pkiMigrationModule struct{}

func (pkiMigrationModule) Name() string                       { return "pki" }
func (pkiMigrationModule) DependsOn() []string                { return nil }
func (pkiMigrationModule) Migrations() embed.FS               { return migrations.FS }
func (pkiMigrationModule) Locales() embed.FS                  { return embed.FS{} }
func (pkiMigrationModule) OpenAPISpec() []byte                { return nil }
func (pkiMigrationModule) Register(_ *pkgcore.Registry) error { return nil }

// applyPKIMigrations applies pki's sqlite/*.sql files to db from zero
// through dbkit.MigrationRegistry -- the same files Kernel.Bootstrap
// applies at the app's every boot, so the tables these tests read and
// write are the real ones.
func applyPKIMigrations(t *testing.T, db *gorm.DB) {
	t.Helper()
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(pkiMigrationModule{}); err != nil {
		t.Fatalf("register pki migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply pki sqlite migrations: %v", err)
	}
}

// newTestService returns a Service wired the way cmd/server wires the
// app's own (server.go): pki's migrations applied, the app's CA chain
// ensured, and every pki call running against real rows and real key
// material. Only content comes from the caller-supplied opener, since
// the tests count opens instead of reading storage bytes.
func newTestService(t *testing.T, content ContentOpener) *Service {
	t.Helper()
	registerTestPKISerializer()
	db := dbtest.NewSQLite(t)
	applyPKIMigrations(t, db)

	store := NewAttestationStore(db)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	certificates := pki.NewCertificateRepository(db)
	chain := pki.NewCAService(
		pki.NewLocalSigner(db), "local",
		pki.NewAuthorityRepository(db),
		certificates,
		pki.NewCertificateRevocationRepository(db),
	)
	svc := NewService(chain, certificates, content, store, db)
	if err := svc.EnsureAuthorityChain(context.Background()); err != nil {
		t.Fatalf("EnsureAuthorityChain: %v", err)
	}
	return svc
}

// recordingContentOpener serves one fixed content for every object it is
// asked to open and counts the opens -- the count is the assertion the
// fast-path tests make, so no real storage bytes are ever needed.
type recordingContentOpener struct {
	content []byte
	opens   int
}

func (o *recordingContentOpener) OpenContent(_ context.Context, _ string) (io.ReadCloser, error) {
	o.opens++
	return io.NopCloser(bytes.NewReader(o.content)), nil
}

// TestEnsureAttested_AttestsAnOutputOnceThenShortCircuitsEveryLaterObservation
// pins the observation hook's two legs. The first observation of an
// output -- no row on file -- opens its bytes and attests them for real:
// a tenant certificate is issued, the canonical message signed under it,
// the row written. The next observation of the same output finds the row
// under the just-issued, still-active certificate and returns without
// opening the bytes again. The open-count assertion is what guards that
// fast path: without it, every poll, enumeration or content read of an
// already-attested output would pay a full content read for a no-op.
func TestEnsureAttested_AttestsAnOutputOnceThenShortCircuitsEveryLaterObservation(t *testing.T) {
	opener := &recordingContentOpener{content: []byte("simulated smile-simulation output bytes")}
	svc := newTestService(t, opener)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	const objectID = "object-1"

	if err := svc.EnsureAttested(ctx, objectID); err != nil {
		t.Fatalf("first EnsureAttested (no row on file): %v", err)
	}
	if opener.opens != 1 {
		t.Fatalf("first EnsureAttested opened the output content %d times, want 1", opener.opens)
	}
	row, ok, err := svc.store.getByObject(ctx, objectID)
	if err != nil || !ok {
		t.Fatalf("row after the first EnsureAttested: ok %v err %v, want the attestation written", ok, err)
	}

	err = svc.EnsureAttested(ctx, objectID)
	if err != nil {
		t.Fatalf("second EnsureAttested (row on file): %v", err)
	}
	if opener.opens != 1 {
		t.Fatalf("second EnsureAttested opened the output content %d times in total, want 1 -- an already-attested observation must cost the row lookup alone, never a content read", opener.opens)
	}
	after, ok, err := svc.store.getByObject(ctx, objectID)
	if err != nil || !ok {
		t.Fatalf("row after the second EnsureAttested: ok %v err %v, want the attestation still on file", ok, err)
	}
	if after.CertificateID != row.CertificateID {
		t.Errorf("second EnsureAttested replaced the attestation (certificate %q -> %q), want it untouched", row.CertificateID, after.CertificateID)
	}
}

// TestCheckContent_RevokedCertificate_AnswersErrAttestationFailed pins
// the gate's one-error shape at its certificate stage: an object whose
// attestation row names a revoked certificate is refused, and the refusal
// satisfies ErrAttestationFailed exactly like every other refusal stage
// -- the sentinel the cmd/server sharing gate matches before it logs the
// wrapped reason (sharing_resolver.go), so a revoked certificate must
// answer the same shape as a tampered digest, never a distinguishable
// fault. The refusing stage's own coded error stays reachable through
// the wrap, for the log.
func TestCheckContent_RevokedCertificate_AnswersErrAttestationFailed(t *testing.T) {
	svc := newTestService(t, &recordingContentOpener{})
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	const objectID = "object-1"

	cert, err := svc.chain.IssueCertificate(ctx, svc.issuingAuthorityID, pki.CertificateParams{
		Purpose:  attestationCertificatePurpose,
		Subject:  pkix.Name{CommonName: "tenant-acme smile-simulation attestation"},
		NotAfter: time.Now().Add(attestationCertificateLifetime),
	})
	if err != nil {
		t.Fatalf("IssueCertificate: %v", err)
	}
	// The repository transition RevokeCertificate performs on the row --
	// enough for the gate, which refuses on the certificate row's status
	// alone before any cryptographic check runs (pki's VerifyCertificate
	// short-circuits at the status).
	moved, err := svc.certificates.RevokeIfActive(ctx, cert.ID, "test revocation", time.Now())
	if err != nil || !moved {
		t.Fatalf("RevokeIfActive: moved %v err %v, want the certificate marked revoked", moved, err)
	}
	err = svc.store.put(ctx, newTestRecord(objectID, cert.ID))
	if err != nil {
		t.Fatalf("put attestation row: %v", err)
	}

	err = svc.CheckContent(ctx, objectID, []byte("live bytes"))
	if err == nil {
		t.Fatal("CheckContent on an object whose certificate is revoked = nil, want a refusal")
	}
	if !errors.Is(err, ErrAttestationFailed) {
		t.Errorf("CheckContent error = %v, want errors.Is(err, ErrAttestationFailed) -- every refusal stage answers the one gate sentinel", err)
	}
	// The refusing stage keeps its coded identity through the wrap -- the
	// same apperr.As code check pki's own tests make of its refusals
	// (WithParam derives a fresh coded error, so errors.Is on the bare
	// sentinel never held even inside pki).
	if coded, ok := apperr.As(err); !ok || coded.Code != pki.ErrCertificateRevoked.Code {
		t.Errorf("CheckContent error = %v, want the refusing stage's code %q preserved through the wrap for the log", err, pki.ErrCertificateRevoked.Code)
	}
}
