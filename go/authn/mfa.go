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

// MFA factor types. Only 'totp' ships in this round.
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
	// MethodMFATOTP is a step-up (or, in a later round, a login)
	// satisfied by a TOTP code.
	MethodMFATOTP = "mfa:totp"
	// MethodMFARecoveryCode is a step-up satisfied by a recovery code.
	MethodMFARecoveryCode = "mfa:recovery_code"
)

// recoveryCodeCount is how many recovery codes ConfirmTOTP and
// RegenerateRecoveryCodes generate, per docs/internal/05.
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

	// UserID is the owning user. Unique together with Type: enrolling
	// again replaces rather than accumulates a second row of the same
	// type -- see Service.EnrollTOTP.
	UserID string `gorm:"column:user_id;size:36;not null;uniqueIndex:idx_user_mfa_factors_user_type,priority:1"`

	// Type is one of the MFAType* constants.
	Type string `gorm:"size:32;not null;uniqueIndex:idx_user_mfa_factors_user_type,priority:2"`

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

// FindByUserAndType returns userID's factor of the given type, or
// ErrNotFound.
func (r *MFAFactorRepository) FindByUserAndType(ctx context.Context, userID, factorType string) (*UserMFAFactor, error) {
	var f UserMFAFactor
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND type = ?", userID, factorType).
		First(&f).Error
	if err != nil {
		return nil, translate(err)
	}
	return &f, nil
}

// DeleteByUserAndType removes userID's factor of the given type, if any. It
// is not an error for none to exist: EnrollTOTP calls this unconditionally
// before creating a fresh row.
func (r *MFAFactorRepository) DeleteByUserAndType(ctx context.Context, userID, factorType string) error {
	return r.db.WithContext(ctx).
		Where("user_id = ? AND type = ?", userID, factorType).
		Delete(&UserMFAFactor{}).Error
}

// Confirm moves a PENDING factor to active, recording step as its
// LastUsedStep so the confirmation code itself cannot be replayed at the
// next step-up verification.
func (r *MFAFactorRepository) Confirm(ctx context.Context, id string, at time.Time, step int64) error {
	return r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, MFAFactorStatusPending).
		Updates(&UserMFAFactor{Status: MFAFactorStatusActive, ConfirmedAt: &at, LastUsedStep: step}).Error
}

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

// CreateBatch inserts every row in codes, filling in IDs where empty.
func (r *RecoveryCodeRepository) CreateBatch(ctx context.Context, codes []*UserRecoveryCode) error {
	for _, c := range codes {
		if c.ID == "" {
			c.ID = newID()
		}
	}
	return r.db.WithContext(ctx).Create(&codes).Error
}

// DeleteAllByUser removes every recovery code belonging to userID,
// regardless of whether it was used. Regenerating replaces the whole set:
// an old, unused code from a batch a user has since regenerated must stop
// working, or "regenerate" would just mean "add ten more".
func (r *RecoveryCodeRepository) DeleteAllByUser(ctx context.Context, userID string) error {
	return r.db.WithContext(ctx).Where("user_id = ?", userID).Delete(&UserRecoveryCode{}).Error
}

// FindUnusedByUserAndHash returns userID's unused recovery code matching
// hash, or ErrNotFound.
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
// replacing any existing TOTP factor (pending or active) with a fresh
// pending one.
//
// Replacing an ALREADY ACTIVE factor requires principal.AMR to carry a
// completed second-factor step-up (docs/internal/05 line 127: changing MFA
// settings needs re-proof, not merely an existing session) -- without this,
// a bare access token could silently seize an established factor by
// deleting it and enrolling an attacker-known secret in its place. The
// check is enforced HERE rather than by wrapping the route in RequireStepUp
// because whether step-up is even required depends on whether an ACTIVE
// factor already exists to protect, information only this method has: a
// brand-new enrollment (turning MFA on for the first time, docs/internal/05
// line 125) has nothing to step up FROM, so it proceeds exactly as before
// regardless of AMR.
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

	switch existing, findErr := s.mfaFactors.FindByUserAndType(ctx, userID, MFATypeTOTP); {
	case findErr == nil:
		if existing.Status == MFAFactorStatusActive && !hasSecondFactor(principal.AMR) {
			return nil, ErrStepUpRequired
		}
	case errors.Is(findErr, ErrNotFound):
		// Nothing enrolled yet: first-time setup has no existing factor
		// to step up from.
	default:
		return nil, findErr
	}

	if delErr := s.mfaFactors.DeleteByUserAndType(ctx, userID, MFATypeTOTP); delErr != nil {
		return nil, delErr
	}

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
	if err := s.mfaFactors.Create(ctx, factor); err != nil {
		return nil, err
	}

	return &EnrollTOTPResult{
		Secret:          secret,
		ProvisioningURI: totp.ProvisioningURI(s.issuer, mfaAccountLabel(user), secret),
	}, nil
}

