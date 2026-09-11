package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/BurntSushi/toml"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"

	"github.com/vislake/speed/go/authn/locales"

	obs "github.com/vislake/speed/go/observability"
)

// Method values a login can be established with beyond MethodPassword,
// MethodSocial and MethodOIDC in model.go.
const (
	// MethodSMS is a phone-number-plus-one-time-SMS-code sign-in.
	MethodSMS = "sms"
)

// FailureReasonBadCode records a wrong verification code, alongside
// model.go's FailureReasonBadPassword.
const FailureReasonBadCode = "bad_code"

// Verification-code purposes. Only one purpose is implemented (phone
// login); the column exists so a second purpose reuses this table and this
// row shape rather than duplicating it -- see
// migrations/sqlite/0007_create_verification_codes.sql.
const (
	// VerificationPurposePhoneLogin is a one-time code sent to sign in
	// with an existing account's phone number.
	VerificationPurposePhoneLogin = "phone_login"
)

// Verification-code status values.
const (
	VerificationCodeStatusActive   = "active"
	VerificationCodeStatusConsumed = "consumed"
	VerificationCodeStatusLocked   = "locked"
)

const (
	// smsCodeDigits is the length of a phone-login verification code.
	smsCodeDigits = 6

	// DefaultSMSCodeTTL is how long a phone-login verification code stays
	// valid after it is sent, absent WithSMSCodeTTL / dynamic
	// configuration.
	DefaultSMSCodeTTL = 5 * time.Minute

	// DefaultSMSCodeMaxAttempts is how many wrong codes a single issued
	// verification code tolerates before it locks.
	DefaultSMSCodeMaxAttempts = 5

	// maxMarkAttemptRetries bounds how many times verifyPhoneLoginCode
	// retries MarkAttempt's compare-and-swap after losing a race to a
	// concurrent wrong guess against the SAME code. It only needs to
	// cover genuine same-instant contention, never an unbounded loop --
	// see verifyPhoneLoginCode's own doc comment.
	maxMarkAttemptRetries = 5

	// DefaultLocale is the platform default language: the tier
	// backend-generated content renders in when the intended recipient has
	// not chosen one -- User.Locale is empty, or a login has not resolved
	// to a user at all yet -- and the last tier of every locale chain this
	// module runs. en-US is the same default @speed/i18n's own negotiation
	// order ends in, so the two halves of the stack agree on where a chain
	// that resolves nothing lands.
	DefaultLocale = i18n.LocaleENUS
)

// VerificationCode is one issued one-time code. IDENTITY-domain data: it is
// issued to a phone number, not to a tenant.
type VerificationCode struct {
	// ID is an application-generated UUID.
	ID string `gorm:"primaryKey;size:36"`

	// Purpose is one of the VerificationPurpose* constants.
	Purpose string `gorm:"size:32;not null;index:idx_verification_codes_target,priority:2"`

	// TargetIndex is the blind index of the phone number (or, for a
	// future purpose, an email address) the code was issued to -- the
	// SAME index UserRepository computes, via Service.identifierIndex's
	// sibling helpers, never a second copy of the plaintext.
	TargetIndex string `gorm:"column:target_index;size:64;not null;index:idx_verification_codes_target,priority:1"`

	// CodeHash is the SHA-256 digest of the code. See the migration's own
	// doc comment for why a plain digest, not argon2id, is the right
	// choice here.
	CodeHash string `gorm:"column:code_hash;size:64;not null"`

	// Attempts counts wrong guesses against this code.
	Attempts int `gorm:"not null"`

	// MaxAttempts is how many wrong guesses this code tolerates before it
	// locks.
	MaxAttempts int `gorm:"column:max_attempts;not null"`

	// Status is one of the VerificationCodeStatus* constants.
	Status string `gorm:"size:16;not null"`

	CreatedAt time.Time `gorm:"autoCreateTime;not null"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`

	// ConsumedAt is nil until the code is successfully used.
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
}

// TableName pins the table name.
func (VerificationCode) TableName() string { return "verification_codes" }

// VerificationCodeRepository reads and writes the verification_codes table.
// Like every other repository in this module it holds a plain *gorm.DB --
// see repository.go's file comment for why identity data takes this shape.
type VerificationCodeRepository struct {
	db *gorm.DB
}

