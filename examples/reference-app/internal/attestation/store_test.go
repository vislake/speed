package attestation

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestStore returns an AttestationStore over a fresh SQLite database
// with its schema ensured -- the store-only fixture store tests need
// (message verification and the full service path are covered by
// message_test.go and the composed flow tests in cmd/server).
func newTestStore(t *testing.T) *AttestationStore {
	t.Helper()
	store := NewAttestationStore(dbtest.NewSQLite(t))
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return store
}

// newTestRecord builds the attestation-row shape tests write, with the
// tenant populated by the ctx at put time (the plugin overwrites it
// anyway).
func newTestRecord(objectID, certificateID string) *attestationRecord {
	return &attestationRecord{
		ObjectID:      objectID,
		CertificateID: certificateID,
		Message:       `{"object_id":"` + objectID + `","content_sha256":"abc","tenant_id":"tenant-acme"}`,
		Signature:     "deadbeef",
	}
}

// TestAttestationStore_PutAndGetByObject_TenantScoped pins the store's
// two contracts at once: a written row reads back under the writing
// tenant, and is invisible -- "no row", never an error -- under another
// tenant, exactly the isolation every tenant-data table in this app is
// required to prove.
func TestAttestationStore_PutAndGetByObject_TenantScoped(t *testing.T) {
	store := newTestStore(t)
	acme := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	globex := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-globex"))

	if err := store.put(acme, newTestRecord("obj-1", "cert-1")); err != nil {
		t.Fatalf("put: %v", err)
	}

	row, ok, err := store.getByObject(acme, "obj-1")
	if err != nil {
		t.Fatalf("getByObject(acme): %v", err)
	}
	if !ok {
		t.Fatal("getByObject(acme) found no row for obj-1")
	}
	if row.CertificateID != "cert-1" || row.ObjectID != "obj-1" {
		t.Errorf("row = %+v, want cert-1 / obj-1", row)
	}
	if row.TenantID != "tenant-acme" {
		t.Errorf("row tenant_id = %q, want tenant-acme (the ctx tenant, never a caller-supplied value)", row.TenantID)
	}

	if _, ok, err := store.getByObject(globex, "obj-1"); err != nil {
		t.Fatalf("getByObject(globex): %v", err)
	} else if ok {
		t.Error("getByObject(globex) found acme's row -- the store is not tenant-isolated")
	}
	if _, ok, err := store.getByObject(acme, "no-such-object"); err != nil {
		t.Fatalf("getByObject(missing): %v", err)
	} else if ok {
		t.Error("getByObject(missing) reported a row")
	}
}

// TestAttestationStore_Put_ReplacesTheObjectsRow pins the upsert: a
// second put for the same object under the same tenant replaces the
// row's certificate/message/signature in place -- the re-attestation
// shape a revoked certificate drives -- rather than erroring on the
// primary key or leaving two rows.
func TestAttestationStore_Put_ReplacesTheObjectsRow(t *testing.T) {
	store := newTestStore(t)
	acme := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))

	if err := store.put(acme, newTestRecord("obj-1", "cert-1")); err != nil {
		t.Fatalf("first put: %v", err)
	}
	replacement := newTestRecord("obj-1", "cert-2")
	replacement.Signature = "cafebabe"
	if err := store.put(acme, replacement); err != nil {
		t.Fatalf("second put: %v", err)
	}

	if count := countAllRows(t, store); count != 1 {
		t.Fatalf("after the replacement put the table holds %d rows, want 1", count)
	}
	row, ok, err := store.getByObject(acme, "obj-1")
	if err != nil {
		t.Fatalf("getByObject after replacement: %v", err)
	}
	if !ok {
		t.Fatal("getByObject after replacement found no row")
	}
	if row.CertificateID != "cert-2" || row.Signature != "cafebabe" {
		t.Errorf("replaced row = %+v, want the cert-2 / cafebabe replacement", row)
	}
}

