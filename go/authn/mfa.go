package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/authn/internal/totp"
)

// MFA factor types; 'totp' is the only shipped type.
const (
	// MFATypeTOTP is a time-based one-time password factor
	// (RFC 6238, internal/totp).
	MFATypeTOTP = "totp"
)

// MFA factor status values.
const (
	// MFAFactorStatusPending is an enrolled-but-not-yet-confirmed factor.
	// It cannot be used to verify anything: confirming it once, with a
	// real code from the authenticator app it was provisioned into, is
	// what proves the secret actually reached a working device rather
	// than being abandoned mid-setup.
	MFAFactorStatusPending = "pending"
	// MFAFactorStatusActive is a confirmed, usable factor.
	MFAFactorStatusActive = "active"
)

// AMR values a second factor contributes, alongside MethodPassword,
// MethodSocial, MethodOIDC (model.go) and MethodSMS (verification.go).
const (
	// MethodMFATOTP is a step-up satisfied by a TOTP code. A TOTP factor
	// does not serve as a sign-in channel of its own.
	MethodMFATOTP = "mfa:totp"
	// MethodMFARecoveryCode is a step-up satisfied by a recovery code.
	MethodMFARecoveryCode = "mfa:recovery_code"
)

// recoveryCodeCount is how many recovery codes ConfirmTOTP and
// RegenerateRecoveryCodes generate.
const recoveryCodeCount = 10

// totpSkewSteps is how many adjacent 30-second time steps totp.Validate
// tolerates in either direction, absorbing ordinary clock drift between the
// server and the device running the authenticator app.
const totpSkewSteps = 1

// UserMFAFactor is a user's enrolled second factor. IDENTITY-domain data,
// like User: MFA belongs to the person, not to a tenant they act inside.
type UserMFAFactor struct {
	// ID is an application-generated UUID.
	ID string `gorm:"primaryKey;size:36"`

	// UserID is the owning user. Unique together with Type AMONG ACTIVE
	// ROWS ONLY -- see idx_user_mfa_factors_user_type (migration 0010), a
	// partial unique index scoped to status='active' -- which is what lets
	// a fresh PENDING replacement factor coexist with the still-ACTIVE
	// factor it will eventually replace, and unique among PENDING rows
	// too, under the twin partial index
	// idx_user_mfa_factors_user_type_pending (migration 0011), which is
	// what keeps two racing enrollments from leaving two pending rows for
	// a confirm to take the wrong one (see
	// MFAFactorRepository.ReplacePending). GORM's uniqueIndex struct tag
	// cannot express a WHERE-qualified index, so, like go/pki's
	// identically-shaped SigningKey.Purpose and go/rbac's RoleBinding,
	// the constraint lives in the migration SQL only, not here. See
	// Service.EnrollTOTP and MFAFactorRepository.Confirm.
	UserID string `gorm:"column:user_id;size:36;not null"`

	// Type is one of the MFAType* constants.
	Type string `gorm:"size:32;not null"`

	// Secret is the factor's shared secret (base32 for TOTP), encrypted
	// at rest. It is returned by an API exactly once, at enrollment.
	Secret string `gorm:"serializer:authn_pii"`

	// Status is one of the MFAFactorStatus* constants.
	Status string `gorm:"size:16;not null"`

	// LastUsedStep is the RFC 6238 time-step counter of the most recently
	// ACCEPTED code, and is what makes a code single-use: accepting one
	// requires the new step to be strictly greater than this value. See
	// totp.Validate's own doc comment for why the matched step, not
	// merely "now", is what must be recorded.
	LastUsedStep int64 `gorm:"column:last_used_step;not null"`

	CreatedAt time.Time `gorm:"autoCreateTime;not null"`

	// ConfirmedAt is nil while Status is pending.
	ConfirmedAt *time.Time `gorm:"column:confirmed_at"`
}

// TableName pins the table name.
func (UserMFAFactor) TableName() string { return "user_mfa_factors" }

// MFAFactorRepository reads and writes the user_mfa_factors table.
type MFAFactorRepository struct {
	db *gorm.DB
}

// NewMFAFactorRepository binds db.
func NewMFAFactorRepository(db *gorm.DB) (*MFAFactorRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewMFAFactorRepository requires a database handle")
	}
	return &MFAFactorRepository{db: db}, nil
}

