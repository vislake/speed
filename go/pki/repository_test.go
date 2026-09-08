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
// proves the ExportJWKS query: given active/retiring/pending/revoked rows
// for the same purpose, asking for exactly {active, retiring} returns those
// two and excludes both pending and revoked -- the JWKS-export statement
// set is narrower than ListVerifiableByPurpose's own (which includes
// pending), per this method's own doc comment.
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

// TestSigningKeyRepository_PromoteToActive_ConcurrentCalls_ExactlyOneWinner
// is the state-arbitration pin behind the expiry scan's concurrency-safety
// claim: the pending -> active + active -> retiring transitions are two
// guarded single-statement UPDATEs inside one transaction -- each matching
// only a row still in the status the caller read -- NOT a Go-level
// read-modify-write of a row the caller would blindly save. Whatever the
// interleaving of concurrent scans (two replicas whose jobs of different
// windows overlap, the shape the windowed idempotency keys permit), exactly
// one PromoteToActive call wins for a given pending key and the losers
// report ErrKeyNotFound: no double promotion, no blind overwrite, no row
// left half-transitioned. The database arbiter is the status predicate in
// each UPDATE's WHERE clause, identical on SQLite and PostgreSQL -- SQLite
// serialization hides nothing here, because the guard lives in the
// statement, not in the dialect.
//
// The rig mirrors TestCAService_GenerateCRL_ConcurrentCalls_EveryCallLandsItsOwnNumber's
// (crl_test.go): 8 goroutines released through a closed channel, no sleeps,
// 25 trials, fresh keys per trial.
func TestSigningKeyRepository_PromoteToActive_ConcurrentCalls_ExactlyOneWinner(t *testing.T) {
	const (
		goroutines = 8
		trials     = 25
	)
	ctx := context.Background()

	for trial := 0; trial < trials; trial++ {
		repo := NewSigningKeyRepository(newTestDB(t))
		now := time.Now().UTC()
		active := newTestSigningKey("kid-active", "authn.access_token", SigningKeyStatusActive)
		if err := repo.Create(ctx, active); err != nil {
			t.Fatalf("trial %d: Create(active): %v", trial, err)
		}
		pending := newTestSigningKey("kid-pending", "authn.access_token", SigningKeyStatusPending)
		if err := repo.Create(ctx, pending); err != nil {
			t.Fatalf("trial %d: Create(pending): %v", trial, err)
		}

		errs := make([]error, goroutines)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = repo.PromoteToActive(ctx, pending.ID, active.ID, now)
			}(i)
		}
		close(start)
		wg.Wait()

		winners := 0
		for i, err := range errs {
			if err == nil {
				winners++
				continue
			}
			if !apperrIs(err, ErrKeyNotFound) {
				t.Fatalf("trial %d: PromoteToActive goroutine %d error = %v, want ErrKeyNotFound for every losing call", trial, i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("trial %d: %d of %d concurrent PromoteToActive calls succeeded, want exactly 1 -- two calls promoting the same pending key means the transition is not database-arbitrated", trial, winners, goroutines)
		}

		gotPending, err := repo.FindByID(ctx, pending.ID)
		if err != nil {
			t.Fatalf("trial %d: FindByID(pending): %v", trial, err)
		}
		if gotPending.Status != SigningKeyStatusActive {
			t.Fatalf("trial %d: pending key status after the concurrent promotion = %q, want %q", trial, gotPending.Status, SigningKeyStatusActive)
		}
		gotActive, err := repo.FindByID(ctx, active.ID)
		if err != nil {
			t.Fatalf("trial %d: FindByID(active): %v", trial, err)
		}
		if gotActive.Status != SigningKeyStatusRetiring {
			t.Fatalf("trial %d: previous active key status after the concurrent promotion = %q, want %q", trial, gotActive.Status, SigningKeyStatusRetiring)
		}
	}
}

// TestSigningKeyRepository_AssertNotTenantScoped proves pki_signing_keys is
// platform data: the tenant-scoping plugin must never filter it, and a row
// is visible regardless of which (or no) tenant is current.
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

// TestAuthorityRepository_Update_PersistsCRLFields proves the
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

