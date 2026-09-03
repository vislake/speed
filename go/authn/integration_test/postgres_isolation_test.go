//go:build integration

package authn_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newID mirrors authn's own unexported newID (model.go) for this external
// test package, which cannot reach it: both draw from the same
// application-generated-UUID convention (root CLAUDE.md's ID-generation
// rule -- gen_random_uuid() is PostgreSQL-only and banned), so the two
// never need to agree on anything beyond "a fresh UUID string".
func newID() string { return uuid.NewString() }

// countRows is this package's own countOf: a plain row count that reads
// whatever tenant is in the *gorm.DB it is handed, which for a genuinely
// unscoped model is nothing at all -- the findFn every AssertNotTenantScoped
// case below needs. It cannot import authn's unexported countOf (model_test.go,
// package authn) from this external test package.
func countRows[T any](db *gorm.DB) (int64, error) {
	var rows []T
	if err := db.Find(&rows).Error; err != nil {
		return 0, err
	}
	return int64(len(rows)), nil
}

// TestIdentityModels_AreNotTenantScoped_Postgres is the postgres-dialect leg
// of the unit tier's identical assertion (model_test.go's
// TestIdentityModels_AreNotTenantScoped and its three siblings in
// identity_test.go, mfa_test.go and verification_test.go, all package
// authn, all against SQLite): the same eight identity-domain tables, the
// same fail-if-TenantScoped check, now against a real PostgreSQL server
// where the isolation plugin and RLS session wiring genuinely apply (the
// frozen round plan's §1.9 -- SQLite structurally cannot exercise either).
func TestIdentityModels_AreNotTenantScoped_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		model    any
		createFn func(db *gorm.DB) error
		findFn   func(db *gorm.DB) (int64, error)
	}{
		{
			name:  "User",
			model: authn.User{},
			createFn: func(db *gorm.DB) error {
				address := "not-scoped-" + newID() + "@example.com"
				index := address
				return db.Create(&authn.User{
					ID: newID(), Email: address, EmailIndex: &index,
					PasswordHash: "x", Status: authn.UserStatusActive,
					CreatedAt: now, UpdatedAt: now,
				}).Error
			},
			findFn: countRows[authn.User],
		},
		{
			name:  "Session",
			model: authn.Session{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&authn.Session{
					ID: newID(), UserID: newID(), Status: authn.SessionStatusActive,
					CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
				}).Error
			},
			findFn: countRows[authn.Session],
		},
		{
			name:  "RefreshToken",
			model: authn.RefreshToken{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&authn.RefreshToken{
					ID: newID(), SessionID: newID(), UserID: newID(),
					FamilyID: newID(), TokenHash: newID(), Status: authn.RefreshTokenStatusActive,
					CreatedAt: now, ExpiresAt: now.Add(time.Hour),
				}).Error
			},
			findFn: countRows[authn.RefreshToken],
		},
		{
			name:  "LoginAttempt",
			model: authn.LoginAttempt{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&authn.LoginAttempt{
					ID: newID(), Method: authn.MethodPassword, Result: authn.LoginResultFailure,
					FailureReason: authn.FailureReasonUnknownUser, CreatedAt: now,
				}).Error
			},
			findFn: countRows[authn.LoginAttempt],
		},
		{
			name:  "UserIdentity",
			model: authn.UserIdentity{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&authn.UserIdentity{
					ID: newID(), UserID: newID(), Provider: authn.ProviderGoogle, ExternalID: newID(),
					CreatedAt: now, UpdatedAt: now,
				}).Error
			},
			findFn: countRows[authn.UserIdentity],
		},
		{
			name:  "VerificationCode",
			model: authn.VerificationCode{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&authn.VerificationCode{
					ID: newID(), Purpose: authn.VerificationPurposePhoneLogin, TargetIndex: newID(),
					CodeHash: "x", MaxAttempts: 5, Status: authn.VerificationCodeStatusActive,
					CreatedAt: now, ExpiresAt: now.Add(time.Hour),
				}).Error
			},
			findFn: countRows[authn.VerificationCode],
		},
		{
			name:  "UserMFAFactor",
			model: authn.UserMFAFactor{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&authn.UserMFAFactor{
					ID: newID(), UserID: newID(), Type: authn.MFATypeTOTP,
					Status: authn.MFAFactorStatusPending, CreatedAt: now,
				}).Error
			},
			findFn: countRows[authn.UserMFAFactor],
		},
		{
			name:  "UserRecoveryCode",
			model: authn.UserRecoveryCode{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&authn.UserRecoveryCode{
					ID: newID(), UserID: newID(), CodeHash: "x", CreatedAt: now,
				}).Error
			},
			findFn: countRows[authn.UserRecoveryCode],
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenancytest.AssertNotTenantScoped(t, db, tc.model, tc.createFn, tc.findFn)
		})
	}
}

// TestSSOConfigRepository_AssertIsolated_Postgres is the postgres-dialect
// leg of the unit tier's TestSSOConfigRepository_AssertIsolated
// (oidc_test.go, package authn, against SQLite): the ONE table this module
// owns that IS tenant data (docs/internal/05's "one TenantSSOConfig per
// tenant" rule), now proven against a real PostgreSQL server where the
// isolation plugin and RLS session wiring genuinely apply.
func TestSSOConfigRepository_AssertIsolated_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	repo := authn.NewSSOConfigRepository(db)

	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *authn.TenantSSOConfig {
		config := &authn.TenantSSOConfig{
			TenantID: string(tenant),
			ID:       newID(),
			Issuer:   "https://idp.example.com",
			ClientID: "client-id",
			Enabled:  true,
		}
		config.SetAllowedDomains([]string{"example.com"})
		return config
	})
}
