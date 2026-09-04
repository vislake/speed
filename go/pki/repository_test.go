package pki

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/pki/internal/testutil"
	"github.com/vislake/speed/go/pki/migrations"
)

// testLocalKeyCipherKey is the fixed 32-byte fixture key this file registers
// LocalKeySerializerName under, mirroring go/notification/contact_test.go's
// identical registerContactSerializer pattern.
const testLocalKeyCipherKey = "0123456789abcdef0123456789abcdef"

var registerLocalKeySerializerOnce sync.Once

// registerLocalKeySerializer installs the pki_local_key_enc gorm serializer
// once per test process, the same registration the host performs at
// bootstrap with a real key. The serializer registry is process-global, so
// the Once keeps repeated registrations from churning it; NewCipher can
// only fail on key length, and the fixture key above is fixed 32 bytes, so
// the panic branch is unreachable by construction rather than a real
// failure path.
func registerLocalKeySerializer() {
	registerLocalKeySerializerOnce.Do(func() {
		cipher, err := dbkit.NewCipher([]byte(testLocalKeyCipherKey))
		if err != nil {
			panic(fmt.Sprintf("pki test: NewCipher on the fixed 32-byte fixture key: %v", err))
		}
		if err := RegisterLocalKeySerializer(cipher); err != nil {
			panic(fmt.Sprintf("pki test: RegisterLocalKeySerializer: %v", err))
		}
	})
}

// newTestDB returns a fresh, per-call SQLite *gorm.DB with this module's
// migrations applied from zero, and LocalKeySerializerName registered.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	registerLocalKeySerializer()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

// --- SigningKeyRepository -------------------------------------------------

func newTestSigningKey(id, purpose, status string) *SigningKey {
	now := time.Now().UTC()
	return &SigningKey{
		ID:         id,
		Purpose:    purpose,
		Algorithm:  AlgorithmEd25519,
		SignerName: "local",
		KeyRef:     "keyref-" + id,
		Status:     status,
		PublicKey:  []byte{0x01, 0x02, 0x03},
		NotBefore:  now,
		NotAfter:   now.Add(24 * time.Hour),
	}
}

func TestSigningKeyRepository_CreateAndFindByID(t *testing.T) {
	repo := NewSigningKeyRepository(newTestDB(t))
	ctx := context.Background()

	key := newTestSigningKey("kid-1", "authn.access_token", SigningKeyStatusActive)
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.FindByID(ctx, "kid-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Purpose != key.Purpose || got.Algorithm != key.Algorithm {
		t.Errorf("FindByID = %+v, want purpose/algorithm to match %+v", got, key)
	}
}

func TestSigningKeyRepository_FindByID_NotFound(t *testing.T) {
	repo := NewSigningKeyRepository(newTestDB(t))
	if _, err := repo.FindByID(context.Background(), "does-not-exist"); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("FindByID(missing) error = %v, want ErrKeyNotFound", err)
	}
}

func TestSigningKeyRepository_FindActiveByPurpose(t *testing.T) {
	repo := NewSigningKeyRepository(newTestDB(t))
	ctx := context.Background()

	if _, err := repo.FindActiveByPurpose(ctx, "authn.access_token"); !apperrIs(err, ErrNoActiveKey) {
		t.Fatalf("FindActiveByPurpose(none yet) error = %v, want ErrNoActiveKey", err)
	}

	active := newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive)
	if err := repo.Create(ctx, active); err != nil {
		t.Fatalf("Create(active): %v", err)
	}
	// A revoked key for the same purpose must never be returned as active.
	revoked := newTestSigningKey("kid-revoked", "authn.access_token", SigningKeyStatusRevoked)
	if err := repo.Create(ctx, revoked); err != nil {
		t.Fatalf("Create(revoked): %v", err)
	}

	got, err := repo.FindActiveByPurpose(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("FindActiveByPurpose: %v", err)
	}
	if got.ID != "kid-active" {
		t.Errorf("FindActiveByPurpose = %q, want %q", got.ID, "kid-active")
	}
}