// Create inserts f, filling in its ID and Status when empty.
func (r *MFAFactorRepository) Create(ctx context.Context, f *UserMFAFactor) error {
	if f.ID == "" {
		f.ID = newID()
	}
	if f.Status == "" {
		f.Status = MFAFactorStatusPending
	}
	return r.db.WithContext(ctx).Create(f).Error
}

// FindActiveByUserAndType returns userID's ACTIVE factor of the given type,
// or ErrNotFound. A PENDING replacement factor can coexist with the
// still-ACTIVE factor it will eventually replace (see EnrollTOTP and
// Confirm), so a status-less "find the one row" lookup would be ambiguous
// exactly while a replacement is in progress -- every caller that means
// "the factor that actually works" (step-up verification, gating
// recovery-code regeneration, deciding whether EnrollTOTP needs a step-up)
// wants this one.
func (r *MFAFactorRepository) FindActiveByUserAndType(ctx context.Context, userID, factorType string) (*UserMFAFactor, error) {
	var f UserMFAFactor
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND type = ? AND status = ?", userID, factorType, MFAFactorStatusActive).
		First(&f).Error
	if err != nil {
		return nil, translate(err)
	}
	return &f, nil
}

// FindPendingByUserAndType returns userID's PENDING factor of the given
// type, or ErrNotFound -- the in-progress enrollment ConfirmTOTP needs to
// complete, never an already-active one.
func (r *MFAFactorRepository) FindPendingByUserAndType(ctx context.Context, userID, factorType string) (*UserMFAFactor, error) {
	var f UserMFAFactor
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND type = ? AND status = ?", userID, factorType, MFAFactorStatusPending).
		First(&f).Error
	if err != nil {
		return nil, translate(err)
	}
	return &f, nil
}

// ReplacePending removes userID's PENDING factor of the given type, if any,
// and inserts f in its place, atomically (one transaction per attempt).
// EnrollTOTP runs every enrollment through this method.
//
// The retrying transaction exists because two plain statements would let
// two rapid enroll requests interleave their deletes and creates (del,
// del, insert, insert) and leave TWO pending rows for one (user, type) --
// the unique index is scoped to ACTIVE rows only (migration 0010), so
// nothing at the database refuses a second pending row -- and
// ConfirmTOTP's FindPendingByUserAndType would then take whichever row the
// database happened to return first, which need not be the enrollment the
// user actually scanned. The database is the arbiter instead:
// idx_user_mfa_factors_user_type_pending (migration 0011), a partial
// unique index over the pending rows, refuses a second pending row with
// gorm.ErrDuplicatedKey, and by the time the violation surfaces the
// winner's row has committed, so this method's bounded retry simply runs
// its delete-then-create again -- the delete removes the winner's row, and
// the retry's insert is the sole survivor. Every racing enrollment
// therefore succeeds and the LAST one to commit owns the pending row,
// exactly the enrollment the user was most recently shown; a confirm
// always matches what the user scanned.
//
// It is not an error for no pending row to exist -- replacing an abandoned
// attempt or turning MFA on for the first time are the same call -- and it
// deliberately never touches an ACTIVE factor: see EnrollTOTP's own doc
// comment for why an active factor survives an enroll call all the way to a
// genuinely successful Confirm, never merely a started one.
func (r *MFAFactorRepository) ReplacePending(ctx context.Context, userID, factorType string, f *UserMFAFactor) error {
	if f.ID == "" {
		f.ID = newID()
	}
	if f.Status == "" {
		f.Status = MFAFactorStatusPending
	}
	var lastErr error
	for attempt := 0; attempt < replacePendingAttempts; attempt++ {
		lastErr = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Where("user_id = ? AND type = ? AND status = ?", userID, factorType, MFAFactorStatusPending).
				Delete(&UserMFAFactor{}).Error; err != nil {
				return err
			}
			return tx.Create(f).Error
		})
		if lastErr == nil {
			return nil
		}
		if !errors.Is(lastErr, gorm.ErrDuplicatedKey) {
			return lastErr
		}
		// Lost the pending-row race: a concurrent enrollment committed its
		// row between this attempt's delete and insert (see the method's
		// doc comment). The transaction rolled back, nothing was deleted,
		// and the next attempt's delete removes the winner's committed row.
	}
	return fmt.Errorf("authn: replace pending %s factor for user %s: %w", factorType, userID, lastErr)
}

