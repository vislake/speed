// service_test.go exercises the Service's observation and gate paths
// against a real pki stack -- pki's sqlite migrations applied from zero,
// real key generation and signing through LocalSigner, real certificate
// and authority rows -- with only the content opener faked: the counting
// opener proves an already-attested observation costs no content read,
// and the scriptable opener fails each I/O stage EnsureAttested surfaces.
// The composed wire-level journey stays in flowtests/attestation_flow_test.go.
package attestation

import (
	"bytes"
	"context"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"strings"
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

// applyPKIMigrations applies pki's sqlite/*.sql files to db from zero
// through dbkit.MigrationRegistry (dbtest.Migrate) -- the same files
// the assembly applies at the app's every boot, so the tables these
// tests read and write are the real ones. The real pki module cannot carry
// its own files here: this test binary cannot bootstrap it, which is the
// case dbtest.Migration exists for.
func applyPKIMigrations(t *testing.T, db *gorm.DB) {
	t.Helper()
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: "pki", FS: migrations.FS})
}

// newTestService returns a Service wired the way internal/app's
// BuildServer wires the app's own (internal/app/server.go): pki's
// migrations applied, the app's CA chain
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
// -- the sentinel internal/app's sharing gate matches before it logs the
// wrapped reason (internal/app/sharing_resolver.go), so a revoked
// certificate must answer the same shape as a tampered digest, never a
// distinguishable fault. The refusing stage's own coded error stays
// reachable through the wrap, for the log.
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
	if !apperr.HasCode(err, pki.ErrCertificateRevoked.Code) {
		t.Errorf("CheckContent error = %v, want the refusing stage's code %q preserved through the wrap for the log", err, pki.ErrCertificateRevoked.Code)
	}
}

// fakeContentOpener is the ContentOpener seam's test implementation (the
// seam's own doc comment names tests as the direct implementers): a
// caller-populated object store whose reads can additionally be scripted
// to fail at each of the three I/O stages EnsureAttested surfaces -- the
// open, the read and the close.
type fakeContentOpener struct {
	objects  map[string][]byte
	openErr  error
	readErr  error
	closeErr error
}

// fakeResultCloser is the ReadCloser fakeContentOpener hands out, with
// scriptable Read and Close failures.
type fakeResultCloser struct {
	*bytes.Reader
	readErr  error
	closeErr error
}

func (c *fakeResultCloser) Read(p []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return c.Reader.Read(p)
}

func (c *fakeResultCloser) Close() error { return c.closeErr }

func (f *fakeContentOpener) OpenContent(ctx context.Context, objectID string) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	content, ok := f.objects[objectID]
	if !ok {
		return nil, fmt.Errorf("no such object %q", objectID)
	}
	return &fakeResultCloser{Reader: bytes.NewReader(content), readErr: f.readErr, closeErr: f.closeErr}, nil
}

func (f *fakeContentOpener) setObject(objectID string, content []byte) {
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[objectID] = content
}

// attestationFixture bundles the real service under test with the pki CA
// it signs through and the app store, so tests can reach behind the
// service's own surface where an assertion needs to (counting pki
// certificate rows, revoking one, reading an attestation row back).
type attestationFixture struct {
	svc          *Service
	chain        *pki.CAService
	store        *AttestationStore
	content      *fakeContentOpener
	certificates *pki.CertificateRepository
	authorities  *pki.AuthorityRepository
}

// newAttestationFixture returns a fully bootstrapped Service over a fresh
// per-test SQLite database: pki's own migrations applied from zero (via
// the same dbkit.MigrationRegistry the app's startup uses), the local key
// serializer registered, the app's CA chain ensured and its schema
// created -- the exact boot order internal/app's BuildServer runs. The
// service is ready for attestation writes on acme's context.
func newAttestationFixture(t *testing.T) *attestationFixture {
	t.Helper()
	registerTestPKISerializer()

	db := dbtest.NewSQLite(t)

	applyPKIMigrations(t, db)

	signer := pki.NewLocalSigner(db)
	chain := pki.NewCAService(signer, "local",
		pki.NewAuthorityRepository(db), pki.NewCertificateRepository(db),
		pki.NewCertificateRevocationRepository(db))
	content := &fakeContentOpener{}
	store := NewAttestationStore(db)
	svc := NewService(chain, pki.NewCertificateRepository(db), content, store, db)

	if err := svc.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := svc.EnsureAuthorityChain(context.Background()); err != nil {
		t.Fatalf("EnsureAuthorityChain: %v", err)
	}
	if svc.issuingAuthorityID == "" {
		t.Fatal("issuingAuthorityID empty after EnsureAuthorityChain")
	}
	return &attestationFixture{
		svc:          svc,
		chain:        chain,
		store:        store,
		content:      content,
		certificates: pki.NewCertificateRepository(db),
		authorities:  pki.NewAuthorityRepository(db),
	}
}