// TestSigningKeyRepository_ActivePurposeUniqueness_IsEnforcedByTheDatabase
// proves the migration's partial unique index is real: a second row for the
// same purpose already holding SigningKeyStatusActive must be refused by
// the database, not merely avoided by well-behaved callers.
func TestSigningKeyRepository_ActivePurposeUniqueness_IsEnforcedByTheDatabase(t *testing.T) {
	repo := NewSigningKeyRepository(newTestDB(t))
	ctx := context.Background()

	if err := repo.Create(ctx, newTestSigningKey("kid-1", "authn.access_token", SigningKeyStatusActive)); err != nil {
		t.Fatalf("Create(first active): %v", err)
	}
	err := repo.Create(ctx, newTestSigningKey("kid-2", "authn.access_token", SigningKeyStatusActive))
	if err == nil {
		t.Fatalf("Create(second active for the same purpose) succeeded, want a unique-constraint error")
	}
}

func TestSigningKeyRepository_ListVerifiableByPurpose_ExcludesRevoked(t *testing.T) {
	repo := NewSigningKeyRepository(newTestDB(t))
	ctx := context.Background()

	for _, k := range []*SigningKey{
		newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive),
		newTestSigningKey("kid-retiring", "authn.access_token", SigningKeyStatusRetiring),
		newTestSigningKey("kid-revoked", "authn.access_token", SigningKeyStatusRevoked),
	} {
		if err := repo.Create(ctx, k); err != nil {
			t.Fatalf("Create(%s): %v", k.ID, err)
		}
	}

	got, err := repo.ListVerifiableByPurpose(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ListVerifiableByPurpose: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, k := range got {
		ids[k.ID] = true
	}
	if !ids["kid-active"] || !ids["kid-retiring"] {
		t.Errorf("ListVerifiableByPurpose = %v, want kid-active and kid-retiring present", ids)
	}
	if ids["kid-revoked"] {
		t.Errorf("ListVerifiableByPurpose returned the revoked key, want it excluded")
	}
}

// TestSigningKeyRepository_ListByPurposeAndStatuses_FiltersByExactStatusSet
// proves round 3's ExportJWKS query: given active/retiring/pending/revoked
// rows for the same purpose, asking for exactly {active, retiring} returns
// those two and excludes both pending and revoked -- the JWKS-export
// statement set is narrower than ListVerifiableByPurpose's own (which
// includes pending), per this method's own doc comment.
func TestSigningKeyRepository_ListByPurposeAndStatuses_FiltersByExactStatusSet(t *testing.T) {
	repo := NewSigningKeyRepository(newTestDB(t))
	ctx := context.Background()

	for _, k := range []*SigningKey{
		newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive),
		newTestSigningKey("kid-retiring", "authn.access_token", SigningKeyStatusRetiring),
		newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending),
		newTestSigningKey("kid-revoked", "authn.access_token", SigningKeyStatusRevoked),
	} {
		if err := repo.Create(ctx, k); err != nil {
			t.Fatalf("Create(%s): %v", k.ID, err)
		}
	}

	got, err := repo.ListByPurposeAndStatuses(ctx, "authn.access_token", SigningKeyStatusActive, SigningKeyStatusRetiring)
	if err != nil {
		t.Fatalf("ListByPurposeAndStatuses: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, k := range got {
		ids[k.ID] = true
	}
	if len(ids) != 2 || !ids["kid-active"] || !ids["kid-retiring"] {
		t.Errorf("ListByPurposeAndStatuses = %v, want exactly kid-active and kid-retiring", ids)
	}
	if ids["kid-pending"] || ids["kid-revoked"] {
		t.Errorf("ListByPurposeAndStatuses returned pending or revoked, want neither: %v", ids)
	}
}