// NewVerificationCodeRepository binds db.
func NewVerificationCodeRepository(db *gorm.DB) (*VerificationCodeRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewVerificationCodeRepository requires a database handle")
	}
	return &VerificationCodeRepository{db: db}, nil
}

// Create inserts c, filling in its ID and Status when empty.
func (r *VerificationCodeRepository) Create(ctx context.Context, c *VerificationCode) error {
	if c.ID == "" {
		c.ID = newID()
	}
	if c.Status == "" {
		c.Status = VerificationCodeStatusActive
	}
	return r.db.WithContext(ctx).Create(c).Error
}

// FindLatestActive returns the most recently issued still-active,
// unexpired code for (purpose, targetIndex), or ErrNotFound.
func (r *VerificationCodeRepository) FindLatestActive(ctx context.Context, purpose, targetIndex string, now time.Time) (*VerificationCode, error) {
	var c VerificationCode
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND target_index = ? AND status = ? AND expires_at > ?",
			purpose, targetIndex, VerificationCodeStatusActive, now).
		Order("created_at DESC, id DESC").
		First(&c).Error
	if err != nil {
		return nil, translate(err)
	}
	return &c, nil
}

// MarkAttempt records one more wrong guess against c, locking it when the
// result reaches its MaxAttempts, and reports whether it won the race.
//
// prevAttempts is part of the WHERE clause, the same compare-and-swap shape
// RefreshTokenRepository.Consume uses: two concurrent wrong guesses against
// the same code must not both merely increment from the same stale count,
// which would undercount and let more guesses through than MaxAttempts
// allows.
func (r *VerificationCodeRepository) MarkAttempt(ctx context.Context, id string, prevAttempts int, lock bool) (bool, error) {
	updates := &VerificationCode{Attempts: prevAttempts + 1}
	if lock {
		updates.Status = VerificationCodeStatusLocked
	}
	res := r.db.WithContext(ctx).
		Where("id = ? AND attempts = ? AND status = ?", id, prevAttempts, VerificationCodeStatusActive).
		Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// FindByID returns the record with the given id in WHATEVER state it is in
// -- active, locked or consumed -- or ErrNotFound. MarkAttempt's retry loop
// (Service.markPhoneLoginAttempt) re-reads by id after losing a
// compare-and-swap, and must be able to observe that the record it lost to
// has left the active state rather than having FindLatestActive silently
// substitute a different, newer record for the same target.
func (r *VerificationCodeRepository) FindByID(ctx context.Context, id string) (*VerificationCode, error) {
	var c VerificationCode
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&c).Error; err != nil {
		return nil, translate(err)
	}
	return &c, nil
}

// Consume atomically moves an ACTIVE code to consumed and reports whether it
// won the race, mirroring RefreshTokenRepository.Consume: single use is a
// database-arbitrated compare-and-swap, not a read followed by a write.
func (r *VerificationCodeRepository) Consume(ctx context.Context, id string, at time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, VerificationCodeStatusActive).
		Updates(&VerificationCode{Status: VerificationCodeStatusConsumed, ConsumedAt: &at})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// RequestSMSCodeInput describes a phone-login code request.
type RequestSMSCodeInput struct {
	// Phone is the account's phone number, in any formatting.
	Phone string
	// AcceptLanguage is the request's Accept-Language header: the
	// transport channel the frontend's language chain sends its resolved
	// language through, and the first tier of the SMS body's language
	// chain -- the requester is the recipient of a sign-in code, so the
	// request's own language outranks the account's stored locale (see
	// smsLocale).
	AcceptLanguage string
	// IP is the requesting client's address, for the send-side rate
	// limit.
	IP string
}

// SMSLoginInput describes a phone-plus-code sign-in attempt.
type SMSLoginInput struct {
	// Phone is the account's phone number, in any formatting.
	Phone string
	// Code is the digits the user typed.
	Code string
	// TenantID is the tenant to issue the first access token for. Empty
	// means "the first tenant this user is a member of". A value here is
	// a REQUEST, never a grant: membership is verified either way, same
	// as LoginInput.TenantID.
	TenantID pkgcore.TenantID
	// Device, UserAgent and IP describe the client, recorded on the
	// session and the login attempt.
	Device    string
	UserAgent string
	IP        string
}