// replacePendingAttempts bounds ReplacePending's retry loop. One retry
// always suffices in theory -- the refusing row is committed by the time
// the violation surfaces, so the retry's delete removes it and its insert
// wins -- the bound exists to turn an unforeseen livelock into a loud error
// rather than an infinite loop.
const replacePendingAttempts = 3

// Confirm atomically promotes the pending factor id to active, recording
// step as its LastUsedStep so the confirmation code itself cannot be
// replayed at the next step-up verification, AND -- in the same
// transaction -- removes userID's other (still-active) factor of the same
// type, if any. It reports whether THIS call won the elevation: an UPDATE
// that matched no row is not an error, and one means the factor is no
// longer pending -- a concurrent ConfirmTOTP activated it between this
// caller's read and this write. The loser's whole transaction is rolled
// back (the demotion-shaped delete included), and the caller must hear
// "false" rather than proceed as if it had won: proceeding would
// regenerate the recovery-code batch the winner just displayed and
// announce a second enrollment over one code.
//
// The demotion-shaped delete runs FIRST, deliberately, mirroring
// go/pki/repository.go's PromoteToActive: idx_user_mfa_factors_user_type
// (migration 0010) is a partial unique index scoped to status='active',
// checked at each statement rather than deferred to commit, so activating
// id before the old active row is gone would momentarily leave two active
// rows for the same (user_id, type) inside this very transaction and be
// refused by the database it is trying to write to. Deleting first briefly
// leaves the type with NO active row instead, which the index has nothing
// to say about.
//
// The second parameter exists because of the shape of enrollment: no
// active factor is deleted at enroll time (see EnrollTOTP), so the old
// factor stays active until THIS call succeeds -- an enrollment that is
// cancelled or abandoned before confirming leaves the account exactly as
// it was, with its working second factor and recovery codes intact.
func (r *MFAFactorRepository) Confirm(ctx context.Context, userID, factorType, id string, at time.Time, step int64) (bool, error) {
	won := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND type = ? AND status = ? AND id != ?",
			userID, factorType, MFAFactorStatusActive, id).
			Delete(&UserMFAFactor{}).Error; err != nil {
			return err
		}
		res := tx.Where("id = ? AND status = ?", id, MFAFactorStatusPending).
			Updates(&UserMFAFactor{Status: MFAFactorStatusActive, ConfirmedAt: &at, LastUsedStep: step})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			// Lost the elevation to a concurrent confirm: roll the whole
			// transaction back and report the loss (see the doc comment).
			return errMFAConfirmLost
		}
		won = true
		return nil
	})
	if err != nil {
		if errors.Is(err, errMFAConfirmLost) {
			return false, nil
		}
		return false, err
	}
	return won, nil
}

// errMFAConfirmLost is the internal rollback marker Confirm uses to report
// that the elevation UPDATE matched no row. It never escapes Confirm.
var errMFAConfirmLost = errors.New("authn: mfa factor is no longer pending")

// UpdateLastUsedStep atomically advances f's replay guard from prevStep to
// newStep and reports whether it won the race, mirroring
// RefreshTokenRepository.Consume's compare-and-swap shape: two concurrent
// verifications of the same code must not both succeed.
func (r *MFAFactorRepository) UpdateLastUsedStep(ctx context.Context, id string, prevStep, newStep int64) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND last_used_step = ?", id, prevStep).
		Updates(&UserMFAFactor{LastUsedStep: newStep})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// UserRecoveryCode is one single-use MFA recovery code. IDENTITY-domain
// data, like UserMFAFactor.
type UserRecoveryCode struct {
	// ID is an application-generated UUID.
	ID string `gorm:"primaryKey;size:36"`

	// UserID is the owning user.
	UserID string `gorm:"column:user_id;size:36;not null;index:idx_user_recovery_codes_user_id"`

	// CodeHash is the SHA-256 digest of the code's normalized form. See
	// hashRecoveryCode.
	CodeHash string `gorm:"column:code_hash;size:64;not null"`

	// UsedAt is nil for an unused code.
	UsedAt *time.Time `gorm:"column:used_at"`

	CreatedAt time.Time `gorm:"autoCreateTime;not null"`
}