// tenantCtx returns ctx carrying tenant.
func tenantCtx(tenant string) context.Context {
	return pkgcore.WithTenant(context.Background(), pkgcore.TenantID(tenant))
}

// countCertificates counts every pki_certificates row across tenants,
// through raw SQL -- fixture plumbing only, never a production read path:
// certificate rows are tenant data (a repository List under one tenant's
// context could never see the other tenant's rows, which is exactly what
// this cross-tenant total must count). The same raw-statement convention
// countAllRows documents in store_test.go.
func (fx *attestationFixture) countCertificates(t *testing.T) int {
	t.Helper()
	var count int64
	if err := fx.store.db.WithContext(context.Background()).
		Raw("SELECT COUNT(*) FROM pki_certificates").Scan(&count).Error; err != nil {
		t.Fatalf("count certificates: %v", err)
	}
	return int(count)
}

// anyTenant names the tenant all per-tenant assertions in this file run
// under -- the tenant whose contexts carry attestation writes and gate
// checks (second-tenant tests derive their own contexts from it).
const anyTenant = "tenant-attestation-service-test"

// TestService_EnsureAuthorityChain_BootstrapsOnceAndIsIdempotent pins the
// two contracts of the chain boot step: a fresh database is minted its
// root plus its issuing intermediate (found again by fixed subject, the
// intermediate signed by the root), and a second boot changes nothing --
// no third authority, no second chain -- which is what makes a restart
// never mint a duplicate chain (service.go's EnsureAuthorityChain doc
// comment).
func TestService_EnsureAuthorityChain_BootstrapsOnceAndIsIdempotent(t *testing.T) {
	fx := newAttestationFixture(t)

	all, err := fx.authorities.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("after first EnsureAuthorityChain the authorities table holds %d rows, want 2 (root + intermediate)", len(all))
	}
	var root, intermediate *pki.Authority
	for i := range all {
		switch all[i].Type {
		case pki.AuthorityTypeRoot:
			root = &all[i]
		case pki.AuthorityTypeIntermediate:
			intermediate = &all[i]
		}
	}
	if root == nil || intermediate == nil {
		t.Fatalf("authorities = %+v, want one root and one intermediate", all)
	}
	if intermediate.ParentID == nil || *intermediate.ParentID != root.ID {
		t.Errorf("intermediate ParentID = %v, want the root's id %q", intermediate.ParentID, root.ID)
	}

	// A second boot must find both rows by subject and change nothing.
	if bootErr := fx.svc.EnsureAuthorityChain(context.Background()); bootErr != nil {
		t.Fatalf("second EnsureAuthorityChain: %v", bootErr)
	}
	all, err = fx.authorities.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll after second boot: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("after the second EnsureAuthorityChain the authorities table holds %d rows, want 2 -- the chain must be found, not re-minted", len(all))
	}
}

