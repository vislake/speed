package attestation

import (
	"context"
	"testing"

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