// TableName pins the table name.
func (UserRecoveryCode) TableName() string { return "user_recovery_codes" }

// RecoveryCodeRepository reads and writes the user_recovery_codes table.
type RecoveryCodeRepository struct {
	db *gorm.DB
}

// NewRecoveryCodeRepository binds db.
func NewRecoveryCodeRepository(db *gorm.DB) (*RecoveryCodeRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewRecoveryCodeRepository requires a database handle")
	}
	return &RecoveryCodeRepository{db: db}, nil
}

// ReplaceAll atomically replaces userID's entire recovery-code batch with
// codes: every existing row -- used or not -- is deleted and the new batch
// inserted in ONE transaction. Regenerating replaces the whole set, so an
// old, unused code from a batch a user has since regenerated must stop
// working, or "regenerate" would just mean "add ten more".
//
// The single transaction is the point, not an incidental: ConfirmTOTP swaps
// the factor and then regenerates the batch, and a regeneration that ran
// its delete and its insert as two separate statements could fail between
// them (a refused insert, the process dying) and leave the account on an
// ACTIVE factor with ZERO backup codes -- no way back in when the
// authenticator is lost. Atomic replacement has no such middle state: the
// old batch survives intact when the transaction rolls back, and only a
// complete new batch ever becomes visible.
func (r *RecoveryCodeRepository) ReplaceAll(ctx context.Context, userID string, codes []*UserRecoveryCode) error {
	for _, c := range codes {
		if c.ID == "" {
			c.ID = newID()
		}
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Delete(&UserRecoveryCode{}).Error; err != nil {
			return err
		}
		return tx.Create(&codes).Error
	})
}

// FindUnusedByUserAndHash returns userID's unused recovery code matching
// hash, or ErrNotFound. Step-up verification deliberately does NOT use
// this lookup -- see FindByUserAndHash for why -- but the method stays
// for callers whose own logic only ever handles an unused code (a code
// that is not unused and not present both collapse to ErrNotFound here).
func (r *RecoveryCodeRepository) FindUnusedByUserAndHash(ctx context.Context, userID, hash string) (*UserRecoveryCode, error) {
	var c UserRecoveryCode
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND code_hash = ? AND used_at IS NULL", userID, hash).
		First(&c).Error
	if err != nil {
		return nil, translate(err)
	}
	return &c, nil
}

// FindByUserAndHash returns userID's recovery code matching hash, used or
// not, or ErrNotFound. Step-up verification uses THIS lookup (not the
// unused-only FindUnusedByUserAndHash) so a code that was issued but has
// already been consumed can be told apart from a code that was never
// issued: the former answers ErrMFACodeUsed, the latter
// ErrMFAInvalidCode -- the honest split whose disclosure bound
// ErrMFAInvalidCode's own doc comment records (only a caller already
// holding an issued code can observe it).
func (r *RecoveryCodeRepository) FindByUserAndHash(ctx context.Context, userID, hash string) (*UserRecoveryCode, error) {
	var c UserRecoveryCode
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND code_hash = ?", userID, hash).
		First(&c).Error
	if err != nil {
		return nil, translate(err)
	}
	return &c, nil
}