// TestSigningKeyRepository_AssertNotTenantScoped proves pki_signing_keys is
// platform data (docs/internal/04-data-and-tenancy.md): the tenant-scoping
// plugin must never filter it, and a row is visible regardless of which (or
// no) tenant is current.
func TestSigningKeyRepository_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	n := 0
	createFn := func(db *gorm.DB) error {
		n++
		return db.Create(newTestSigningKey(fmt.Sprintf("kid-scope-%d", n), fmt.Sprintf("purpose-%d", n), SigningKeyStatusActive)).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var count int64
		err := db.Model(&SigningKey{}).Count(&count).Error
		return count, err
	}
	tenancytest.AssertNotTenantScoped(t, db, SigningKey{}, createFn, findFn)
}

// --- AuthorityRepository ---------------------------------------------------

func newTestAuthority(id, authorityType string, parentID *string) *Authority {
	now := time.Now().UTC()
	return &Authority{
		ID:             id,
		Type:           authorityType,
		ParentID:       parentID,
		Subject:        "CN=Test CA " + id,
		Serial:         "aa" + id,
		CertificatePEM: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n",
		SignerName:     "local",
		KeyRef:         "keyref-" + id,
		Status:         AuthorityStatusActive,
		NotBefore:      now,
		NotAfter:       now.Add(24 * time.Hour),
	}
}

func TestAuthorityRepository_CreateAndFindByID(t *testing.T) {
	repo := NewAuthorityRepository(newTestDB(t))
	ctx := context.Background()

	authority := newTestAuthority("auth-1", AuthorityTypeRoot, nil)
	if err := repo.Create(ctx, authority); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.FindByID(ctx, "auth-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Subject != authority.Subject || got.ParentID != nil {
		t.Errorf("FindByID = %+v, want subject %q and nil ParentID", got, authority.Subject)
	}
}

func TestAuthorityRepository_FindByID_NotFound(t *testing.T) {
	repo := NewAuthorityRepository(newTestDB(t))
	if _, err := repo.FindByID(context.Background(), "does-not-exist"); !apperrIs(err, ErrAuthorityNotFound) {
		t.Errorf("FindByID(missing) error = %v, want ErrAuthorityNotFound", err)
	}
}

// TestAuthorityRepository_AssertNotTenantScoped proves pki_authorities is
// platform data, the identical property SigningKeyRepository's own test
// proves.
func TestAuthorityRepository_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	n := 0
	createFn := func(db *gorm.DB) error {
		n++
		return db.Create(newTestAuthority(fmt.Sprintf("auth-scope-%d", n), AuthorityTypeRoot, nil)).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var count int64
		err := db.Model(&Authority{}).Count(&count).Error
		return count, err
	}
	tenancytest.AssertNotTenantScoped(t, db, Authority{}, createFn, findFn)
}

// TestAuthorityRepository_Update_PersistsCRLFields proves round 3's
// GenerateCRL persistence path: a full-Save Update round-trips the CRL*
// fields FindByID loaded and GenerateCRL would mutate.
func TestAuthorityRepository_Update_PersistsCRLFields(t *testing.T) {
	repo := NewAuthorityRepository(newTestDB(t))
	ctx := context.Background()

	authority := newTestAuthority("auth-crl", AuthorityTypeRoot, nil)
	if err := repo.Create(ctx, authority); err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := repo.FindByID(ctx, "auth-crl")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	now := time.Now().UTC()
	loaded.CRLNumber = 1
	loaded.CRLPEM = "-----BEGIN X509 CRL-----\ntest\n-----END X509 CRL-----\n"
	loaded.CRLIssuedAt = &now
	loaded.CRLNextUpdate = &now
	if err = repo.Update(ctx, loaded); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.FindByID(ctx, "auth-crl")
	if err != nil {
		t.Fatalf("FindByID(after update): %v", err)
	}
	if got.CRLNumber != 1 || got.CRLPEM != loaded.CRLPEM {
		t.Errorf("FindByID(after update) = %+v, want CRLNumber=1 and CRLPEM=%q", got, loaded.CRLPEM)
	}
	if got.CRLIssuedAt == nil || got.CRLNextUpdate == nil {
		t.Errorf("FindByID(after update) CRLIssuedAt/CRLNextUpdate = %v/%v, want both set", got.CRLIssuedAt, got.CRLNextUpdate)
	}
}