// TestService_EnsureAuthorityChain_IgnoresUnrelatedAuthorities pins
// findAuthority's matching: a row of the same type but a different subject
// -- or the right subject on the wrong type -- never satisfies the fixed
// subject lookup, so a pre-existing unrelated authority does not stop the
// app's own chain from being minted, and the app's own chain is then found
// (not duplicated) by a later boot.
func TestService_EnsureAuthorityChain_IgnoresUnrelatedAuthorities(t *testing.T) {
	fx := newAttestationFixture(t)

	// A second root with an unrelated subject, minted out from under the
	// fixed-subject lookup.
	if _, err := fx.chain.CreateRootCA(context.Background(), pki.CAParams{
		Subject:  pkix.Name{CommonName: "some other clinic CA"},
		NotAfter: time.Now().Add(attestationRootLifetime),
	}); err != nil {
		t.Fatalf("CreateRootCA (unrelated subject): %v", err)
	}

	if err := fx.svc.EnsureAuthorityChain(context.Background()); err != nil {
		t.Fatalf("EnsureAuthorityChain with an unrelated root present: %v", err)
	}
	all, err := fx.authorities.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	// The unrelated root is untouched and the app's own pair was minted
	// beside it -- three rows total, not two and not four.
	if len(all) != 3 {
		t.Fatalf("authorities table holds %d rows, want 3 (unrelated root + the app's root + intermediate)", len(all))
	}
}

// TestService_EnsureAttestedContent_FirstAttestationIssuesSignsAndStores
// drives the write-or-refresh happy path: a first observation of an output
// issues the tenant's simulation-attestation certificate, signs the
// canonical message (object id, digest, tenant) with its key and stores
// the row -- after which the sharing gate's own half (CheckContent) passes
// over the same bytes. A second observation of the same object is a no-op:
// the same certificate is reused, no second row and no second certificate
// appear.
func TestService_EnsureAttestedContent_FirstAttestationIssuesSignsAndStores(t *testing.T) {
	fx := newAttestationFixture(t)
	ctx := tenantCtx(anyTenant)
	content := []byte("simulated smile output bytes")
	fx.content.setObject("obj-1", content)

	if err := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); err != nil {
		t.Fatalf("EnsureAttestedContent: %v", err)
	}

	row, ok, err := fx.store.getByObject(ctx, "obj-1")
	if err != nil || !ok {
		t.Fatalf("getByObject(obj-1) = ok %v err %v, want the attestation row", ok, err)
	}
	if row.CertificateID == "" || row.Message == "" || row.Signature == "" {
		t.Fatalf("attestation row = %+v, want certificate, message and signature populated", row)
	}
	if fx.countCertificates(t) != 1 {
		t.Fatalf("pki_certificates holds %d rows, want exactly 1 -- one tenant certificate for the first attestation", fx.countCertificates(t))
	}

	// The gate over the same bytes passes: leaf chain verified, signature
	// over the stored message verified, live digest matches.
	if checkErr := fx.svc.CheckContent(ctx, "obj-1", content); checkErr != nil {
		t.Fatalf("CheckContent over the attested content: %v", checkErr)
	}

	// A repeated observation must not mint a second certificate or replace
	// the row.
	if repeatErr := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); repeatErr != nil {
		t.Fatalf("second EnsureAttestedContent: %v", repeatErr)
	}
	row2, ok, err := fx.store.getByObject(ctx, "obj-1")
	if err != nil || !ok {
		t.Fatalf("getByObject(obj-1) after the second observation = ok %v err %v", ok, err)
	}
	if row2.CertificateID != row.CertificateID {
		t.Errorf("second observation re-attested under certificate %q, want the reused %q", row2.CertificateID, row.CertificateID)
	}
	if fx.countCertificates(t) != 1 {
		t.Errorf("pki_certificates holds %d rows after the second observation, want 1 -- the active certificate must be reused", fx.countCertificates(t))
	}

	// A DIFFERENT object's first observation goes through
	// activeCertificateForTenant's reuse decision: the tenant's current
	// certificate is still active, so it is reused -- never a second
	// issuance per object.
	if err := fx.svc.EnsureAttestedContent(ctx, "obj-2", []byte("second output bytes")); err != nil {
		t.Fatalf("EnsureAttestedContent(obj-2): %v", err)
	}
	if fx.countCertificates(t) != 1 {
		t.Errorf("pki_certificates holds %d rows after attesting a second object, want 1 -- the tenant's active certificate is shared across its outputs", fx.countCertificates(t))
	}
}