// smsCodeRequestLatencyFloor is the minimum wall-clock duration
// RequestSMSCode's per-number work (deliverSMSCode below) takes to answer,
// regardless of which branch it took -- see RequestSMSCode's own doc
// comment for why. A package-level var, not a const, purely
// so a test can shrink it (and restore it via t.Cleanup) without paying
// this floor's real cost on every other test in the suite.
//
// The value is a rough, deliberately conservative estimate of a real
// SMSSender.Send call's typical wall-clock cost (a network round trip to
// an SMS gateway) plus the persisted VerificationCode row -- both of which
// only the KNOWN-number branch pays and neither of which this floor can
// know precisely for an arbitrary deployment's own gateway. It closes the
// SYSTEMATIC timing gap between the two branches (an unknown number's
// answer is microseconds against a known number's milliseconds), which is
// the difference an attacker rotating IPs would otherwise probe; it does
// not close every possible timing correlation: a real gateway's own tail
// latency, still visible above this floor, is a residual this constant
// does not and cannot erase.
var smsCodeRequestLatencyFloor = 150 * time.Millisecond

// RequestSMSCode issues and delivers a one-time sign-in code for an
// EXISTING account's phone number.
//
// It never discloses whether phone is registered, on two axes at once.
// The RESPONSE BODY never distinguishes them: a request for an unknown
// number returns nil exactly like a request for a known one, and neither a
// code nor an SMS is generated for the former -- the same enumeration
// defence Login's generic ErrInvalidCredentials answer is, applied to a
// request endpoint that has no password to be generic ABOUT. The WALL-
// CLOCK TIME to answer must not distinguish them either: a known number's
// real work (a persisted VerificationCode row plus one real SMSSender.Send
// call) costs measurably more than an unknown number's silent no-op, and an
// attacker rotating IPs to dodge limitSMSSendByIP could otherwise probe
// that timing difference directly -- silence is only the whole defence at
// the response-BODY layer. deliverSMSCode does
// the real per-number work; this method pads its total duration up to
// smsCodeRequestLatencyFloor afterward, regardless of which branch ran, so
// neither branch can finish faster than that floor. The rate-limit gate
// above is deliberately excluded from the padded span: it behaves
// identically for a known and an unknown number (both pay the same
// blind-index-keyed check before either branch is reached), so padding it
// too would only slow every caller for no timing-parity benefit.
func (s *Service) RequestSMSCode(ctx context.Context, in RequestSMSCodeInput) error {
	// The channel gate, for the same reasons login() checks its own first:
	// a deployment that turned the SMS channel off must not keep issuing
	// codes -- each one is a real SMS delivery and a verification-code row
	// -- just because the requester found the endpoint.
	if err := s.channelEnabled(ctx, FeatureFlagSMSLogin); err != nil {
		return err
	}

	index, err := s.users.PhoneIndexOf(in.Phone)
	if err != nil {
		return err
	}

	if guardErr := s.guard.CheckSMSSend(ctx, index, in.IP); guardErr != nil {
		return guardErr
	}

	start := time.Now()
	err = s.deliverSMSCode(ctx, in, index)
	if remaining := smsCodeRequestLatencyFloor - time.Since(start); remaining > 0 {
		time.Sleep(remaining)
	}
	return err
}

