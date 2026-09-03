//go:build integration

package authn_test

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/dbkit"
)

// newUserRepository builds an authn.UserRepository directly over db, with
// real blind indexers under testutil's fixed test key -- the same
// dbkit.NewBlindIndexer wiring authn.NewService performs internally
// (service.go), replicated here because this test needs the repository in
// isolation, without the rest of Service's machinery.
func newUserRepository(t *testing.T, db *gorm.DB) *authn.UserRepository {
	t.Helper()
	emailIndex, err := dbkit.NewBlindIndexer("email_index", testutil.BlindIndexKey(), dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(email_index): %v", err)
	}
	phoneIndex, err := dbkit.NewBlindIndexer("phone_index", testutil.BlindIndexKey(), dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(phone_index): %v", err)
	}
	repo, err := authn.NewUserRepository(db, emailIndex, phoneIndex)
	if err != nil {
		t.Fatalf("authn.NewUserRepository: %v", err)
	}
	return repo
}

// TestBlindIndex_EmailUniqueConstraint_CaseInsensitive_Postgres proves the
// users.email_index unique constraint behaves identically on PostgreSQL to
// what the unit tier already proves on SQLite (repository_test.go's blind-
// index equality lookup tests): two registrations whose email addresses
// differ only in case collide, because dbkit.NormalizeEmail lowercases
// before the HMAC is computed, so both rows would compute the SAME index
// value. This is worth its own PostgreSQL leg specifically because
// collation and unique-index case-sensitivity behaviour genuinely differ
// between the two dialects (the frozen round plan's §1.9) -- SQLite's
// default collation and PostgreSQL's both happen to treat the raw index
// BYTES as case-sensitively distinct strings, which is exactly why the
// NORMALIZATION (not the database's own collation) is what has to do the
// case-folding work, and this test is what proves that division of labor
// actually holds on the dialect that matters for production.
func TestBlindIndex_EmailUniqueConstraint_CaseInsensitive_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	repo := newUserRepository(t, db)
	ctx := context.Background()

	first := &authn.User{DisplayName: "First", Email: "Case@Example.com", PasswordHash: "x"}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}

	second := &authn.User{DisplayName: "Second", Email: "CASE@EXAMPLE.COM", PasswordHash: "x"}
	err := repo.Create(ctx, second)
	if err == nil {
		t.Fatal("Create(second, same email under different casing) succeeded, want a unique-constraint error")
	}

	found, lookupErr := repo.FindByEmail(ctx, "case@example.com")
	if lookupErr != nil {
		t.Fatalf("FindByEmail() error = %v", lookupErr)
	}
	if found.ID != first.ID {
		t.Errorf("FindByEmail() returned %s, want the first registration %s", found.ID, first.ID)
	}
}

// TestBlindIndex_PhoneUniqueConstraint_FormattingInsensitive_Postgres is the
// phone-column counterpart: two registrations whose phone numbers differ
// only in formatting collide, because dbkit.NormalizePhoneE164 canonicalizes
// before the HMAC is computed.
func TestBlindIndex_PhoneUniqueConstraint_FormattingInsensitive_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	repo := newUserRepository(t, db)
	ctx := context.Background()

	first := &authn.User{DisplayName: "First", Phone: "+1 415 555 0100", PasswordHash: "x"}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}

	second := &authn.User{DisplayName: "Second", Phone: "+14155550100", PasswordHash: "x"}
	err := repo.Create(ctx, second)
	if err == nil {
		t.Fatal("Create(second, same phone under different formatting) succeeded, want a unique-constraint error")
	}

	found, lookupErr := repo.FindByPhone(ctx, "+14155550100")
	if lookupErr != nil {
		t.Fatalf("FindByPhone() error = %v", lookupErr)
	}
	if found.ID != first.ID {
		t.Errorf("FindByPhone() returned %s, want the first registration %s", found.ID, first.ID)
	}
}