// TestAttestationStore_LatestCertificate pins the per-tenant
// newest-row certificate answer EnsureAttested's issue-vs-reuse decision
// reads: the most recently written row's certificate for the tenant, and
// no answer for a tenant with no rows -- and never another tenant's
// certificate.
func TestAttestationStore_LatestCertificate(t *testing.T) {
	store := newTestStore(t)
	acme := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	globex := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-globex"))

	if _, ok, err := store.latestCertificate(acme); err != nil {
		t.Fatalf("latestCertificate(empty): %v", err)
	} else if ok {
		t.Error("latestCertificate(empty tenant) reported a certificate")
	}

	if err := store.put(acme, newTestRecord("obj-1", "cert-1")); err != nil {
		t.Fatalf("put obj-1: %v", err)
	}
	if err := store.put(acme, newTestRecord("obj-2", "cert-2")); err != nil {
		t.Fatalf("put obj-2: %v", err)
	}

	certificate, ok, err := store.latestCertificate(acme)
	if err != nil {
		t.Fatalf("latestCertificate(acme): %v", err)
	}
	if !ok || certificate != "cert-2" {
		t.Errorf("latestCertificate(acme) = %q/%v, want cert-2/true (the newer row's certificate)", certificate, ok)
	}

	if _, ok, err := store.latestCertificate(globex); err != nil {
		t.Fatalf("latestCertificate(globex): %v", err)
	} else if ok {
		t.Error("latestCertificate(globex) reported acme's certificate -- not tenant-isolated")
	}
}