// deliverSMSCode implements RequestSMSCode's known/unknown-number branch as
// its own method so RequestSMSCode's body can pad its total duration -- see
// RequestSMSCode's own doc comment.
//
// Every failure that can fire only once a registered row is in hand is
// logged and folded into the answer the unknown-number side gives:
// the row's PII columns failing to decrypt while the lookup reads them
// back, persisting the code row, rendering the SMS body, handing it to the
// transport. Surfacing any of them instead would turn the response into a
// registration oracle for exactly as long as the failure lasts: an unknown
// number never reaches those steps, so a 5xx only registered numbers can
// produce discloses the one fact RequestSMSCode's doc comment promises is
// never disclosed. The decrypt failure is why the lookup's non-miss errors
// fold too: an unknown number has no row to read back, so the decode step,
// unlike the SELECT itself, is reachable only on the registered side.
//
// What still surfaces as an error is what the two number classes genuinely
// share: the gates RequestSMSCode runs before this method (same input
// either way, so identical failures), and the code draw, which every path
// below reaches exactly once -- the real draw, or the burn that both
// lookup failures route through -- so a broken entropy source fails both
// classes identically, whatever their lookups did.
func (s *Service) deliverSMSCode(ctx context.Context, in RequestSMSCodeInput, index string) error {
	user, err := s.users.FindByPhone(ctx, in.Phone)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			// The miss is the ordinary unknown-number answer and logs
			// nothing; anything else is logged and answered the same way.
			// Two conditions arrive here as one error: a genuine query
			// failure, which is symmetric on its own (the unknown side
			// runs the same SELECT and fails it the same way), and a
			// registered row whose PII columns no longer decrypt -- a
			// botched key rotation, a corrupted row -- which is
			// registered-only, and answering it with 5xx would disclose
			// an account's existence for as long as the damage lasts.
			obs.FromContext(ctx).Error("sms verification code user lookup failed", "error", err)
		}
		// Both error kinds exit through the same burn, which keeps the
		// "every post-gate path draws exactly one code" invariant intact
		// even when the lookup failed: an entropy failure answers 5xx
		// here exactly as it does for a registered number whose lookup
		// succeeded, so it cannot separate the two classes either.
		return s.burnSMSCodeRequestWork()
	}

	code, err := generateNumericCode(smsCodeDigits)
	if err != nil {
		// Every path through this method reaches the draw exactly once --
		// this real draw, or the burn the miss and the folded lookup
		// failures above route through -- so a broken entropy source
		// fails every path of both number classes alike and cannot tell
		// the two apart. It stays a real error, unlike the registered-only
		// failures below: a host condition this broad should reach the
		// operator as 5xx, and it answers with the same status on either
		// side.
		return ErrInternal.WithCause(err)
	}

	// The code's lifetime and attempt budget are resolved once per issued
	// code through the declared dynamic configuration
	// (authn.sms_code_ttl / authn.sms_code_max_attempts) when a settings
	// reader is wired, falling back to the construction-time values; the
	// row carries the resolved values, so a later change never rewrites the
	// rules of a code already in flight.
	smsTTL := s.smsCodeTTLFor(ctx)
	smsMaxAttempts := s.smsCodeMaxAttemptsFor(ctx)

	record := &VerificationCode{
		Purpose:     VerificationPurposePhoneLogin,
		TargetIndex: index,
		CodeHash:    hashVerificationCode(code),
		MaxAttempts: smsMaxAttempts,
		CreatedAt:   s.now(),
		ExpiresAt:   s.now().Add(smsTTL),
	}
	if createErr := s.verificationCodes.Create(ctx, record); createErr != nil {
		// Reachable only for a registered number -- the unknown-number
		// branch persists nothing -- so surfacing this failure would
		// answer 5xx for registered numbers while unregistered ones still
		// answered success: a registration oracle for as long as the
		// write path is broken. Logged and answered exactly like a
		// successful send, the same posture the transport branch below
		// documents. No further work is attempted either: without the
		// row the code could never verify (verifyPhoneLoginCode finds
		// codes by target through this table), so sending it would only
		// cost a delivery for a code that is dead on arrival.
		obs.FromContext(ctx).Error("sms verification code row could not be persisted", "error", createErr)
		return nil
	}

	minutes := int(smsTTL / time.Minute)
	text, usedLocale, err := renderSMSCode(smsLocale(in.AcceptLanguage, user.Locale), code, minutes)
	if err != nil {
		// The same registered-only shape as the persist branch above and
		// the transport branch below: only a registered number ever
		// renders a body, so answering this failure would distinguish the
		// two number classes while the locale bundles are broken. The row
		// persisted above is the harmless kind the transport branch
		// describes -- it expires under its own TTL with nothing able to
		// read it back.
		obs.FromContext(ctx).Error("sms verification code message could not be rendered", "error", err)
		return nil
	}
	// The message carries the render's identity -- the id it was rendered
	// from, the locale it was ACTUALLY rendered in (usedLocale, the
	// post-fallback value, never smsLocale's request-side answer), and the
	// values it interpolated -- so a template-typed carrier adapter can
	// select the account template the operator mapped for this
	// (locale, message-id) pair.
	if err := s.sms.Send(ctx, pkgcore.SMS{
		To:        in.Phone,
		Text:      text,
		MessageID: smsVerificationCodeMessageID,
		Locale:    usedLocale,
		Params: map[string]string{
			"code":    code,
			"minutes": strconv.Itoa(minutes),
		},
	}); err != nil {
		obs.FromContext(ctx).Error("sms verification code delivery failed", "error", err)
		// A delivery failure must not answer differently from a request
		// whose number has no account behind it. The registered branch's
		// real work (a persisted row plus one gateway call) is exactly what
		// an attacker probes to learn whether a number is registered, and
		// the SMS gateway is the one piece of that work whose outage is
		// invisible to the unknown-number branch -- surfacing it as an
		// error turns every gateway outage into a registration oracle on
		// the response status, undoing the promise RequestSMSCode's own
		// doc comment makes. The real error stays in this log line (and the
		// caller's own observability); the request answers identically to a
		// successful send, and a code that never arrived simply fails its
		// later verification with the same generic
		// ErrVerificationCodeInvalid every other dead code answers. The
		// persisted VerificationCode row is harmless: it expires under its
		// own TTL and nothing about it can be read back.
	}
	return nil
}