// ConfirmTOTP validates code against userID's pending TOTP factor,
// activates it, and returns a fresh batch of recoveryCodeCount recovery
// codes in PLAINTEXT -- the only moment they are ever available in that
// form. The caller must show them to the user immediately; they are never
// retrievable again.
func (s *Service) ConfirmTOTP(ctx context.Context, userID, code string) ([]string, error) {
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	factor, err := s.mfaFactors.FindByUserAndType(ctx, userID, MFATypeTOTP)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrMFANotEnrolled
		}
		return nil, err
	}
	if factor.Status != MFAFactorStatusPending {
		return nil, ErrMFAAlreadyEnrolled
	}

	ok, step := totp.Validate(factor.Secret, code, totpSkewSteps)
	if !ok {
		return nil, ErrMFAInvalidCode
	}
	if confirmErr := s.mfaFactors.Confirm(ctx, factor.ID, s.now(), step); confirmErr != nil {
		return nil, confirmErr
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

// RegenerateRecoveryCodes discards userID's existing recovery codes and
// issues a fresh batch, requiring an ACTIVE TOTP factor to exist: a set of
// backup codes for a second factor that was never actually confirmed would
// let someone bypass a step-up that the account owner never really enabled.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return nil, ErrAuthenticationRequired
	}
	factor, err := s.mfaFactors.FindByUserAndType(ctx, userID, MFATypeTOTP)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrMFANotEnrolled
		}
		return nil, err
	}
	if factor.Status != MFAFactorStatusActive {
		return nil, ErrMFANotEnrolled
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
// invariant it maintains (delete-then-create leaves no window where old AND
// new codes both work), not to a database lock: SQLite and PostgreSQL have
// no common row-locking primitive this module reaches for (see
// RefreshTokenRepository.Consume's own doc comment on why compare-and-swap,
// not SELECT ... FOR UPDATE, is this codebase's answer to that gap), and
// this operation does not need one -- it is a single user regenerating
// their own codes, not two parties racing over one row.
func (s *Service) regenerateRecoveryCodesLocked(ctx context.Context, userID string) ([]string, error) {
	if err := s.recoveryCodes.DeleteAllByUser(ctx, userID); err != nil {
		return nil, err
	}

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
	if err := s.recoveryCodes.CreateBatch(ctx, rows); err != nil {
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
	if session.Status != SessionStatusActive {
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
		factor, err := s.mfaFactors.FindByUserAndType(ctx, userID, MFATypeTOTP)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", ErrMFANotEnrolled
			}
			return "", err
		}
		if factor.Status != MFAFactorStatusActive {
			return "", ErrMFANotEnrolled
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
// factor's LastUsedStep -- the check that makes a code single-use.
func (s *Service) verifyTOTPFactor(ctx context.Context, factor *UserMFAFactor, code string) error {
	ok, step := totp.Validate(factor.Secret, code, totpSkewSteps)
	if !ok {
		return ErrMFAInvalidCode
	}
	if step <= factor.LastUsedStep {
		return ErrMFAInvalidCode
	}
	won, err := s.mfaFactors.UpdateLastUsedStep(ctx, factor.ID, factor.LastUsedStep, step)
	if err != nil {
		return err
	}
	if !won {
		return ErrMFAInvalidCode
	}
	return nil
}

// verifyRecoveryCode validates and single-use-consumes one of userID's
// recovery codes.
func (s *Service) verifyRecoveryCode(ctx context.Context, userID, code string) error {
	hash := hashRecoveryCode(code)
	row, err := s.recoveryCodes.FindUnusedByUserAndHash(ctx, userID, hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrMFAInvalidCode
		}
		return err
	}
	won, err := s.recoveryCodes.MarkUsed(ctx, row.ID, s.now())
	if err != nil {
		return err
	}
	if !won {
		return ErrMFAInvalidCode
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
// contains it -- so re-verifying step-up twice in the same access token's
// life (it cannot happen today, since each VerifyStepUp call mints a fresh
// token from the session's ORIGINAL amr, but a future caller composing amr
// differently should not have to rediscover this) never duplicates an
// entry. It always returns a new slice; it never mutates base itself, which
// may be session.AMRList()'s live backing data.
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
// sibling), for the sensitive actions docs/internal/05 names: changing a
// password, changing MFA itself, deleting an organization, exporting data.
//
// Known limitation, stated rather than hidden: an account with NO MFA
// factor enrolled has nothing to step up WITH, so this middleware blocks
// the sensitive action unconditionally rather than falling back to, say, a
// password re-entry. A password-re-entry fallback for that case is
// deferred -- see this module's AGENTS.md.
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