// MarkUsed atomically consumes an unused recovery code and reports whether
// it won the race -- the same "used_at IS NULL" compare-and-swap shape as
// RefreshTokenRepository.Consume, so two concurrent uses of the same code
// cannot both succeed.
func (r *RecoveryCodeRepository) MarkUsed(ctx context.Context, id string, at time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND used_at IS NULL", id).
		Updates(&UserRecoveryCode{UsedAt: &at})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// EnrollTOTPResult is what EnrollTOTP returns.
type EnrollTOTPResult struct {
	// Secret is the base32-encoded shared secret, for manual entry. It is
	// never retrievable again after this call.
	Secret string
	// ProvisioningURI is the otpauth://totp/... URI an authenticator app
	// scans, conventionally rendered as a QR code by the FRONTEND (this
	// package renders no image -- see internal/totp's own doc comment).
	ProvisioningURI string
}

// EnrollTOTP starts TOTP enrollment for the user principal identifies,
// creating a fresh PENDING factor without disturbing an existing ACTIVE one
// of the same type -- see ConfirmTOTP for the moment a pending factor
// actually takes over.
//
// This is a two-phase replacement, deliberately: the existing ACTIVE factor
// is never deleted at enroll time, so an enrollment that is cancelled or
// abandoned before confirming cannot leave the account with no working
// second factor and no working recovery codes at all -- a silent
// security-posture downgrade nothing about the cancel path would warn the
// user of. The old factor stays fully functional for VerifyStepUp and
// RegenerateRecoveryCodes all the way through a successful enroll; only a
// genuinely successful ConfirmTOTP retires it (see that method and
// MFAFactorRepository.Confirm for the atomic swap).
//
// Replacing an ALREADY ACTIVE factor still requires principal.AMR to carry
// a completed second-factor step-up: changing MFA settings needs re-proof,
// not merely an existing session -- without this, a bare access token could
// silently seize an established factor by starting a replacement enrollment
// an attacker-known secret would later confirm. The check is enforced HERE
// rather than by wrapping the route in RequireStepUp because whether
// step-up is even required depends on whether an ACTIVE factor already
// exists to protect, information only this method has: a brand-new
// enrollment (turning MFA on for the first time) has nothing to step up
// from, so it proceeds without one regardless of AMR.
//
// The returned secret must be confirmed with ConfirmTOTP before it can
// verify anything: a pending factor cannot satisfy VerifyStepUp.
func (s *Service) EnrollTOTP(ctx context.Context, principal Principal) (*EnrollTOTPResult, error) {
	userID := principal.UserID
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	switch _, findErr := s.mfaFactors.FindActiveByUserAndType(ctx, userID, MFATypeTOTP); {
	case findErr == nil:
		if !hasSecondFactor(principal.AMR) {
			return nil, ErrStepUpRequired
		}
	case errors.Is(findErr, ErrNotFound):
		// Nothing active yet: first-time setup has no existing factor to
		// step up from.
	default:
		return nil, findErr
	}

	// Only a PENDING row from an earlier, abandoned attempt is replaced
	// here -- an ACTIVE factor (if any) is deliberately left in place --
	// and the replacement is one atomic, per-user-serialized operation
	// (MFAFactorRepository.ReplacePending's doc comment: two rapid enrolls
	// must never leave two pending rows for a confirm to take the wrong
	// one). See this method's own doc comment.
	secret, err := totp.GenerateSecret()
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	factor := &UserMFAFactor{
		UserID:    userID,
		Type:      MFATypeTOTP,
		Secret:    secret,
		CreatedAt: s.now(),
	}
	if err := s.mfaFactors.ReplacePending(ctx, userID, MFATypeTOTP, factor); err != nil {
		return nil, err
	}

	return &EnrollTOTPResult{
		Secret:          secret,
		ProvisioningURI: totp.ProvisioningURI(s.issuer, mfaAccountLabel(user), secret),
	}, nil
}

// ConfirmTOTP validates code against userID's pending TOTP factor,
// activates it -- atomically retiring the factor it replaces, if any (see
// MFAFactorRepository.Confirm) -- and returns a fresh batch of
// recoveryCodeCount recovery codes in PLAINTEXT -- the only moment they are
// ever available in that form. The caller must show them to the user
// immediately; they are never retrievable again.
//
// This is the ONE moment a replacement enrollment actually takes effect:
// EnrollTOTP deliberately leaves an existing active factor and its recovery
// codes untouched, precisely so that reaching this call is what the
// replacement's replacingNotice copy warns the user about, not the wizard
// merely opening.
func (s *Service) ConfirmTOTP(ctx context.Context, userID, code string) ([]string, error) {
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	factor, err := s.mfaFactors.FindPendingByUserAndType(ctx, userID, MFATypeTOTP)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		// No pending factor: mfaFactorStateError tells "never enrolled"
		// apart from "already active, nothing pending" the same way
		// EnrollTOTP does, so confirming a second time still answers
		// ErrMFAAlreadyEnrolled rather than the misleading ErrMFANotEnrolled.
		return nil, s.mfaFactorStateError(ctx, userID)
	}

	ok, step := totp.Validate(factor.Secret, code, totpSkewSteps)
	if !ok {
		return nil, ErrMFAInvalidCode
	}
	won, confirmErr := s.mfaFactors.Confirm(ctx, userID, MFATypeTOTP, factor.ID, s.now(), step)
	if confirmErr != nil {
		return nil, confirmErr
	}
	if !won {
		// A concurrent ConfirmTOTP validated the same pending factor with
		// the same code and activated it between the read above and this
		// write (or an EnrollTOTP replaced the pending row). This call
		// lost the elevation: it must not regenerate the recovery-code
		// batch the winner just displayed -- the loser would replace the
		// codes the winner's screen is showing -- nor announce a second
		// enrollment, nor answer success. Classify exactly like a confirm
		// that found no pending factor.
		return nil, s.mfaFactorStateError(ctx, userID)
	}

	codes, err := s.regenerateRecoveryCodesLocked(ctx, userID)
	if err != nil {
		return nil, err
	}

	s.publish(ctx, pkgcore.Event{
		Type:    EventMFAEnrolled,
		Payload: MFAEnrolledPayload{UserID: userID, Type: MFATypeTOTP},
	})
	return codes, nil
}