// burnSMSCodeRequestWork performs the same KIND of work the known-number
// branch of deliverSMSCode does (generate a code, hash it) for a request
// that cannot proceed to the real work -- a phone number with no account
// behind it, or one whose lookup failed and is answered like a miss --
// then discards the result. Doing so is cheap either way and does not
// meaningfully close the timing gap on its own (RequestSMSCode's own floor
// does that): it exists so every path the request can take pays the same
// CPU cost shape, the same "burn the same kind of work" spirit as
// Service.burnPasswordWork's identical role for Login -- and so every
// post-gate path draws exactly one code, which is what keeps a broken
// entropy source from separating the two number classes (see
// deliverSMSCode's own doc comment).
//
// It deliberately does NOT persist a VerificationCode row and does NOT call
// s.sms.Send: neither the miss nor the folded failure persists anything, so
// a code that was never stored could never verify -- a delivery would only
// send a code dead on arrival, and persisting or sending here would risk
// reaching an address that happens to be a real subscriber elsewhere and
// leaving a stray row with no owner, exactly what this method's own
// caller's doc comment rules out.
func (s *Service) burnSMSCodeRequestWork() error {
	code, err := generateNumericCode(smsCodeDigits)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	_ = hashVerificationCode(code)
	return nil
}

// LoginWithSMSCode verifies a phone-login code and, on success, starts a
// session exactly like Login does for a password.
//
// A successful verification is itself proof of phone ownership: on success
// this marks User.PhoneVerified true if it was not already, which is what
// lets a phone-only account satisfy LoginMethodCount without ever visiting
// a separate "verify my phone" flow.
//
// The per-target wrong-guess rate-limit budget (guard.CheckSMSVerifyWrongGuess)
// is deliberately consulted AFTER verifyPhoneLoginCode, not before: see that
// method's own doc comment for why an up-front check would let an attacker
// who knows the target but not the code hold the real holder's own correct
// attempt hostage for an entire window. The IP dimension
// (guard.CheckSMSVerifyIP) is unaffected and still checked up front, since
// it does not create that property.
func (s *Service) LoginWithSMSCode(ctx context.Context, in SMSLoginInput) (*TokenPair, error) {
	start := time.Now()
	pair, err := s.loginWithSMSCode(ctx, in)
	s.recordAuthMetric(ctx, authOpSMSCodeLogin, start, err)
	return pair, err
}