// TestAuthorityRepository_UpdateCRLIfCurrent_GuardedTransition pins the
// CRL-write guard's semantics at the repository level, mirroring
// TestCertificateRepository_RevokeIfActive_GuardedTransition's shape for
// the certificate-row transition: a conditional UPDATE matching only a row
// whose CRLNumber still equals the caller's expected value. The first write
// at a fresh expected number lands; a stale expected number -- the shape a
// call whose snapshot lost a concurrent generation race carries -- lands
// nothing and leaves the row byte-for-byte as the winner's write left it;
// the fresh number then lands the next write.
func TestAuthorityRepository_UpdateCRLIfCurrent_GuardedTransition(t *testing.T) {
	repo := NewAuthorityRepository(newTestDB(t))
	ctx := context.Background()

	authority := newTestAuthority("auth-crl-guard", AuthorityTypeRoot, nil)
	if err := repo.Create(ctx, authority); err != nil {
		t.Fatalf("Create: %v", err)
	}

	issuedAt := time.Now().UTC()
	nextUpdate := issuedAt.Add(24 * time.Hour)
	winnerPEM := "-----BEGIN X509 CRL-----\nwinner\n-----END X509 CRL-----\n"
	stalePEM := "-----BEGIN X509 CRL-----\nstale-loser\n-----END X509 CRL-----\n"

	moved, err := repo.UpdateCRLIfCurrent(ctx, authority.ID, 0, 1, winnerPEM, issuedAt, nextUpdate)
	if err != nil {
		t.Fatalf("UpdateCRLIfCurrent(fresh): %v", err)
	}
	if !moved {
		t.Fatal("UpdateCRLIfCurrent(fresh expected number) moved = false, want true")
	}

	// A concurrent generator already advanced the register to 1 by the time
	// this stale call writes: its expected number 0 matches nothing, and its
	// document must not overwrite the winner's committed one.
	moved, err = repo.UpdateCRLIfCurrent(ctx, authority.ID, 0, 1, stalePEM, issuedAt, nextUpdate)
	if err != nil {
		t.Fatalf("UpdateCRLIfCurrent(stale): %v", err)
	}
	if moved {
		t.Fatal("UpdateCRLIfCurrent(stale expected number) moved = true, want false")
	}
	got, err := repo.FindByID(ctx, authority.ID)
	if err != nil {
		t.Fatalf("FindByID(after stale write): %v", err)
	}
	if got.CRLNumber != 1 || got.CRLPEM != winnerPEM {
		t.Errorf("FindByID(after stale write) = CRLNumber %d / CRLPEM %q, want the winner's 1 / %q -- the stale call overwrote the committed row", got.CRLNumber, got.CRLPEM, winnerPEM)
	}

	// The next generator reads the fresh number 1 and its write lands.
	moved, err = repo.UpdateCRLIfCurrent(ctx, authority.ID, 1, 2, stalePEM, issuedAt, nextUpdate)
	if err != nil {
		t.Fatalf("UpdateCRLIfCurrent(fresh after stale): %v", err)
	}
	if !moved {
		t.Fatal("UpdateCRLIfCurrent(fresh number after the stale write) moved = false, want true")
	}
	got, err = repo.FindByID(ctx, authority.ID)
	if err != nil {
		t.Fatalf("FindByID(after second write): %v", err)
	}
	if got.CRLNumber != 2 {
		t.Errorf("FindByID(after second write) CRLNumber = %d, want 2", got.CRLNumber)
	}
}

// TestAuthorityRepository_ListAll_ReturnsEveryAuthority proves the
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
// isolation suite against pki_certificates. Certificate is tenant data, so
// AssertIsolated -- not AssertNotTenantScoped -- is the correct half of the
// pair: a certificate issued for one tenant must never be readable,
// updatable or deletable from another one.
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