// TestService_EnsureAttestedContent_SecondTenantGetsItsOwnCertificate
// pins per-tenant certificate issuance: a second tenant's first
// attestation issues under its own fresh certificate, never the first
// tenant's -- each tenant's outputs ride the tenant's own issuance (an
// object id is bound to its owning tenant by the storage layer the
// ContentOpener seam stands for, so the two tenants attest different
// objects). The sharing gate under each tenant passes over its own row
// and passes untouched over the other tenant's object -- the gate only
// ever reads the caller's own attestation rows, so another tenant's
// attested output is "unattested" here, never a refusal.
func TestService_EnsureAttestedContent_SecondTenantGetsItsOwnCertificate(t *testing.T) {
	fx := newAttestationFixture(t)
	acme := tenantCtx(anyTenant)
	globex := tenantCtx("tenant-attestation-other")

	if err := fx.svc.EnsureAttestedContent(acme, "obj-acme", []byte("acme's output")); err != nil {
		t.Fatalf("EnsureAttestedContent(acme): %v", err)
	}
	if err := fx.svc.EnsureAttestedContent(globex, "obj-globex", []byte("globex's output")); err != nil {
		t.Fatalf("EnsureAttestedContent(globex): %v", err)
	}

	if fx.countCertificates(t) != 2 {
		t.Fatalf("pki_certificates holds %d rows, want 2 -- one per attesting tenant", fx.countCertificates(t))
	}
	// Each tenant's gate passes over its own attested output...
	if err := fx.svc.CheckContent(acme, "obj-acme", []byte("acme's output")); err != nil {
		t.Fatalf("CheckContent(acme): %v", err)
	}
	if err := fx.svc.CheckContent(globex, "obj-globex", []byte("globex's output")); err != nil {
		t.Fatalf("CheckContent(globex): %v", err)
	}
	// ...and the gate under globex passes untouched over acme's attested
	// object: acme's row is invisible to globex's tenant-scoped lookup, so
	// the object reads as plain, unattested content -- the gate never
	// refuses on another tenant's attestation.
	if err := fx.svc.CheckContent(globex, "obj-acme", []byte("acme's output")); err != nil {
		t.Fatalf("CheckContent(globex over acme's object): %v", err)
	}
	acmeRow, ok, err := fx.store.getByObject(acme, "obj-acme")
	if err != nil || !ok {
		t.Fatalf("getByObject(acme) = ok %v err %v", ok, err)
	}
	globexRow, ok, err := fx.store.getByObject(globex, "obj-globex")
	if err != nil || !ok {
		t.Fatalf("getByObject(globex) = ok %v err %v", ok, err)
	}
	if acmeRow.CertificateID == globexRow.CertificateID {
		t.Error("both tenants' attestation rows name the same certificate, want one per tenant")
	}
}

// TestService_EnsureAttestedContent_RevokedCertificate_ReattestsUnderAFreshOne
// drives the recovery path service.go's EnsureAttestedContent doc comment
// names: a row whose certificate is no longer usable (here: revoked) is
// re-attested under a freshly issued certificate -- the row replaced, the
// old certificate's outputs shareable again -- and the gate passes again
// under the new certificate.
func TestService_EnsureAttestedContent_RevokedCertificate_ReattestsUnderAFreshOne(t *testing.T) {
	fx := newAttestationFixture(t)
	ctx := tenantCtx(anyTenant)
	content := []byte("output whose certificate gets revoked")

	if err := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); err != nil {
		t.Fatalf("EnsureAttestedContent: %v", err)
	}
	row, _, err := fx.store.getByObject(ctx, "obj-1")
	if err != nil {
		t.Fatalf("getByObject: %v", err)
	}
	originalCertificate := row.CertificateID

	if revoked, revokeErr := fx.chain.RevokeCertificate(ctx, originalCertificate, "test revocation"); revokeErr != nil || !revoked {
		t.Fatalf("RevokeCertificate = (revoked %v, err %v), want (true, nil)", revoked, revokeErr)
	}
	// The gate now refuses: chain verification fails for a revoked leaf.
	if checkErr := fx.svc.CheckContent(ctx, "obj-1", content); checkErr == nil {
		t.Fatal("CheckContent after revocation succeeded, want the revoked certificate's output refused")
	}

	if reattestErr := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); reattestErr != nil {
		t.Fatalf("EnsureAttestedContent after revocation: %v", reattestErr)
	}
	row, _, err = fx.store.getByObject(ctx, "obj-1")
	if err != nil {
		t.Fatalf("getByObject after re-attestation: %v", err)
	}
	if row.CertificateID == originalCertificate {
		t.Errorf("re-attestation kept the revoked certificate %q, want a fresh one", row.CertificateID)
	}
	if fx.countCertificates(t) != 2 {
		t.Errorf("pki_certificates holds %d rows, want 2 -- the revoked certificate and its replacement", fx.countCertificates(t))
	}
	if err := fx.svc.CheckContent(ctx, "obj-1", content); err != nil {
		t.Fatalf("CheckContent after re-attestation: %v", err)
	}
}