// mfaFactorStateError answers a ConfirmTOTP that found no pending factor to
// elevate, distinguishing the two shapes that look alike from the caller's
// side: an ACTIVE factor already exists (the second confirm of a completed
// enrollment, or the loser of a concurrent confirm race -- ErrMFAAlreadyEnrolled)
// versus none ever enrolled (ErrMFANotEnrolled).
func (s *Service) mfaFactorStateError(ctx context.Context, userID string) error {
	_, err := s.mfaFactors.FindActiveByUserAndType(ctx, userID, MFATypeTOTP)
	switch {
	case err == nil:
		return ErrMFAAlreadyEnrolled
	case errors.Is(err, ErrNotFound):
		return ErrMFANotEnrolled
	default:
		return err
	}
}

// RegenerateRecoveryCodes discards userID's existing recovery codes and
// issues a fresh batch, requiring an ACTIVE TOTP factor to exist: a set of
// backup codes for a second factor that was never actually confirmed would
// let someone bypass a step-up that the account owner never really enabled.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	if _, err := s.mfaFactors.FindActiveByUserAndType(ctx, userID, MFATypeTOTP); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrMFANotEnrolled
		}
		return nil, err
	}

	codes, err := s.regenerateRecoveryCodesLocked(ctx, userID)
	if err != nil {
		return nil, err
	}

	s.publish(ctx, pkgcore.Event{
		Type:    EventMFARecoveryCodesRegenerated,
		Payload: MFAEnrolledPayload{UserID: userID, Type: MFATypeTOTP},
	})
	return codes, nil
}

// regenerateRecoveryCodesLocked replaces userID's recovery-code batch and
// returns the new one in plaintext. "Locked" in the name refers to the
// invariant it maintains -- the replacement is atomic, so there is never a
// state where old AND new codes both work, nor one where the account has an
// active factor and NO backup codes (RecoveryCodeRepository.ReplaceAll's
// doc comment) -- not to a database lock held across the call: the atomic
// replacement transaction is that guarantee, and this operation needs no
// row lock on top of it, since it is a single user regenerating their own
// codes, not two parties racing over one row.
func (s *Service) regenerateRecoveryCodesLocked(ctx context.Context, userID string) ([]string, error) {
	plain := make([]string, recoveryCodeCount)
	rows := make([]*UserRecoveryCode, recoveryCodeCount)
	now := s.now()
	for i := range plain {
		code, err := generateRecoveryCode()
		if err != nil {
			return nil, ErrInternal.WithCause(err)
		}
		plain[i] = code
		rows[i] = &UserRecoveryCode{UserID: userID, CodeHash: hashRecoveryCode(code), CreatedAt: now}
	}
	if err := s.recoveryCodes.ReplaceAll(ctx, userID, rows); err != nil {
		return nil, err
	}
	return plain, nil
}

