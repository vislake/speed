package pki

// Regression suite for migration 0009 (both dialects), which widens the
// key_ref columns of pki_signing_keys, pki_authorities and pki_certificates
// from VARCHAR(255) to VARCHAR(4096) -- see
// migrations/{postgres,sqlite}/0009_widen_key_ref_columns.sql for the width
// rationale. What each test below is and is not proof of is stated on the
// test itself, per the codebase's honesty rule about verification limits.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// envelopeKeyRef returns a keyRef of the exact shape
// go/pki/signer/kmsaws's envelope mode stores (signer.go): base64 of the
// whole KMS Encrypt CiphertextBlob. The blob is 2048 bytes -- the top of
// the 0.5-2KB window a real KMS symmetric ciphertext occupies when
// wrapping an ~80-byte PKCS8 ed25519 key -- which base64s to 2732
// characters (4*ceil(2048/3)), far beyond the pre-0009 width of 255. The
// exact length is arithmetic, asserted here so the fixture fails loudly if
// it ever stops representing that shape.
func envelopeKeyRef() string {
	keyRef := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, 2048))
	if len(keyRef) != 2732 {
		panic(fmt.Sprintf("pki test fixture drift: base64(2048 bytes) = %d chars, want 2732", len(keyRef)))
	}
	return keyRef
}

// TestKeyRefColumns_RoundTripEnvelopeLengthKeyRefs pins the POST-migration
// behavior migration 0009 exists to enable: each of the three widened
// tables round-trips a key_ref of envelope-mode length (2732 chars) through
// its own repository against the migrated schema, reading back exactly what
// was written.
//
// What this test is NOT is stated honestly: it cannot fail against the
// pre-0009 schema on SQLite, because SQLite does not enforce VARCHAR length
// -- an over-length write refusal is a PostgreSQL-only failure mode, and
// this module has no PostgreSQL integration tier. This test pins the other
// half: the migrated schema must admit and preserve the real envelope
// keyRef shape without truncation or error.
func TestKeyRefColumns_RoundTripEnvelopeLengthKeyRefs(t *testing.T) {
	db := newTestDB(t)
	keyRef := envelopeKeyRef()

	t.Run("SigningKey", func(t *testing.T) {
		ctx := context.Background()
		key := newTestSigningKey("sk-envelope", "authn.access_token", SigningKeyStatusActive)
		key.SignerName = "kms.aws"
		key.KeyRef = keyRef
		repo := NewSigningKeyRepository(db)
		if err := repo.Create(ctx, key); err != nil {
			t.Fatalf("Create with an envelope-length keyRef: %v", err)
		}
		got, err := repo.FindByID(ctx, key.ID)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.KeyRef != keyRef {
			t.Errorf("KeyRef read back = %d chars, want the written %d chars", len(got.KeyRef), len(keyRef))
		}
	})

	t.Run("Authority", func(t *testing.T) {
		ctx := context.Background()
		auth := newTestAuthority("auth-envelope", AuthorityTypeRoot, nil)
		auth.SignerName = "kms.aws"
		auth.KeyRef = keyRef
		repo := NewAuthorityRepository(db)
		if err := repo.Create(ctx, auth); err != nil {
			t.Fatalf("Create with an envelope-length keyRef: %v", err)
		}
		got, err := repo.FindByID(ctx, auth.ID)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.KeyRef != keyRef {
			t.Errorf("KeyRef read back = %d chars, want the written %d chars", len(got.KeyRef), len(keyRef))
		}
	})

	t.Run("Certificate", func(t *testing.T) {
		ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
		now := time.Now().UTC()
		cert := &Certificate{
			ID:             "cert-envelope",
			AuthorityID:    "auth-1",
			Purpose:        "tenant.jwt_signing",
			Subject:        "CN=cert-envelope",
			Serial:         "serial-envelope",
			CertificatePEM: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n",
			SignerName:     "kms.aws",
			KeyRef:         keyRef,
			Status:         CertificateStatusActive,
			NotBefore:      now,
			NotAfter:       now.Add(24 * time.Hour),
		}
		repo := NewCertificateRepository(db)
		if err := repo.Create(ctx, cert); err != nil {
			t.Fatalf("Create with an envelope-length keyRef: %v", err)
		}
		got, err := repo.FindByID(ctx, cert.ID)
		if err != nil {
			t.Fatalf("FindByID: %v", err)
		}
		if got.KeyRef != keyRef {
			t.Errorf("KeyRef read back = %d chars, want the written %d chars", len(got.KeyRef), len(keyRef))
		}
	})
}

// TestMigration0009_SQLiteDeclaresTheWidenedKeyRefColumns pins the one
// property SQLite genuinely records about a column's width: the declared
// type in the table definition. SQLite does not enforce VARCHAR length,
// but it does report the declared type, so this is the direct proof on
// this dialect that migration 0009 rewrote the three tables' key_ref
// columns to VARCHAR(4096) -- and it DOES fail against the pre-0009
// schema, where PRAGMA table_info reports VARCHAR(255). The length
// enforcement the widening exists for lives only on PostgreSQL, whose
// half of 0009 is a plain ALTER COLUMN TYPE this module cannot execute in
// its unit suite (no PostgreSQL integration tier; the migration follows
// the module's own dialect conventions, reviewed as such).
func TestMigration0009_SQLiteDeclaresTheWidenedKeyRefColumns(t *testing.T) {
	db := newTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}

	for _, table := range []string{tableSigningKeys, tableAuthorities, tableCertificates} {
		t.Run(table, func(t *testing.T) {
			rows, err := sqlDB.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
			if err != nil {
				t.Fatalf("PRAGMA table_info(%s): %v", table, err)
			}
			defer rows.Close()

			found := false
			for rows.Next() {
				var (
					cid      int
					name     string
					declared string
					notNull  int
					dflt     sql.NullString
					pk       int
				)
				if err := rows.Scan(&cid, &name, &declared, &notNull, &dflt, &pk); err != nil {
					t.Fatalf("scan PRAGMA row: %v", err)
				}
				if name != "key_ref" {
					continue
				}
				found = true
				if declared != "VARCHAR(4096)" {
					t.Errorf("key_ref declared type = %q, want VARCHAR(4096) after migration 0009", declared)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterating PRAGMA rows: %v", err)
			}
			if !found {
				t.Fatal("PRAGMA table_info reported no key_ref column")
			}
		})
	}
}