// TestService_EnsureAttestedContent_Refusals pins the fail-closed refusals
// that need no certificate machinery: attesting before the chain boot step
// ran is an error rather than a silent skip, a context without a tenant is
// refused before anything is written, and content over the attestation
// bound is refused rather than attested.
func TestService_EnsureAttestedContent_Refusals(t *testing.T) {
	fx := newAttestationFixture(t)
	ctx := tenantCtx(anyTenant)

	// Unbootstrapped: a Service constructed but whose EnsureAuthorityChain
	// never ran (or failed) must refuse -- the issuing authority is unknown.
	unbootstrapped := NewService(fx.chain, pki.NewCertificateRepository(fx.store.db),
		fx.content, NewAttestationStore(fx.store.db), fx.store.db)
	if err := unbootstrapped.EnsureAttestedContent(ctx, "obj-1", []byte("x")); err == nil {
		t.Fatal("EnsureAttestedContent on an unbootstrapped Service succeeded, want the authority-chain error")
	}

	// No tenant in context fails closed with pkgcore.ErrNoTenant.
	if err := fx.svc.EnsureAttestedContent(context.Background(), "obj-1", []byte("x")); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("EnsureAttestedContent(no tenant) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
	}

	// Oversized content is refused, and nothing is written.
	big := make([]byte, MaxAttestedBytes+1)
	if err := fx.svc.EnsureAttestedContent(ctx, "obj-big", big); err == nil {
		t.Fatal("EnsureAttestedContent over the attestation bound succeeded, want the size refusal")
	}
	if _, ok, err := fx.store.getByObject(ctx, "obj-big"); err != nil || ok {
		t.Errorf("getByObject(obj-big) = ok %v err %v, want no row -- the oversized output must not be attested", ok, err)
	}
}

// TestService_EnsureAttested_OpensReadsAndClosesTheOutputBytes drives the
// observation hook whole: EnsureAttested opens the output through the
// ContentOpener seam, digests what it read and attests it -- the same
// flow internal/app's observation of a succeeded simulation job runs -- and
// surfaces each I/O failure of the seam as a wrapped error without
// attesting anything.
func TestService_EnsureAttested_OpensReadsAndClosesTheOutputBytes(t *testing.T) {
	content := []byte("bytes read through the content opener")

	t.Run("happy_path_attests_the_opened_bytes", func(t *testing.T) {
		fx := newAttestationFixture(t)
		ctx := tenantCtx(anyTenant)
		fx.content.setObject("obj-1", content)

		if err := fx.svc.EnsureAttested(ctx, "obj-1"); err != nil {
			t.Fatalf("EnsureAttested: %v", err)
		}
		if err := fx.svc.CheckContent(ctx, "obj-1", content); err != nil {
			t.Fatalf("CheckContent over the attested bytes: %v", err)
		}
	})

	t.Run("open_failure_is_wrapped", func(t *testing.T) {
		fx := newAttestationFixture(t)
		fx.content.openErr = errors.New("fake open failure")
		err := fx.svc.EnsureAttested(tenantCtx(anyTenant), "obj-1")
		if err == nil || !strings.Contains(err.Error(), "fake open failure") {
			t.Fatalf("EnsureAttested with a failing opener error = %v, want the open failure wrapped", err)
		}
	})

	t.Run("read_failure_is_wrapped", func(t *testing.T) {
		fx := newAttestationFixture(t)
		ctx := tenantCtx(anyTenant)
		fx.content.setObject("obj-1", content)
		fx.content.readErr = errors.New("fake read failure")
		err := fx.svc.EnsureAttested(ctx, "obj-1")
		if err == nil || !strings.Contains(err.Error(), "fake read failure") {
			t.Fatalf("EnsureAttested with a failing read error = %v, want the read failure wrapped", err)
		}
		if _, ok, err := fx.store.getByObject(ctx, "obj-1"); err != nil || ok {
			t.Errorf("getByObject(obj-1) = ok %v err %v, want no row -- a failed read must not attest", ok, err)
		}
	})

	t.Run("close_failure_is_wrapped", func(t *testing.T) {
		fx := newAttestationFixture(t)
		ctx := tenantCtx(anyTenant)
		fx.content.setObject("obj-1", content)
		fx.content.closeErr = errors.New("fake close failure")
		err := fx.svc.EnsureAttested(ctx, "obj-1")
		if err == nil || !strings.Contains(err.Error(), "fake close failure") {
			t.Fatalf("EnsureAttested with a failing close error = %v, want the close failure wrapped", err)
		}
	})
}

