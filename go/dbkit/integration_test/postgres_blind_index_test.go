//go:build integration

package dbkit_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// This file exercises the blind-index mechanism (blind_index.go) against a
// real PostgreSQL 16, mirroring the SQLite round-trip test in the parent
// package (blind_index_test.go) on the production dialect. The unit tier
// already proves normalization, key binding, error paths, and tenant-plugin
// composition; this tier exists to prove the one thing SQLite cannot: that
// the mechanism's column layout — BYTEA ciphertext for the encrypted field,
// VARCHAR(64) HMAC hex for the index — round-trips and queries through a
// real Postgres, including an index on the blind-index column matching the
// way a versioned migration would actually declare it.

// postgresBlindIndexAccount is the identity-domain shape the blind-index
// mechanism exists for: a model without a tenant of its own (like a users
// table, where the login identifier lives — see
// docs/internal/04-data-and-tenancy.md's data-domain table), whose email is
// encrypted at rest and queryable only through its blind-index column. No
// TenantScoped method and no tenant_id column: dbkit's tenant-scoping plugin
// must leave it alone, exactly as it leaves platform and identity data
// alone.
type postgresBlindIndexAccount struct {
	ID         string `gorm:"primaryKey;size:26"`
	Email      string `gorm:"column:email;serializer:dbkit_it_blind_email_enc"`
	EmailIndex string `gorm:"column:email_index;size:64;not null"`
}

// TableName pins the table name explicitly, matching the raw CREATE TABLE
// this file applies. The literal serializer name in the tag must match the
// name registered in the test below.
func (postgresBlindIndexAccount) TableName() string { return "postgres_blind_index_accounts" }

