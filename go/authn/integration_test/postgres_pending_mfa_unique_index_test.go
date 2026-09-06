//go:build integration

package authn_test

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/testutil"
)

// TestSecondPendingMFAFactorRefused_Postgres is the PostgreSQL leg of the
// P3-22 regression (its SQLite twin, TestMFAFactorRepository_
// SecondPendingRowForOneUserIsRefused, lives in the unit suite): migration
// 0011's partial unique index over the status='pending' rows must exist and
// be enforced on real PostgreSQL, so a second PENDING factor for one
// (user_id, type) is refused by the database -- while a pending row may
// still coexist with an ACTIVE one (the migration-0010 design).
func TestSecondPendingMFAFactorRefused_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	repo, err := authn.NewMFAFactorRepository(db)
	if err != nil {
		t.Fatalf("NewMFAFactorRepository() error = %v", err)
	}
	if !db.Migrator().HasIndex("user_mfa_factors", "idx_user_mfa_factors_user_type_pending") {
		t.Fatal("migration 0011's pending-scope unique index does not exist after applying every migration from zero")
	}

	first := &authn.UserMFAFactor{UserID: "user-1", Type: authn.MFATypeTOTP, Secret: "first-secret", CreatedAt: time.Now()}
	if err := repo.Create(t.Context(), first); err != nil {
		t.Fatalf("create the first pending row: %v", err)
	}

	// A second pending row for the same user and type must be refused.
	second := &authn.UserMFAFactor{UserID: "user-1", Type: authn.MFATypeTOTP, Secret: "second-secret", CreatedAt: time.Now()}
	createErr := repo.Create(t.Context(), second)
	if createErr == nil {
		var rows []authn.UserMFAFactor
		if err := db.Where("user_id = ? AND type = ? AND status = ?",
			"user-1", authn.MFATypeTOTP, authn.MFAFactorStatusPending).Find(&rows).Error; err != nil {
			t.Fatalf("query pending rows: %v", err)
		}
		t.Fatalf("a second PENDING row for one (user, type) was inserted on PostgreSQL (%d rows now), want the database to refuse it (P3-22)", len(rows))
	}
	if !errors.Is(createErr, gorm.ErrDuplicatedKey) {
		t.Fatalf("the refused second insert error = %v, want gorm.ErrDuplicatedKey", createErr)
	}

	// A pending row may still coexist with an ACTIVE one of the same type.
	active := &authn.UserMFAFactor{
		UserID:      "user-1",
		Type:        authn.MFATypeTOTP,
		Secret:      "active-secret",
		Status:      authn.MFAFactorStatusActive,
		ConfirmedAt: &first.CreatedAt,
		CreatedAt:   time.Now(),
	}
	if err := repo.Create(t.Context(), active); err != nil {
		t.Fatalf("an ACTIVE row alongside the pending one was refused: %v", err)
	}
}