// TestCertificateRepository_RevokeIfActive_GuardedTransition pins the
// guarded single-statement transition CAService.RevokeCertificate's
// certificate-row half is built on (revocation.go): a still-active row
// moves to revoked, carrying the given reason and timestamp, and reports
// true; a row that is already revoked matches zero rows and reports
// (false, nil) with its reason and timestamp untouched -- the guard that
// keeps a concurrent loser from overwriting the winner's committed values
// -- and a row of ANOTHER tenant is equally unmatchable, the isolation
// plugin's injected tenant filter being what makes the guard cross-tenant
// safe (the same layer org's and notification's conditional transitions
// rely on; nothing here hand-writes a tenant_id filter).
func TestCertificateRepository_RevokeIfActive_GuardedTransition(t *testing.T) {
	repo := NewCertificateRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	otherCtx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-other"))

	now := time.Now().UTC()
	mkCert := func(id string) *Certificate {
		return &Certificate{
			ID:             id,
			AuthorityID:    "auth-1",
			Purpose:        "tenant.jwt_signing",
			Subject:        "CN=" + id,
			Serial:         "serial-" + id,
			CertificatePEM: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n",
			SignerName:     "local",
			KeyRef:         "keyref-" + id,
			Status:         CertificateStatusActive,
			NotBefore:      now,
			NotAfter:       now.Add(24 * time.Hour),
		}
	}
	seed := func(ctx context.Context, id string) {
		t.Helper()
		if err := repo.Create(ctx, mkCert(id)); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	seed(ctx, "cert-acme")
	seed(otherCtx, "cert-other")

	revokedAt := now.Add(time.Second)
	moved, err := repo.RevokeIfActive(ctx, "cert-acme", "compromised", revokedAt)
	if err != nil {
		t.Fatalf("RevokeIfActive(active): %v", err)
	}
	if !moved {
		t.Fatal("RevokeIfActive(active row) moved = false, want true")
	}

	got, err := repo.FindByID(ctx, "cert-acme")
	if err != nil {
		t.Fatalf("FindByID after the transition: %v", err)
	}
	if got.Status != CertificateStatusRevoked {
		t.Errorf("Status = %q after the transition, want %q", got.Status, CertificateStatusRevoked)
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(revokedAt) {
		t.Errorf("RevokedAt = %v, want the passed timestamp %v", got.RevokedAt, revokedAt)
	}
	if got.RevocationReason != "compromised" {
		t.Errorf("RevocationReason = %q, want %q", got.RevocationReason, "compromised")
	}

	// A second call against the now-revoked row matches zero rows: it must
	// report (false, nil) and leave the committed values untouched, never
	// overwrite them with the second call's own reason and timestamp.
	moved, err = repo.RevokeIfActive(ctx, "cert-acme", "superseded", revokedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("RevokeIfActive(already revoked): %v", err)
	}
	if moved {
		t.Fatal("RevokeIfActive(already revoked row) moved = true, want false")
	}
	got, err = repo.FindByID(ctx, "cert-acme")
	if err != nil {
		t.Fatalf("FindByID after the no-op call: %v", err)
	}
	if got.RevocationReason != "compromised" {
		t.Errorf("RevocationReason = %q after the no-op call, want the first call's %q unchanged", got.RevocationReason, "compromised")
	}
	if got.RevokedAt == nil || !got.RevokedAt.Equal(revokedAt) {
		t.Errorf("RevokedAt = %v after the no-op call, want the first call's %v unchanged", got.RevokedAt, revokedAt)
	}

	// Another tenant's active row is unmatchable from this tenant: the
	// injected tenant filter is part of the guard, so a cross-tenant call
	// can neither transition the row nor learn that it exists.
	moved, err = repo.RevokeIfActive(ctx, "cert-other", "compromised", revokedAt)
	if err != nil {
		t.Fatalf("RevokeIfActive(other tenant's row): %v", err)
	}
	if moved {
		t.Fatal("RevokeIfActive(other tenant's active row) moved = true, want false")
	}
	other, err := repo.FindByID(otherCtx, "cert-other")
	if err != nil {
		t.Fatalf("FindByID(other tenant's row): %v", err)
	}
	if other.Status != CertificateStatusActive {
		t.Errorf("other tenant's row Status = %q after the cross-tenant call, want %q unchanged", other.Status, CertificateStatusActive)
	}
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

// TestCertificateRevocationRepository_CertificateIDUniqueness_IsEnforcedByTheDatabase
// proves migration 0008's UNIQUE index on certificate_id is real: a second
// ledger row for a certificate that already has one must be refused by the
// database, not merely avoided by well-behaved callers -- the arbitration
// RevokeCertificate's insert-if-absent ledger write is built on (see
// repository.go's InsertIfAbsent) depends on exactly that refusal. Mirrors
// TestSigningKeyRepository_ActivePurposeUniqueness_IsEnforcedByTheDatabase's
// identical proof shape for its own partial unique index.
func TestCertificateRevocationRepository_CertificateIDUniqueness_IsEnforcedByTheDatabase(t *testing.T) {
	repo := NewCertificateRevocationRepository(newTestDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	first := &CertificateRevocation{
		ID: "rev-1", CertificateID: "cert-1", AuthorityID: "auth-1",
		Serial: "aa01", TenantID: "tenant-acme",
		RevokedAt: now, RevocationReason: "compromised",
	}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create(first ledger row): %v", err)
	}

	second := &CertificateRevocation{
		ID: "rev-2", CertificateID: "cert-1", AuthorityID: "auth-1",
		Serial: "aa01", TenantID: "tenant-acme",
		RevokedAt: now, RevocationReason: "compromised",
	}
	err := repo.Create(ctx, second)
	if err == nil {
		t.Fatalf("Create(second ledger row for the same certificate) succeeded, want a unique-constraint error")
	}

	rows, err := repo.ListByAuthority(ctx, "auth-1")
	if err != nil {
		t.Fatalf("ListByAuthority: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ledger holds %d rows after the refused duplicate insert, want exactly 1", len(rows))
	}
}

// TestCertificateRevocationRepository_InsertIfAbsent_NoOpsWhenRowExists
// proves InsertIfAbsent's arbitration verdict -- the repository half of the
// single-winner contract CAService.RevokeCertificate builds its concurrent
// revoke on (revocation.go): the first insert for a certificate reports
// (true, nil) and lands the row; the second, same certificate_id but a
// different row, reports (false, nil) and changes nothing. The no-op is
// the database's own ON CONFLICT DO NOTHING verdict, not a check-then-act
// read that two racing callers could both pass.
func TestCertificateRevocationRepository_InsertIfAbsent_NoOpsWhenRowExists(t *testing.T) {
	repo := NewCertificateRevocationRepository(newTestDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	first := &CertificateRevocation{
		ID: "rev-1", CertificateID: "cert-1", AuthorityID: "auth-1",
		Serial: "aa01", TenantID: "tenant-acme",
		RevokedAt: now, RevocationReason: "compromised",
	}
	inserted, err := repo.InsertIfAbsent(ctx, first)
	if err != nil {
		t.Fatalf("InsertIfAbsent(first): %v", err)
	}
	if !inserted {
		t.Errorf("InsertIfAbsent(first) = (false, nil), want (true, nil) -- the first insert for a certificate must win the arbitration")
	}

	duplicate := &CertificateRevocation{
		ID: "rev-2", CertificateID: "cert-1", AuthorityID: "auth-1",
		Serial: "aa01", TenantID: "tenant-acme",
		RevokedAt: now, RevocationReason: "superseded",
	}
	inserted, err = repo.InsertIfAbsent(ctx, duplicate)
	if err != nil {
		t.Fatalf("InsertIfAbsent(duplicate): %v", err)
	}
	if inserted {
		t.Errorf("InsertIfAbsent(duplicate) = (true, nil), want (false, nil) -- a second insert for the same certificate must no-op, not error and not overwrite")
	}

	rows, err := repo.ListByAuthority(ctx, "auth-1")
	if err != nil {
		t.Fatalf("ListByAuthority: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger holds %d rows after an insert and its duplicate no-op, want exactly 1", len(rows))
	}
	if rows[0].ID != "rev-1" || rows[0].RevocationReason != "compromised" {
		t.Errorf("ledger row = {id %q, reason %q} after the no-op, want the FIRST insert's row unchanged (id rev-1, reason compromised)", rows[0].ID, rows[0].RevocationReason)
	}
}

// apperrIs reports whether err is (a decorated instance of) want, matching
// on Code the way every *apperr.Error sentinel in this codebase must be
// compared -- see errors.go's own doc comment.
func apperrIs(err error, want *apperr.Error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == want.Code
}