// VerifyStepUp re-authenticates the CURRENTLY signed-in principal with a
// second factor -- a TOTP code or a recovery code -- and returns a freshly
// minted access token whose AMR carries the factor that was used, without
// otherwise changing the session: the refresh token is untouched, exactly
// like SwitchTenant.
//
// The elevation is deliberately NOT persisted to the session row. It lives
// only in the access token this call mints, so it expires with that
// token's own TTL and a subsequent NATURAL refresh reverts to the
// session's original AMR -- the property that makes step-up a periodic
// re-proof rather than a permanent unlock for the rest of the session, with
// no separate expiry mechanism needed: Refresh already mints from
// session.AMRList(), never from a token's own claims.
func (s *Service) VerifyStepUp(ctx context.Context, principal Principal, code, ip string) (*TokenPair, error) {
	start := time.Now()
	pair, err := s.verifyStepUp(ctx, principal, code, ip)
	s.recordAuthMetric(ctx, authOpMFAChallenge, start, err)
	return pair, err
}

// verifyStepUp is VerifyStepUp's actual implementation, split out for the
// identical shadow-avoidance reason service.go's Login doc comment
// explains.
func (s *Service) verifyStepUp(ctx context.Context, principal Principal, code, ip string) (*TokenPair, error) {
	if principal.UserID == "" || principal.SessionID == "" {
		return nil, ErrAuthenticationRequired
	}

	if err := s.guard.CheckStepUp(ctx, principal.UserID, ip); err != nil {
		return nil, err
	}

	session, err := s.sessionRepo.FindByID(ctx, principal.SessionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrSessionRevoked
		}
		return nil, err
	}
	if session.UserID != principal.UserID {
		return nil, ErrTokenInvalid
	}
	// The same idiom Rotate and SwitchTenant carry (session.go,
	// service.go): a session that is Active in name but past its own
	// ExpiresAt is not usable either, and nothing here ever flips Status
	// away from active when a session merely times out.
	if session.Status != SessionStatusActive || !s.now().Before(session.ExpiresAt) {
		return nil, ErrSessionRevoked
	}

	method, err := s.verifySecondFactor(ctx, principal.UserID, code)
	if err != nil {
		return nil, err
	}

	user, err := s.users.FindByID(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	if user.Status != UserStatusActive {
		return nil, ErrInvalidCredentials
	}

	amr := appendAMR(session.AMRList(), method)
	tenantID := pkgcore.TenantID(session.CurrentTenantID)
	return s.mintPairWithAMR(ctx, user, session, tenantID, IssuedRefreshToken{}, amr)
}

// verifySecondFactor tries code as a TOTP code when it looks like one (six
// ASCII digits, the fixed shape this module's TOTP convention always
// produces), and as a recovery code otherwise -- the same
// look-at-the-shape dispatch Service.findByIdentifier uses for email versus
// phone.
func (s *Service) verifySecondFactor(ctx context.Context, userID, code string) (string, error) {
	if isTOTPShaped(code) {
		factor, err := s.mfaFactors.FindActiveByUserAndType(ctx, userID, MFATypeTOTP)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", ErrMFANotEnrolled
			}
			return "", err
		}
		if err := s.verifyTOTPFactor(ctx, factor, code); err != nil {
			return "", err
		}
		return MethodMFATOTP, nil
	}

	if err := s.verifyRecoveryCode(ctx, userID, code); err != nil {
		return "", err
	}
	return MethodMFARecoveryCode, nil
}

// verifyTOTPFactor validates code against factor and advances its replay
// guard, refusing a code whose matched step is not strictly newer than the
// factor's LastUsedStep -- the check that makes a code single-use. The
// refusal is the same in both failure shapes; only the classification
// differs: a code that NEVER validates answers ErrMFAInvalidCode, while a
// code that validates but is refused by the guard -- it was verified
// before, or the guard advanced past its step -- answers ErrMFACodeUsed,
// so a holder of a spent code is told it is spent instead of "invalid, try
// again" (ErrMFAInvalidCode's doc comment carries the disclosure
// analysis).
func (s *Service) verifyTOTPFactor(ctx context.Context, factor *UserMFAFactor, code string) error {
	ok, step := totp.Validate(factor.Secret, code, totpSkewSteps)
	if !ok {
		return ErrMFAInvalidCode
	}
	if step <= factor.LastUsedStep {
		return ErrMFACodeUsed
	}
	won, err := s.mfaFactors.UpdateLastUsedStep(ctx, factor.ID, factor.LastUsedStep, step)
	if err != nil {
		return err
	}
	if !won {
		// Lost the compare-and-swap to a concurrent verification of the
		// same step: that verification consumed the code. Spent, not
		// invalid -- same refusal, honest classification.
		return ErrMFACodeUsed
	}
	return nil
}