// loginWithSMSCode implements LoginWithSMSCode as its own unexported method,
// the same shadow-avoidance shape service.go's Login doc comment explains.
func (s *Service) loginWithSMSCode(ctx context.Context, in SMSLoginInput) (*TokenPair, error) {
	// The channel gate, mirroring login()'s own: with the flag off, a code
	// -- even a genuinely correct one -- must not start a session.
	if err := s.channelEnabled(ctx, FeatureFlagSMSLogin); err != nil {
		return nil, err
	}

	index, err := s.users.PhoneIndexOf(in.Phone)
	if err != nil {
		return nil, err
	}

	if guardErr := s.guard.CheckSMSVerifyIP(ctx, in.IP); guardErr != nil {
		return nil, guardErr
	}

	if verifyErr := s.verifyPhoneLoginCode(ctx, index, in.Code); verifyErr != nil {
		s.recordSMSFailureByPhone(ctx, in.Phone, index, in.IP)

		// Only a WRONG guess -- verifyErr is non-nil here -- ever consumes
		// this budget; a correct guess (the branch below) never reaches
		// this call at all, so it can never be refused by it.
		if guardErr := s.guard.CheckSMSVerifyWrongGuess(ctx, index); guardErr != nil {
			return nil, guardErr
		}
		return nil, verifyErr
	}

	user, err := s.users.FindByPhone(ctx, in.Phone)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The code matched a target index with no account behind
			// it any more (the account was deleted after the code
			// was issued). Reported exactly like a wrong code: there
			// is nothing for a caller to act on differently.
			return nil, ErrVerificationCodeInvalid
		}
		return nil, err
	}
	if user.Status != UserStatusActive {
		return nil, ErrInvalidCredentials
	}

	if !user.PhoneVerified {
		// A guarded single-column write, never a whole-row Save of the
		// freshly read user: saving the read's snapshot back in full would
		// silently undo whatever another caller committed on the row
		// between this read and the write (a password rehash from a
		// concurrent sign-in, say). MarkPhoneVerified sets only
		// phone_verified, and only while the row still carries the blind
		// index of the very phone this call verified -- a row whose phone
		// moved on since the read is left unverified rather than blessing
		// the new occupant.
		updated, err := s.users.MarkPhoneVerified(ctx, user.ID, index)
		switch {
		case err != nil:
			obs.FromContext(ctx).Warn("phone-verified flag could not be persisted", "user_id", user.ID, "error", err)
		case !updated:
			obs.FromContext(ctx).Debug("phone-verified flag left as-is: the row already reads verified or its phone changed since this login read it",
				"user_id", user.ID)
		}
	}

	return s.startExternalSession(ctx, user, []string{MethodSMS}, MethodSMS, in.TenantID, in.Device, in.UserAgent, in.IP)
}

// verifyPhoneLoginCode checks code against the latest active phone-login
// code for targetIndex, applying the attempt counter and single-use
// consumption.
//
// A miss, an expired code, a locked code and a wrong code all return the
// same ErrVerificationCodeInvalid: distinguishing them would tell a caller
// exactly how close they are to the attempt limit or that a code they
// cannot see even exists, neither of which helps anyone but an attacker.
//
// A wrong guess retries MarkAttempt's compare-and-swap, up to
// maxMarkAttemptRetries times, when it loses the race: unlike Consume just
// below (single-use -- losing means someone else already finished the job,
// so there is nothing left to retry), MarkAttempt's counter is not
// single-use, and a lost race here means a CONCURRENT wrong guess advanced
// it first, not that this guess's own attempt was recorded. Silently
// dropping the loser (as a single unconditional call would) lets a burst of
// truly concurrent wrong guesses under-count the shared counter and try
// more distinguishable guesses than MaxAttempts allows -- exactly what
// MarkAttempt's own doc comment says must not happen.
func (s *Service) verifyPhoneLoginCode(ctx context.Context, targetIndex, code string) error {
	record, err := s.verificationCodes.FindLatestActive(ctx, VerificationPurposePhoneLogin, targetIndex, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrVerificationCodeInvalid
		}
		return err
	}
	if record.Attempts >= record.MaxAttempts {
		return ErrVerificationCodeInvalid
	}

	if hashVerificationCode(code) != record.CodeHash {
		s.markPhoneLoginAttempt(ctx, record)
		return ErrVerificationCodeInvalid
	}

	won, err := s.verificationCodes.Consume(ctx, record.ID, s.now())
	if err != nil {
		return err
	}
	if !won {
		// Consumed (or locked) by a concurrent verification between the
		// read above and this write. Safe reading: refuse.
		return ErrVerificationCodeInvalid
	}
	return nil
}