// TestService_CheckContent_PassesAnUnattestedObject pins the gate's
// narrowness: an object with no attestation row -- an uploaded patient
// photo, the case this layer must never touch -- passes untouched.
func TestService_CheckContent_PassesAnUnattestedObject(t *testing.T) {
	fx := newAttestationFixture(t)
	ctx := tenantCtx(anyTenant)

	if err := fx.svc.CheckContent(ctx, "plain-photo-object", []byte("patient photo bytes")); err != nil {
		t.Fatalf("CheckContent over an unattested object: %v", err)
	}
}

// TestService_CheckContent_RefusalStages pins the gate's refusal stages
// beyond the revoked-certificate leg (covered by the revocation test
// above): a stored signature that is not valid hex is refused as an
// attestation failure before any verification, and an attestation whose
// stored message names a DIFFERENT object than the one being served is
// refused even though the signature itself is genuine -- the gate parses
// the message and compares the served object id.
func TestService_CheckContent_RefusalStages(t *testing.T) {
	fx := newAttestationFixture(t)
	ctx := tenantCtx(anyTenant)
	content := []byte("attested bytes")

	if err := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); err != nil {
		t.Fatalf("EnsureAttestedContent: %v", err)
	}
	row, ok, err := fx.store.getByObject(ctx, "obj-1")
	if err != nil || !ok {
		t.Fatalf("getByObject(obj-1) = ok %v err %v", ok, err)
	}

	// Corrupt the stored signature into non-hex: the gate's decode stage
	// refuses with ErrAttestationFailed.
	tampered := row
	tampered.Signature = "zz-not-hex"
	if err := fx.store.put(ctx, &tampered); err != nil {
		t.Fatalf("put tampered row: %v", err)
	}
	if err := fx.svc.CheckContent(ctx, "obj-1", content); !errors.Is(err, ErrAttestationFailed) {
		t.Fatalf("CheckContent over a non-hex stored signature error = %v, want errors.Is(err, ErrAttestationFailed)", err)
	}

	// Restore the genuine row, then plant the genuine message+signature on
	// a DIFFERENT object's row: the signature verifies, but the parsed
	// message names obj-1, not the obj-2 being served.
	if err := fx.store.put(ctx, &row); err != nil {
		t.Fatalf("put restored row: %v", err)
	}
	planted := row
	planted.ObjectID = "obj-2"
	planted.Message = row.Message
	planted.Signature = row.Signature
	if err := fx.store.put(ctx, &planted); err != nil {
		t.Fatalf("put planted row: %v", err)
	}
	if err := fx.svc.CheckContent(ctx, "obj-2", content); !errors.Is(err, ErrAttestationFailed) {
		t.Fatalf("CheckContent over a message naming another object error = %v, want errors.Is(err, ErrAttestationFailed)", err)
	}
}