// TestAuthorityRepository_ListAll_ReturnsEveryAuthority proves round 3's
// RegenerateAllCRLs query: every authority, regardless of type.
func TestAuthorityRepository_ListAll_ReturnsEveryAuthority(t *testing.T) {
	repo := NewAuthorityRepository(newTestDB(t))
	ctx := context.Background()

	root := newTestAuthority("auth-root", AuthorityTypeRoot, nil)
	if err := repo.Create(ctx, root); err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	intermediate := newTestAuthority("auth-intermediate", AuthorityTypeIntermediate, &root.ID)
	if err := repo.Create(ctx, intermediate); err != nil {
		t.Fatalf("Create(intermediate): %v", err)
	}

	got, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAll returned %d authorities, want 2", len(got))
	}
	ids := map[string]bool{}
	for _, a := range got {
		ids[a.ID] = true
	}
	if !ids["auth-root"] || !ids["auth-intermediate"] {
		t.Errorf("ListAll = %v, want auth-root and auth-intermediate", ids)
	}
}

// --- LocalKeyRepository -----------------------------------------------------

func TestLocalKeyRepository_CreateFindDelete(t *testing.T) {
	repo := NewLocalKeyRepository(newTestDB(t))
	ctx := context.Background()

	key := &LocalKey{KeyRef: "keyref-1", Algorithm: AlgorithmEd25519, EncryptedPrivateKey: "fake-pkcs8-bytes"}
	if err := repo.Create(ctx, key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.FindByKeyRef(ctx, "keyref-1")
	if err != nil {
		t.Fatalf("FindByKeyRef: %v", err)
	}
	if got.EncryptedPrivateKey != "fake-pkcs8-bytes" {
		t.Errorf("FindByKeyRef round-trip = %q, want %q", got.EncryptedPrivateKey, "fake-pkcs8-bytes")
	}

	if err := repo.Delete(ctx, "keyref-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.FindByKeyRef(ctx, "keyref-1"); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("FindByKeyRef(after delete) error = %v, want ErrKeyNotFound", err)
	}
	if err := repo.Delete(ctx, "keyref-1"); !apperrIs(err, ErrKeyNotFound) {
		t.Errorf("Delete(already deleted) error = %v, want ErrKeyNotFound", err)
	}
}

// TestLocalKeyRepository_AssertNotTenantScoped proves pki_local_keys is
// platform data, the identical property the other two platform tables'
// tests prove.
func TestLocalKeyRepository_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	n := 0
	createFn := func(db *gorm.DB) error {
		n++
		return db.Create(&LocalKey{
			KeyRef:              fmt.Sprintf("keyref-scope-%d", n),
			Algorithm:           AlgorithmEd25519,
			EncryptedPrivateKey: "fake-pkcs8-bytes",
		}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var count int64
		err := db.Model(&LocalKey{}).Count(&count).Error
		return count, err
	}
	tenancytest.AssertNotTenantScoped(t, db, LocalKey{}, createFn, findFn)
}

// --- CertificateRepository --------------------------------------------------

// TestCertificateRepository_AssertIsolated runs the mandatory tenant-
// isolation suite against pki_certificates. Certificate is tenant data
// (docs/internal/04-data-and-tenancy.md), so AssertIsolated -- not
// AssertNotTenantScoped -- is the correct half of the pair: a certificate
// issued for one tenant must never be readable, updatable or deletable from
// another one.
func TestCertificateRepository_AssertIsolated(t *testing.T) {
	repo := NewCertificateRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Certificate {
		n++
		now := time.Now().UTC()
		return &Certificate{
			ID:             fmt.Sprintf("cert-%d", n),
			AuthorityID:    "auth-1",
			Purpose:        "tenant.jwt_signing",
			Subject:        fmt.Sprintf("CN=cert-%d", n),
			Serial:         fmt.Sprintf("serial-%d", n),
			CertificatePEM: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n",
			SignerName:     "local",
			KeyRef:         fmt.Sprintf("keyref-%d", n),
			Status:         CertificateStatusActive,
			NotBefore:      now,
			NotAfter:       now.Add(24 * time.Hour),
		}
	})
}