// markPhoneLoginAttempt records one more wrong guess against record,
// retrying MarkAttempt's compare-and-swap when a concurrent wrong guess
// against the SAME code wins the race first -- see verifyPhoneLoginCode's
// own doc comment for why a lost race must be retried here rather than
// silently dropped. Any error, or exhausting maxMarkAttemptRetries, is
// logged and swallowed: verifyPhoneLoginCode's own caller already gets
// ErrVerificationCodeInvalid regardless, and this is best-effort accounting
// on top of that authoritative answer, not a second gate.
func (s *Service) markPhoneLoginAttempt(ctx context.Context, record *VerificationCode) {
	for range maxMarkAttemptRetries {
		locked := record.Attempts+1 >= record.MaxAttempts
		won, err := s.verificationCodes.MarkAttempt(ctx, record.ID, record.Attempts, locked)
		if err != nil {
			obs.FromContext(ctx).Error("verification code attempt could not be recorded", "error", err)
			return
		}
		if won {
			return
		}

		// Lost the race to a concurrent wrong guess against the same
		// code: re-read THAT record's current state and retry against the
		// fresh attempt count. The re-read is BY ID, never "the latest
		// active code for this target": the target's latest may be a
		// NEWER code the user re-requested while this retry was losing
		// (or a newer code a concurrent successful verification left as
		// the latest), and counting this guess against the new code would
		// burn the fresh code's attempt budget with a guess that was
		// never aimed at it.
		fresh, findErr := s.verificationCodes.FindByID(ctx, record.ID)
		if findErr != nil {
			return
		}
		if fresh.Status != VerificationCodeStatusActive {
			// Locked or consumed by the concurrent guess that won the
			// race: either way there is nothing left to retry.
			return
		}
		record = fresh
	}
	obs.FromContext(ctx).Warn("verification code attempt retries exhausted",
		"retries", maxMarkAttemptRetries)
}

// recordSMSFailureByPhone resolves phone's owning user (if any) and records
// a bad-code failure against index -- the lookup+call pair the SMS
// wrong-guess path needs at both of its call sites (see LoginWithSMSCode's
// own doc comment).
func (s *Service) recordSMSFailureByPhone(ctx context.Context, phone, index, ip string) {
	user, lookupErr := s.users.FindByPhone(ctx, phone)
	userID := ""
	if lookupErr == nil {
		userID = user.ID
	}
	s.recordSMSFailure(ctx, userID, index, ip, FailureReasonBadCode)
}

// recordSMSFailure writes a failed phone-login attempt to the login history
// and publishes EventLoginFailed, mirroring Service.recordFailure for the
// password channel.
func (s *Service) recordSMSFailure(ctx context.Context, userID, index, ip, reason string) {
	s.record(ctx, &LoginAttempt{
		UserID:          userID,
		IdentifierIndex: index,
		Method:          MethodSMS,
		Result:          LoginResultFailure,
		FailureReason:   reason,
		IP:              ip,
		CreatedAt:       s.now(),
	})
	// A failed sign-in is a pre-tenant fact -- no tenant is attested by a
	// sign-in that resolved none, whatever the caller's context holds (see
	// Service.publishTenantless's doc comment) -- so it never inherits one.
	s.publishTenantless(ctx, pkgcore.Event{
		Type: EventLoginFailed,
		Payload: LoginFailedPayload{
			UserID: userID,
			Method: MethodSMS,
			Reason: reason,
			IP:     ip,
		},
	})
}

// generateNumericCode draws a server-side, full-entropy decimal code of
// length digits, zero-padded.
func generateNumericCode(digits int) (string, error) {
	max := big.NewInt(1)
	ten := big.NewInt(10)
	for range digits {
		max.Mul(max, ten)
	}
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("authn: draw verification code: %w", err)
	}
	return fmt.Sprintf("%0*d", digits, n.Int64()), nil
}

// hashVerificationCode returns the hex SHA-256 digest stored for a
// verification code. See VerificationCode.CodeHash's doc comment for why a
// plain digest, not argon2id, is the correct choice here.
func hashVerificationCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// smsVerificationCodeMessageID is the locale message id the SMS body is
// rendered from. Both locales/zh-CN.toml and locales/en-US.toml carry it,
// with placeholders "{{.code}}" and "{{.minutes}}", following the same
// convention as every parameterized error message in this module's own
// catalog (see errors.go's authn.password_too_short, for instance).
const smsVerificationCodeMessageID = "authn.sms.verification_code"