// verifyRecoveryCode validates and single-use-consumes one of userID's
// recovery codes. A code no issued row matches -- never issued to this
// user, or invalidated by a batch regeneration -- answers
// ErrMFAInvalidCode; a code whose row is already marked used, or whose
// compare-and-swap a concurrent use won, answers ErrMFACodeUsed: the
// code was real and is gone, which is the truth a step-up surface needs
// to tell a user who just burned their last one.
func (s *Service) verifyRecoveryCode(ctx context.Context, userID, code string) error {
	hash := hashRecoveryCode(code)
	row, err := s.recoveryCodes.FindByUserAndHash(ctx, userID, hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrMFAInvalidCode
		}
		return err
	}
	if row.UsedAt != nil {
		return ErrMFACodeUsed
	}
	won, err := s.recoveryCodes.MarkUsed(ctx, row.ID, s.now())
	if err != nil {
		return err
	}
	if !won {
		return ErrMFACodeUsed
	}
	return nil
}

// isTOTPShaped reports whether code has this module's fixed TOTP shape:
// exactly totp.Digits ASCII digits.
func isTOTPShaped(code string) bool {
	if len(code) != totp.Digits {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// mfaAccountLabel is the "account name" half of a TOTP provisioning URI's
// label. Email is preferred, since it is what a person recognizes in their
// authenticator app's account list; a phone-only account falls back to its
// user id, which is at least stable and unique.
func mfaAccountLabel(user *User) string {
	if user.Email != "" {
		return user.Email
	}
	return user.ID
}

// appendAMR returns base with method appended, unless base already
// contains it -- so a step-up verification never duplicates an entry when
// the composed amr already carries the factor. Each VerifyStepUp call
// mints from the session's ORIGINAL amr, so the dedupe is defensive rather
// than hot; a caller composing amr some other way should not have to
// rediscover it. It always returns a new slice; it never mutates base
// itself, which may be session.AMRList()'s live backing data.
func appendAMR(base []string, method string) []string {
	if slices.Contains(base, method) {
		out := make([]string, len(base))
		copy(out, base)
		return out
	}
	out := make([]string, 0, len(base)+1)
	out = append(out, base...)
	return append(out, method)
}

// hasSecondFactor reports whether amr already carries proof of a second
// factor, TOTP or recovery code.
func hasSecondFactor(amr []string) bool {
	return slices.Contains(amr, MethodMFATOTP) || slices.Contains(amr, MethodMFARecoveryCode)
}

// RequireStepUp refuses a request whose Principal has not recently
// completed a second-factor step-up (RequireAuthenticated's stricter
// sibling), for a host's sensitive actions: changing a password, changing
// MFA itself, deleting an organization, exporting data.
//
// Known limitation: an account with NO MFA factor enrolled has nothing to
// step up WITH, so this middleware blocks the sensitive action
// unconditionally rather than falling back to, say, a password re-entry;
// no password-re-entry fallback exists for that case.
func RequireStepUp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		if !ok {
			writeAppError(w, ErrAuthenticationRequired)
			return
		}
		if !hasSecondFactor(principal.AMR) {
			writeAppError(w, ErrStepUpRequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// generateRecoveryCode draws a server-side, full-entropy recovery code,
// formatted as two four-character base32 groups separated by a dash for
// readability (e.g. "K3QM-7XHP").
func generateRecoveryCode() (string, error) {
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authn: draw recovery code: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return encoded[:4] + "-" + encoded[4:], nil
}

// normalizeRecoveryCode strips whitespace and dashes and upper-cases code,
// so a user retyping "k3qm7xhp" or "K3QM-7XHP" both match the same stored
// hash.
func normalizeRecoveryCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	return strings.ReplaceAll(code, "-", "")
}

// hashRecoveryCode returns the hex SHA-256 digest stored for a recovery
// code's normalized form.
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}