// --- CertificateRevocationRepository -----------------------------------------

// TestCertificateRevocationRepository_CreateAndListByAuthority proves round
// 3's ledger write and its per-authority read -- the query GenerateCRL
// (crl.go) drives.
func TestCertificateRevocationRepository_CreateAndListByAuthority(t *testing.T) {
	repo := NewCertificateRevocationRepository(newTestDB(t))
	ctx := context.Background()

	for _, rev := range []*CertificateRevocation{
		{ID: "rev-1", CertificateID: "cert-1", AuthorityID: "auth-1", Serial: "aa01", TenantID: "tenant-acme", RevokedAt: time.Now().UTC(), RevocationReason: "compromised"},
		{ID: "rev-2", CertificateID: "cert-2", AuthorityID: "auth-1", Serial: "aa02", TenantID: "tenant-acme", RevokedAt: time.Now().UTC(), RevocationReason: "superseded"},
		{ID: "rev-3", CertificateID: "cert-3", AuthorityID: "auth-2", Serial: "aa03", TenantID: "tenant-other", RevokedAt: time.Now().UTC(), RevocationReason: "compromised"},
	} {
		if err := repo.Create(ctx, rev); err != nil {
			t.Fatalf("Create(%s): %v", rev.ID, err)
		}
	}

	got, err := repo.ListByAuthority(ctx, "auth-1")
	if err != nil {
		t.Fatalf("ListByAuthority: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByAuthority(auth-1) returned %d rows, want 2", len(got))
	}
	serials := map[string]bool{}
	for _, r := range got {
		serials[r.Serial] = true
		if r.AuthorityID != "auth-1" {
			t.Errorf("ListByAuthority(auth-1) returned a row for authority %q", r.AuthorityID)
		}
	}
	if !serials["aa01"] || !serials["aa02"] {
		t.Errorf("ListByAuthority(auth-1) = %v, want serials aa01 and aa02", serials)
	}

	empty, err := repo.ListByAuthority(ctx, "auth-unknown")
	if err != nil {
		t.Fatalf("ListByAuthority(unknown): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListByAuthority(unknown authority) = %d rows, want 0", len(empty))
	}
}

// TestCertificateRevocationRepository_AssertNotTenantScoped proves
// pki_certificate_revocations is platform data despite carrying a TenantID
// column -- CertificateRevocation's own model.go doc comment explains why
// that column is informational only, the identical treatment
// send_records/platform_blacklist/AuditEvent already get.
func TestCertificateRevocationRepository_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	n := 0
	createFn := func(db *gorm.DB) error {
		n++
		return db.Create(&CertificateRevocation{
			ID:               fmt.Sprintf("rev-scope-%d", n),
			CertificateID:    fmt.Sprintf("cert-scope-%d", n),
			AuthorityID:      "auth-1",
			Serial:           fmt.Sprintf("serial-scope-%d", n),
			TenantID:         "tenant-acme",
			RevokedAt:        time.Now().UTC(),
			RevocationReason: "test",
		}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var count int64
		err := db.Model(&CertificateRevocation{}).Count(&count).Error
		return count, err
	}
	tenancytest.AssertNotTenantScoped(t, db, CertificateRevocation{}, createFn, findFn)
}

// apperrIs reports whether err is (a decorated instance of) want, matching
// on Code the way every *apperr.Error sentinel in this codebase must be
// compared -- see errors.go's own doc comment.
func apperrIs(err error, want *apperr.Error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == want.Code
}