// blindIndexTestKey derives a deterministic 32-byte key from label, the
// same shape dbkit.NewCipher and dbkit.NewBlindIndexer require. Distinct
// labels always produce distinct keys; hashing instead of literal key bytes
// keeps the source free of anything that could be mistaken for a real
// secret.
func blindIndexTestKey(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

// TestBlindIndexer_Postgres_RoundTripAndEqualityLookup is the real-Postgres
// counterpart of the SQLite round-trip test in the parent package: writes
// through the encryption serializer plus an explicitly computed index
// column, reads back the raw at-rest bytes, and drives equality lookups
// through the index with messy, human-typed input.
func TestBlindIndexer_Postgres_RoundTripAndEqualityLookup(t *testing.T) {
	ctx := context.Background()
	pgContainer := startPostgresContainer(t, ctx)
	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open(DialectPostgres) error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1) // search_path is session-scoped; see openWidgetDB's comment

	const schema = "dbkit_it_blind_index"
	if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if err := db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})
	if err := db.Exec("SET search_path TO " + schema).Error; err != nil {
		t.Fatalf("set search_path to %s: %v", schema, err)
	}

	// The encrypted column is BYTEA and the index column is VARCHAR(64) —
	// 64 hex characters of HMAC-SHA256 — with a real index on the latter,
	// the shape a versioned migration would generate for this model.
	if err := db.Exec(`CREATE TABLE postgres_blind_index_accounts (
		id          VARCHAR(26) PRIMARY KEY,
		email       BYTEA NOT NULL,
		email_index VARCHAR(64) NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create postgres_blind_index_accounts: %v", err)
	}
	if err := db.Exec(`CREATE INDEX postgres_blind_index_accounts_email_index_idx
		ON postgres_blind_index_accounts (email_index)`).Error; err != nil {
		t.Fatalf("create email_index index: %v", err)
	}

	cipher, err := dbkit.NewCipher(blindIndexTestKey("it-blind-encryption"))
	if err != nil {
		t.Fatalf("dbkit.NewCipher() error = %v", err)
	}
	dbkit.RegisterEncryptedSerializer("dbkit_it_blind_email_enc", cipher)

	indexer, err := dbkit.NewBlindIndexer("email_index", blindIndexTestKey("it-blind-index"), dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer() error = %v", err)
	}

	// Seed three accounts: one with a canonical write and a duplicate of it,
	// one whose write side carried messy input (spacing and case), so both
	// directions of normalization are exercised against real Postgres.
	seed := func(id, rawEmail string) postgresBlindIndexAccount {
		t.Helper()
		indexValue, err := indexer.Index(rawEmail)
		if err != nil {
			t.Fatalf("Index(%q) error = %v", rawEmail, err)
		}
		m := postgresBlindIndexAccount{ID: id, Email: rawEmail, EmailIndex: indexValue}
		if err := db.Create(&m).Error; err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
		return m
	}
	seed("it-acc-1", "User@Example.COM")
	seed("it-acc-2", "User@Example.COM") // duplicate: same canonical email, distinct account
	seed("it-acc-3", " Owner@Acme.TEST ")

	t.Run("AtRest_EmailIsCiphertext_IndexIsCanonicalHMAC", func(t *testing.T) {
		var stored struct {
			Email      []byte
			EmailIndex string
		}
		if err := db.Raw(`SELECT email, email_index FROM postgres_blind_index_accounts WHERE id = 'it-acc-1'`).Scan(&stored).Error; err != nil {
			t.Fatalf("raw at-rest read: %v", err)
		}
		if decrypted, err := cipher.Decrypt(stored.Email); err != nil {
			t.Errorf("cipher.Decrypt(email column) error = %v; the BYTEA column must hold serializer ciphertext", err)
		} else if string(decrypted) != "User@Example.COM" {
			t.Errorf("decrypted email column = %q, want %q", decrypted, "User@Example.COM")
		}
		want := dbkit.BlindIndex(blindIndexTestKey("it-blind-index"), "user@example.com")
		if stored.EmailIndex != want {
			t.Errorf("stored email_index = %q, want %q (the column must hold the index of the normalized value)", stored.EmailIndex, want)
		}
	})

	t.Run("EqualityLookup_NormalizesMessyQueryInput", func(t *testing.T) {
		cond, err := indexer.Equal("  USER@example.com  ")
		if err != nil {
			t.Fatalf("Equal() error = %v", err)
		}
		var got postgresBlindIndexAccount
		if err := db.Where(cond).First(&got).Error; err != nil {
			t.Fatalf("First(Equal(variant)) error = %v; the variant must normalize to the stored index", err)
		}
		if got.ID != "it-acc-1" {
			t.Errorf("First(Equal(variant)) = %+v, want the row with id it-acc-1", got)
		}
		if got.Email != "User@Example.COM" {
			t.Errorf("First(Equal(variant)).Email = %q, want the stored plaintext %q", got.Email, "User@Example.COM")
		}

		// The duplicate rows with the same canonical email are both matched:
		// the index is an equality aid on a shared table, not a uniqueness
		// constraint.
		var all []postgresBlindIndexAccount
		if err := db.Where(cond).Find(&all).Error; err != nil {
			t.Fatalf("Find(Equal) error = %v", err)
		}
		if len(all) != 2 {
			t.Errorf("Find(Equal) returned %d rows, want 2 (it-acc-1 and it-acc-2 share one canonical index value)", len(all))
		}

		// The account seeded with messy write-side input is found by its
		// canonical query form, and decrypts back to exactly what was stored.
		cond3, err := indexer.Equal("owner@acme.test")
		if err != nil {
			t.Fatalf("Equal(messy-write account) error = %v", err)
		}
		var third postgresBlindIndexAccount
		if err := db.Where(cond3).First(&third).Error; err != nil {
			t.Fatalf("First(Equal) for the messy-write account error = %v", err)
		}
		if third.ID != "it-acc-3" || third.Email != " Owner@Acme.TEST " {
			t.Errorf("First(Equal) = %+v, want it-acc-3 with plaintext %q", third, " Owner@Acme.TEST ")
		}
	})

	t.Run("LookupUnderDifferentNormalizer_MatchesNothing", func(t *testing.T) {
		// A second indexer bound to the same column under an identity
		// normalizer computes its condition over the raw string instead of
		// the column's canonical form — the one way to produce a mismatch,
		// visible in the bootstrap code rather than reachable from a query
		// call site. Against the mixed-case row it must return zero rows,
		// not tenant B's data, not a wrong row, not an error.
		identity := func(raw string) (string, error) { return raw, nil }
		other, err := dbkit.NewBlindIndexer("email_index", blindIndexTestKey("it-blind-index"), identity)
		if err != nil {
			t.Fatalf("NewBlindIndexer(identity normalizer) error = %v", err)
		}
		cond, err := other.Equal("User@Example.COM")
		if err != nil {
			t.Fatalf("Equal() under the identity normalizer error = %v", err)
		}
		var got postgresBlindIndexAccount
		if err := db.Where(cond).First(&got).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Errorf("First(Equal under a different normalizer) error = %v, want gorm.ErrRecordNotFound (an index computed under different normalization must not match)", err)
		}
	})

	t.Run("UnindexableInput_FailsClosedOnQuerySide", func(t *testing.T) {
		// An empty input has no canonical form: Equal refuses to build a
		// condition rather than emitting one that could never match, or —
		// worse — that would match rows some broken writer stored an empty
		// index for.
		if cond, err := indexer.Equal(""); err == nil {
			t.Errorf("Equal(\"\") = %+v, nil error; want an error (an empty input must never become a query condition)", cond)
		}
	})
}