// TestService_CertificateUsable_Decisions pins the issue-vs-reuse read's
// decision table directly: a certificate that no longer exists (a row
// never issued, or one deleted out from under the attestation) is
// unusable, an active certificate is usable, and a revoked one is not.
func TestService_CertificateUsable_Decisions(t *testing.T) {
	fx := newAttestationFixture(t)
	ctx := tenantCtx(anyTenant)

	usable, err := fx.svc.certificateUsable(ctx, "no-such-certificate")
	if err != nil {
		t.Fatalf("certificateUsable(unknown): %v", err)
	}
	if usable {
		t.Error("certificateUsable(unknown) = true, want false -- a missing row is never usable")
	}

	content := []byte("attested bytes")
	if attestErr := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); attestErr != nil {
		t.Fatalf("EnsureAttestedContent: %v", attestErr)
	}
	row, _, err := fx.store.getByObject(ctx, "obj-1")
	if err != nil {
		t.Fatalf("getByObject: %v", err)
	}

	usable, err = fx.svc.certificateUsable(ctx, row.CertificateID)
	if err != nil {
		t.Fatalf("certificateUsable(active): %v", err)
	}
	if !usable {
		t.Error("certificateUsable(active) = false, want true")
	}

	if _, revokeErr := fx.chain.RevokeCertificate(ctx, row.CertificateID, "test revocation"); revokeErr != nil {
		t.Fatalf("RevokeCertificate: %v", revokeErr)
	}
	usable, err = fx.svc.certificateUsable(ctx, row.CertificateID)
	if err != nil {
		t.Fatalf("certificateUsable(revoked): %v", err)
	}
	if usable {
		t.Error("certificateUsable(revoked) = true, want false -- revocation is terminal for the decision read")
	}
}

// TestService_EnsureAttestedContent_MissingCertificateRow_RepairsUnderAFreshOne
// pins the repair an attestation row whose pki certificate row is gone
// must take: the certificate is unusable (not revoked -- absent), so the
// next observation issues a fresh certificate and replaces the row, and
// the gate passes again. The absent-row leg is the one
// certificateUsable's record-not-found answer exists for, and it must
// read as "unusable, repair" -- never as an error that keeps every later
// observation of the output failing. The row is removed with a raw
// DELETE through the fixture's *gorm.DB -- pki ships no certificate
// delete path, so the erasure is fixture plumbing only, the same
// raw-statement convention countAllRows documents in store_test.go.
func TestService_EnsureAttestedContent_MissingCertificateRow_RepairsUnderAFreshOne(t *testing.T) {
	fx := newAttestationFixture(t)
	ctx := tenantCtx(anyTenant)
	content := []byte("output whose certificate row goes missing")

	if err := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); err != nil {
		t.Fatalf("EnsureAttestedContent: %v", err)
	}
	row, _, err := fx.store.getByObject(ctx, "obj-1")
	if err != nil {
		t.Fatalf("getByObject: %v", err)
	}
	originalCertificate := row.CertificateID

	if deleteErr := fx.store.db.WithContext(context.Background()).
		Exec("DELETE FROM pki_certificates WHERE id = ?", originalCertificate).Error; deleteErr != nil {
		t.Fatalf("raw delete of the certificate row: %v", deleteErr)
	}

	// The decision read must answer "unusable" -- not an error -- and the
	// observation must repair the attestation under a fresh certificate.
	usable, err := fx.svc.certificateUsable(ctx, originalCertificate)
	if err != nil {
		t.Fatalf("certificateUsable(missing row) error = %v, want (false, nil) -- an absent certificate is unusable, not an error", err)
	}
	if usable {
		t.Fatal("certificateUsable(missing row) = true, want false")
	}

	if reattestErr := fx.svc.EnsureAttestedContent(ctx, "obj-1", content); reattestErr != nil {
		t.Fatalf("EnsureAttestedContent with the certificate row gone: %v", reattestErr)
	}
	row, _, err = fx.store.getByObject(ctx, "obj-1")
	if err != nil {
		t.Fatalf("getByObject after repair: %v", err)
	}
	if row.CertificateID == originalCertificate {
		t.Errorf("repair kept the deleted certificate %q, want a fresh issuance", row.CertificateID)
	}
	if err := fx.svc.CheckContent(ctx, "obj-1", content); err != nil {
		t.Fatalf("CheckContent after the repair: %v", err)
	}
}
