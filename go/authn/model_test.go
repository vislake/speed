package authn

import (
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// TestIdentityModels_AreNotTenantScoped is the mandatory isolation assertion
// for this module's data domain, and it is the reason it is worth reading
// twice: every table authn owns in this block is IDENTITY data, so the
// property being proved is the opposite of the usual one. A users row must
// stay visible whatever tenant is in the calling context, because a person
// belongs to several tenants; a users table that accidentally acquired tenant
// filtering would present in production as "the account vanished", which is
// among the most expensive bugs to diagnose.
//
// AssertNotTenantScoped fails outright if the Go type implements
// dbkit.TenantScoped, so this also catches the specific mistake of embedding
// dbkit.TenantModel into one of these structs.
func TestIdentityModels_AreNotTenantScoped(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	email := "not-scoped@example.com"

	cases := []struct {
		name     string
		model    any
		createFn func(db *gorm.DB) error
		findFn   func(db *gorm.DB) (int64, error)
	}{
		{
			name:  "User",
			model: User{},
			createFn: func(db *gorm.DB) error {
				address := email + "." + newID()
				index := address
				return db.Create(&User{
					ID: newID(), Email: address, EmailIndex: &index,
					PasswordHash: "x", Status: UserStatusActive,
					CreatedAt: now, UpdatedAt: now,
				}).Error
			},
			findFn: countOf[User],
		},
		{
			name:  "Session",
			model: Session{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&Session{
					ID: newID(), UserID: newID(), Status: SessionStatusActive,
					CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
				}).Error
			},
			findFn: countOf[Session],
		},
		{
			name:  "RefreshToken",
			model: RefreshToken{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&RefreshToken{
					ID: newID(), SessionID: newID(), UserID: newID(),
					FamilyID: newID(), TokenHash: newID(), Status: RefreshTokenStatusActive,
					CreatedAt: now, ExpiresAt: now.Add(time.Hour),
				}).Error
			},
			findFn: countOf[RefreshToken],
		},
		{
			name:  "LoginAttempt",
			model: LoginAttempt{},
			createFn: func(db *gorm.DB) error {
				return db.Create(&LoginAttempt{
					ID: newID(), Method: MethodPassword, Result: LoginResultFailure,
					FailureReason: FailureReasonUnknownUser, CreatedAt: now,
				}).Error
			},
			findFn: countOf[LoginAttempt],
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenancytest.AssertNotTenantScoped(t, db, tc.model, tc.createFn, tc.findFn)
		})
	}
}

// countOf is the findFn AssertNotTenantScoped needs: a plain row count that
// reads whatever tenant is in the *gorm.DB it is handed, which for a
// genuinely unscoped model is nothing at all.
func countOf[T any](db *gorm.DB) (int64, error) {
	var rows []T
	if err := db.Find(&rows).Error; err != nil {
		return 0, err
	}
	return int64(len(rows)), nil
}

// TestIdentityModels_DoNotImplementTenantScoped states the same requirement
// at the type level, without a database.
//
// AssertNotTenantScoped above already fails on a model that implements the
// interface, but only when someone remembers to add the model to its table.
// This test enumerates the types independently, so a fifth identity model
// added later without a row in that table is still checked here -- and the
// failure message says exactly what went wrong rather than "count mismatch".
func TestIdentityModels_DoNotImplementTenantScoped(t *testing.T) {
	t.Parallel()

	scoped := reflect.TypeOf((*dbkit.TenantScoped)(nil)).Elem()
	for _, model := range []any{User{}, Session{}, RefreshToken{}, LoginAttempt{}} {
		typ := reflect.TypeOf(model)
		if typ.Implements(scoped) || reflect.PointerTo(typ).Implements(scoped) {
			t.Errorf("%s implements dbkit.TenantScoped; identity-domain models must not, because one person belongs to several tenants", typ.Name())
		}
	}
}

// TestSession_SetAMRRoundTripsThroughAMRList pins the delimited encoding, so a
// later change to it cannot silently break step-up policy, which decides by
// reading this list.
func TestSession_SetAMRRoundTripsThroughAMRList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []string
		want  []string
		wantS string
	}{
		{name: "empty", input: nil, want: []string{}, wantS: ""},
		{name: "single", input: []string{MethodPassword}, want: []string{"password"}, wantS: "password"},
		{
			name:  "several",
			input: []string{"password", "mfa:totp"},
			want:  []string{"password", "mfa:totp"},
			wantS: "password mfa:totp",
		},
		{
			name:  "empty entries are dropped rather than stored as a double space",
			input: []string{"password", "", "mfa:totp"},
			want:  []string{"password", "mfa:totp"},
			wantS: "password mfa:totp",
		},
		{
			name:  "an entry containing whitespace is split, never stored verbatim",
			input: []string{"password mfa:totp"},
			want:  []string{"password", "mfa:totp"},
			wantS: "password mfa:totp",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Session
			s.SetAMR(tc.input)
			if s.AMR != tc.wantS {
				t.Errorf("stored AMR = %q, want %q", s.AMR, tc.wantS)
			}
			if got := s.AMRList(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("AMRList() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestSerializerName_MatchesTheTestutilCopy pins the duplicated constant in
// internal/testutil, which cannot import this package (import cycle) and so
// carries its own copy of the serializer name. Without this test the two
// could drift and every encrypted column would silently stop being encrypted.
func TestSerializerName_MatchesTheTestutilCopy(t *testing.T) {
	t.Parallel()

	// The testutil package registers the cipher under its own copy of the
	// name; a mismatch shows up as a schema-parse failure the moment an
	// encrypted column is written, which this proves cannot happen.
	db := testutil.NewDB(t)
	address := "serializer@example.com"
	index := "serializer-index"
	if err := db.Create(&User{
		ID: newID(), Email: address, EmailIndex: &index,
		PasswordHash: "x", Status: UserStatusActive,
	}).Error; err != nil {
		t.Fatalf("writing an encrypted column failed, which means the serializer registered under %q is not the one the model asks for: %v", SerializerName, err)
	}
}

// TestUser_EmailIsEncryptedAtRest proves the column holds ciphertext rather
// than the address. It reads the raw column with a scan into a byte slice, so
// the serializer never gets a chance to decrypt it on the way out.
func TestUser_EmailIsEncryptedAtRest(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	address := "ciphertext@example.com"
	index := "ciphertext-index"
	user := &User{ID: newID(), Email: address, EmailIndex: &index, PasswordHash: "x", Status: UserStatusActive}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	var stored []byte
	if err := db.Raw("SELECT email FROM users WHERE id = ?", user.ID).Row().Scan(&stored); err != nil {
		t.Fatalf("read the raw email column: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("the raw email column is empty; nothing was stored")
	}
	if string(stored) == address {
		t.Fatal("the raw email column holds the plaintext address; the encrypted serializer is not applied")
	}

	var readBack User
	if err := db.Where("id = ?", user.ID).First(&readBack).Error; err != nil {
		t.Fatalf("read the user back: %v", err)
	}
	if readBack.Email != address {
		t.Fatalf("decrypted email = %q, want %q", readBack.Email, address)
	}
}