// countAllRows counts every row of the attestations table across tenants,
// through raw SQL -- store-test plumbing only, never a production read
// path (raw statements are the one family dbkit's tenant plugin cannot
// intercept, which is exactly why only a test helper uses one here).
func countAllRows(t *testing.T, store *AttestationStore) int64 {
	t.Helper()
	var count int64
	if err := store.db.WithContext(context.Background()).
		Raw("SELECT COUNT(*) FROM " + attestationsTable).Scan(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

// isoTenantA, isoTenantB and isoTenantC are the tenants
// TestAttestationStore_AssertIsolated exercises. Fixed values, not derived
// from the test name: the suite runs against a fresh per-test database
// whose only rows are the ones it writes, so no cross-test collision is
// possible (the same reasoning internal/smilesim's own equivalent suite
// documents for its constants -- unlike the tenancytest suite, which must
// derive its tenants because it cannot assume an empty table).
const (
	isoTenantA = "attestation-iso-tenant-a"
	isoTenantB = "attestation-iso-tenant-b"
	isoTenantC = "attestation-iso-tenant-c"
)

// isoObjectID and isoCertificateID build one tenant's n-th attested
// object's id and its certificate id: unique per call (object ids are the
// row's primary key), self-describing in failure output.
func isoObjectID(tenant string, n int) string {
	return fmt.Sprintf("attestation-iso-%s-object-%d", tenant, n)
}

func isoCertificateID(tenant string, n int) string {
	return fmt.Sprintf("attestation-iso-%s-cert-%d", tenant, n)
}

// TestAttestationStore_AssertIsolated is AttestationStore's equivalent of
// the mandatory tenancytest.AssertIsolated suite (root CLAUDE.md's
// multi-tenant isolation rule; the suite tools/check_repo_isolation.py
// requires every tenant-data repository to run). It cannot run the
// tenancytest suite itself: AssertIsolated reflects T's exported "ID"
// field and queries the "id" column, while attestationRecord's primary key
// is the go/storage object id of the attested output -- application
// generated and globally unique, never a per-tenant "id" column
// (record.go's own doc comment gives the reasoning). The suite below
// therefore asserts, through AttestationStore's real surface, every
// isolation property tenancytest.AssertIsolated pins that this
// put/getByObject/latestCertificate shape has: rows saved under one tenant
// are visible to that tenant's lookups and to no other tenant's -- the
// other tenant answers "no row", never an error; the newest-row
// certificate answer for one tenant is never another tenant's certificate,
// even when the other tenant's rows are the table's newest; put overwrites
// a caller-forged TenantID with the context tenant; and a context carrying
// no tenant fails closed with pkgcore.ErrNoTenant before the database is
// touched. This is the suite named TestAttestationStore_AssertIsolated
// that check_repo_isolation.py recognizes as coverage for the
// AttestationStore embedding.
func TestAttestationStore_AssertIsolated(t *testing.T) {
	store := newTestStore(t)

	ctxA := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(isoTenantA))
	ctxB := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(isoTenantB))

	// Two tenants attest their own objects -- distinct object ids, since an
	// object id is globally unique across tenants (a go/storage object id;
	// record.go's primary-key reasoning) -- tenant B's rows written after
	// tenant A's, so the table's globally-newest rows belong to B. Tenant C
	// never writes: it is the no-rows tenant the empty-answer assertions
	// read.
	aObjects := make([]string, 0, 2)
	for n := 1; n <= 2; n++ {
		objectID := isoObjectID(isoTenantA, n)
		if err := store.put(ctxA, newTestRecord(objectID, isoCertificateID(isoTenantA, n))); err != nil {
			t.Fatalf("put(tenant %q, record %d) error = %v", isoTenantA, n, err)
		}
		aObjects = append(aObjects, objectID)
	}
	bObjects := make([]string, 0, 2)
	for n := 1; n <= 2; n++ {
		objectID := isoObjectID(isoTenantB, n)
		if err := store.put(ctxB, newTestRecord(objectID, isoCertificateID(isoTenantB, n))); err != nil {
			t.Fatalf("put(tenant %q, record %d) error = %v", isoTenantB, n, err)
		}
		bObjects = append(bObjects, objectID)
	}

	t.Run("get_by_object_scopes_to_the_calling_tenant", func(t *testing.T) {
		t.Helper()
		for n := 1; n <= 2; n++ {
			assertGetOwnedBy(t, store, ctxA, isoTenantA, isoObjectID(isoTenantA, n), isoCertificateID(isoTenantA, n))
			assertGetOwnedBy(t, store, ctxB, isoTenantB, isoObjectID(isoTenantB, n), isoCertificateID(isoTenantB, n))
		}
		for n := 1; n <= 2; n++ {
			assertGetDenied(t, store, ctxB, isoTenantB, isoObjectID(isoTenantA, n))
			assertGetDenied(t, store, ctxA, isoTenantA, isoObjectID(isoTenantB, n))
		}
		// The "no row" answer is the same for an object another tenant owns
		// and for an object nobody owns -- the store never discloses which
		// other tenants attested, and a miss is never an error.
		assertGetDenied(t, store, ctxA, isoTenantA, "no-such-object")
	})

	t.Run("latest_certificate_scopes_to_the_calling_tenant", func(t *testing.T) {
		t.Helper()
		// Tenant B's rows are the table's newest (written last) -- A's
		// answer must still be A's own newest certificate, which pins that
		// the created_at-descending newest-row scan never reaches across
		// tenants, and B's answer its own.
		certificate, ok, err := store.latestCertificate(ctxA)
		if err != nil {
			t.Fatalf("latestCertificate(tenant %q) error = %v", isoTenantA, err)
		}
		if !ok {
			t.Fatal("latestCertificate(tenant A) = no certificate, want A's own newest")
		}
		if certificate != isoCertificateID(isoTenantA, 2) {
			t.Errorf("latestCertificate(tenant %q) = %q, want %q (A's newest row, not B's globally-newer one)",
				isoTenantA, certificate, isoCertificateID(isoTenantA, 2))
		}
		certificate, ok, err = store.latestCertificate(ctxB)
		if err != nil {
			t.Fatalf("latestCertificate(tenant %q) error = %v", isoTenantB, err)
		}
		if !ok {
			t.Fatal("latestCertificate(tenant B) = no certificate, want B's own newest")
		}
		if certificate != isoCertificateID(isoTenantB, 2) {
			t.Errorf("latestCertificate(tenant %q) = %q, want %q (B's newest row)", isoTenantB, certificate, isoCertificateID(isoTenantB, 2))
		}
		// A tenant with no rows gets no answer -- never another tenant's
		// certificate.
		if _, ok, err := store.latestCertificate(pkgcore.WithTenant(context.Background(), pkgcore.TenantID(isoTenantC))); err != nil {
			t.Fatalf("latestCertificate(tenant %q) error = %v", isoTenantC, err)
		} else if ok {
			t.Errorf("latestCertificate(tenant %q) = a certificate, want none -- no row of its own exists", isoTenantC)
		}
	})

	t.Run("put_overwrites_a_forged_tenant_id_with_the_context_tenant", func(t *testing.T) {
		t.Helper()
		// A caller forging TenantID on the record it hands to put must not
		// be able to land a row under the forged tenant: put runs inside
		// dbkit.WithTenantSession, where the tenant-scoping plugin forces
		// the tenant_id column to the ctx tenant on every create,
		// overwriting whatever the caller populated (store.go's put doc
		// comment) -- the same property tenancytest.AssertIsolated's own
		// forged-tenant check proves for the generic base.
		objectID := isoObjectID(isoTenantA, 99)
		forged := newTestRecord(objectID, "attestation-iso-forged-cert")
		forged.TenantModel = dbkit.TenantModel{TenantID: isoTenantB} // forged
		if err := store.put(ctxA, forged); err != nil {
			t.Fatalf("put(tenant %q, forged-TenantID record) error = %v", isoTenantA, err)
		}

		row, ok, err := store.getByObject(ctxA, objectID)
		if err != nil || !ok {
			t.Fatalf("getByObject(tenant %q, forged record's object) = ok %v err %v, want the row under the CONTEXT tenant", isoTenantA, ok, err)
		}
		if row.GetTenantID() != pkgcore.TenantID(isoTenantA) {
			t.Errorf("forged record landed under tenant %q, want the context tenant %q", row.GetTenantID(), isoTenantA)
		}
		if _, ok, err := store.getByObject(ctxB, objectID); err != nil || ok {
			t.Errorf("getByObject(forged tenant %q, forged record's object) = ok %v err %v, want none -- the forged tenant must never see the row", isoTenantB, ok, err)
		}
	})

	t.Run("no_tenant_in_context_fails_closed", func(t *testing.T) {
		t.Helper()
		noTenant := context.Background()

		if err := store.put(noTenant, newTestRecord(isoObjectID(isoTenantA, 100), "attestation-iso-no-tenant-cert")); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("put(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
		if _, _, err := store.getByObject(noTenant, aObjects[0]); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("getByObject(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
		if _, _, err := store.latestCertificate(noTenant); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("latestCertificate(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
	})
}

// assertGetOwnedBy asserts that getByObject(ctx, objectID) returns the row
// the owning tenant saved: the object is found, its tenant_id column names
// owner (never a caller-supplied value), and its certificate is the one
// that tenant attested with.
func assertGetOwnedBy(t *testing.T, store *AttestationStore, ctx context.Context, tenant, objectID, certificateID string) {
	t.Helper()
	row, ok, err := store.getByObject(ctx, objectID)
	if err != nil {
		t.Fatalf("getByObject(tenant %q, %q) error = %v", tenant, objectID, err)
	}
	if !ok {
		t.Fatalf("getByObject(tenant %q, %q) = no row, want the row that tenant saved", tenant, objectID)
	}
	if row.GetTenantID() != pkgcore.TenantID(tenant) {
		t.Errorf("getByObject(tenant %q, %q).GetTenantID() = %q, want %q", tenant, objectID, row.GetTenantID(), tenant)
	}
	if row.CertificateID != certificateID {
		t.Errorf("getByObject(tenant %q, %q).CertificateID = %q, want %q", tenant, objectID, row.CertificateID, certificateID)
	}
}

// assertGetDenied asserts that getByObject(ctx, objectID), called under a
// tenant that does not own objectID, answers "no row" -- ok false with a
// nil error, the store's documented convention -- never the row and never
// a different, more revealing error.
func assertGetDenied(t *testing.T, store *AttestationStore, ctx context.Context, tenant, objectID string) {
	t.Helper()
	row, ok, err := store.getByObject(ctx, objectID)
	if err != nil {
		t.Fatalf("getByObject(tenant %q, %q) error = %v, want (no row, nil error)", tenant, objectID, err)
	}
	if ok {
		t.Errorf("getByObject(tenant %q, %q) = a row (%+v), want none -- the object belongs to another tenant", tenant, objectID, row)
	}
}