var (
	smsLocaleOnce     sync.Once
	smsLocaleMessages map[string]map[string]string
	smsLocaleErr      error
)

// loadSMSLocaleMessages parses this module's OWN embedded locale files
// once, independently of the merged pkgcore/i18n catalog Registry.Locales()
// exposes.
//
// This is a deliberate, narrower choice than routing through the merged
// catalog, and it is safe only because the message this module renders
// through it never needs another module's text: an SMS body is composed
// and delivered synchronously, inside the request that asked for the code,
// from a bundle whose files (and their key parity) this module already
// owns and already tests in errors_test.go's sibling assertions. Reaching
// for the shared Registry here would mean threading a *pkgcore.ComponentRegistry
// reference into Service for the sole benefit of one message, when the
// files this function reads are already right here.
//
// The language set is supportedLocales(), the module's single statement of
// which languages it ships -- the same set the preference validation and
// the Accept-Language negotiation read (preferences.go) -- so adding a
// language cannot update one consumer and miss this one.
func loadSMSLocaleMessages() (map[string]map[string]string, error) {
	smsLocaleOnce.Do(func() {
		languages := supportedLocales()
		result := make(map[string]map[string]string, len(languages))
		for _, language := range languages {
			raw, err := locales.FS.ReadFile(language + ".toml")
			if err != nil {
				smsLocaleErr = fmt.Errorf("authn: read embedded %s locale: %w", language, err)
				return
			}
			var flat map[string]string
			if err := toml.Unmarshal(raw, &flat); err != nil {
				smsLocaleErr = fmt.Errorf("authn: parse embedded %s locale: %w", language, err)
				return
			}
			result[language] = flat
		}
		smsLocaleMessages = result
	})
	return smsLocaleMessages, smsLocaleErr
}

// smsLocale applies the SMS body's language chain. Its shape is the
// "requester is recipient" tier of the module's locale chains: the request
// that asked for the code comes from the person about to receive it, so
// the request's own language (the frontend chain's resolved value,
// transported in Accept-Language) outranks the account's stored locale,
// which is only the fallback for a client that sent no usable header.
// DefaultLocale closes the chain -- an empty or unknown stored locale
// renders in the platform default rather than failing a delivery that
// cannot be retried in another language.
func smsLocale(acceptLanguage, stored string) string {
	if locale, ok := i18n.Negotiate(acceptLanguage, supportedLocales()); ok {
		return locale
	}
	if locale, ok := canonicalLocale(stored); ok && locale != "" {
		return locale
	}
	return DefaultLocale
}

// renderSMSCode composes the SMS body for a phone-login verification code
// in locale, and reports the locale it ACTUALLY rendered in.
//
// An unknown or empty locale falls back to DefaultLocale rather than
// erroring -- the "never an error, fall back to the platform default" rule
// go/config's own pre-authentication endpoints follow for the same reason:
// an SMS already in flight cannot be retried in a different language, so
// refusing to render it is strictly worse than defaulting. The returned
// usedLocale names the fallback when it applied, and the caller must carry
// IT (never the requested value) on the SMS seam: a template-typed carrier
// adapter maps templates by the locale the body was really rendered in, so
// reporting an unsupported requested locale would fail a delivery the
// render itself handled.
func renderSMSCode(locale, code string, minutes int) (text string, usedLocale string, err error) {
	messages, err := loadSMSLocaleMessages()
	if err != nil {
		return "", "", err
	}
	usedLocale = locale
	bundle, ok := messages[locale]
	if !ok {
		usedLocale = DefaultLocale
		bundle = messages[DefaultLocale]
	}
	template, ok := bundle[smsVerificationCodeMessageID]
	if !ok {
		return "", "", fmt.Errorf("authn: locale bundle carries no %q message", smsVerificationCodeMessageID)
	}
	replacer := strings.NewReplacer("{{.code}}", code, "{{.minutes}}", strconv.Itoa(minutes))
	return replacer.Replace(template), usedLocale, nil
}
