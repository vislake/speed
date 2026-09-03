// This file holds every GORM call authn makes, and it holds a plain *gorm.DB
// rather than embedding dbkit.Repository[T]. That is the documented pattern
// for this data, not a loophole around the isolation guard.
//
// dbkit.Repository[T] is constrained to T: dbkit.TenantScoped, and identity
// data must NOT implement that interface: a person can belong to several
// tenants, so a users row scoped to one tenant makes the multi-tenant case
// unrepresentable. go/dbkit/AGENTS.md's "Known limitations" section covers
// exactly this: the identity and platform domains use dbkit.Open()'s plain
// *gorm.DB, and the compensating control is that every one of these models is
// pinned by tenancytest.AssertNotTenantScoped in model_test.go -- which fails
// loudly if any of them ever starts implementing TenantScoped, and equally
// loudly if a query here ever starts behaving differently depending on what
// tenant happens to be in the context.
//
// Two rules therefore apply to this file specifically:
//
//   - No .Table, .Model or .Raw. They are the three entry points a semgrep
//     rule in repo-checks watches for, and nothing here needs them: every
//     conditional update below passes its target struct to Updates, from
//     which GORM parses the same schema .Model would have named.
//   - No hand-written "WHERE tenant_id = ?", ever. There is no tenant column
//     on any of these tables to filter on, and writing one would mean the
//     model was put in the wrong data domain.
//
// The one table this module owns that IS tenant data -- the per-tenant SSO
// configuration -- does embed dbkit.Repository[T], and lands with the
// federation work.

package authn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// ErrNotFound is returned by every repository read below when no row matched.
// It replaces gorm.ErrRecordNotFound at the module boundary so that callers
// classify a miss without importing GORM's error vocabulary into business
// logic.
var ErrNotFound = errors.New("authn: record not found")

// defaultLoginHistoryLimit bounds an unbounded login-history request. A
// caller that asks for everything gets the most recent page, not a table
// scan.
const defaultLoginHistoryLimit = 50

// translate maps GORM's not-found error onto ErrNotFound and passes anything
// else through unchanged.
func translate(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

// UserRepository reads and writes the users table.
//
// It owns the blind indexers, which is why no caller ever computes an index
// value: the encrypted column and its index column are written from the same
// plaintext in the same call, so they cannot disagree. A caller that could
// set EmailIndex itself could set one that does not match Email, and the
// mismatch would only surface as "this account cannot be signed in to".
type UserRepository struct {
	db         *gorm.DB
	emailIndex *dbkit.BlindIndexer
	phoneIndex *dbkit.BlindIndexer
}

// NewUserRepository binds db together with the blind indexers for the email
// and phone columns. Both indexers are required: an identity column that is
// encrypted but not indexed cannot be looked up at all.
func NewUserRepository(db *gorm.DB, emailIndex, phoneIndex *dbkit.BlindIndexer) (*UserRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewUserRepository requires a database handle")
	}
	if emailIndex == nil || phoneIndex == nil {
		return nil, errors.New("authn: NewUserRepository requires both the email and phone blind indexers")
	}
	return &UserRepository{db: db, emailIndex: emailIndex, phoneIndex: phoneIndex}, nil
}

// EmailIndexOf returns the blind index of raw, for callers that must count or
// look something up by address without storing the address -- the login
// history is the reason this is exported at all.
func (r *UserRepository) EmailIndexOf(raw string) (string, error) {
	value, err := r.emailIndex.Index(raw)
	if err != nil {
		return "", ErrInvalidEmail.WithCause(err)
	}
	return value, nil
}

// PhoneIndexOf returns the blind index of raw in the same way as
// EmailIndexOf.
func (r *UserRepository) PhoneIndexOf(raw string) (string, error) {
	value, err := r.phoneIndex.Index(raw)
	if err != nil {
		return "", ErrInvalidPhone.WithCause(err)
	}
	return value, nil
}

// applyIndexes recomputes u's blind-index columns from its current plaintext.
// A nil identifier clears its index to NULL rather than to the empty string,
// which is what lets any number of accounts have no phone number while the
// unique index still means "one account per number".
func (r *UserRepository) applyIndexes(u *User) error {
	u.EmailIndex = nil
	if u.Email != "" {
		value, err := r.EmailIndexOf(u.Email)
		if err != nil {
			return err
		}
		u.EmailIndex = &value
	}

	u.PhoneIndex = nil
	if u.Phone != "" {
		value, err := r.PhoneIndexOf(u.Phone)
		if err != nil {
			return err
		}
		u.PhoneIndex = &value
	}
	return nil
}

// Create inserts u, deriving its blind-index columns from its plaintext
// identifiers first. u.ID is filled in when it is empty.
func (r *UserRepository) Create(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = newID()
	}
	if u.Status == "" {
		u.Status = UserStatusActive
	}
	if err := r.applyIndexes(u); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(u).Error
}

// Save rewrites u in full, recomputing its blind-index columns so a changed
// address can never leave a stale index behind.
func (r *UserRepository) Save(ctx context.Context, u *User) error {
	if err := r.applyIndexes(u); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Save(u).Error
}

// FindByID returns the user with the given id, or ErrNotFound.
func (r *UserRepository) FindByID(ctx context.Context, id string) (*User, error) {
	var u User
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&u).Error; err != nil {
		return nil, translate(err)
	}
	return &u, nil
}

// FindByEmail returns the user whose email matches raw under the column's own
// normalizer, so "  User@Example.COM " finds the account registered as
// "user@example.com". It returns ErrNotFound when nothing matches, and
// ErrInvalidEmail when raw has no canonical form at all.
func (r *UserRepository) FindByEmail(ctx context.Context, raw string) (*User, error) {
	cond, err := r.emailIndex.Equal(raw)
	if err != nil {
		return nil, ErrInvalidEmail.WithCause(err)
	}
	var u User
	if err := r.db.WithContext(ctx).Where(cond).First(&u).Error; err != nil {
		return nil, translate(err)
	}
	return &u, nil
}

// FindByPhone returns the user whose phone matches raw in E.164 canonical
// form, so "+86 138 0000 0000" and "+8613800000000" find the same account.
func (r *UserRepository) FindByPhone(ctx context.Context, raw string) (*User, error) {
	cond, err := r.phoneIndex.Equal(raw)
	if err != nil {
		return nil, ErrInvalidPhone.WithCause(err)
	}
	var u User
	if err := r.db.WithContext(ctx).Where(cond).First(&u).Error; err != nil {
		return nil, translate(err)
	}
	return &u, nil
}

// SessionRepository reads and writes the sessions table.
type SessionRepository struct {
	db *gorm.DB
}

// NewSessionRepository binds db.
func NewSessionRepository(db *gorm.DB) (*SessionRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewSessionRepository requires a database handle")
	}
	return &SessionRepository{db: db}, nil
}

// Create inserts s, filling in its ID when empty.
func (r *SessionRepository) Create(ctx context.Context, s *Session) error {
	if s.ID == "" {
		s.ID = newID()
	}
	if s.Status == "" {
		s.Status = SessionStatusActive
	}
	return r.db.WithContext(ctx).Create(s).Error
}

// FindByID returns the session with the given id, or ErrNotFound.
func (r *SessionRepository) FindByID(ctx context.Context, id string) (*Session, error) {
	var s Session
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&s).Error; err != nil {
		return nil, translate(err)
	}
	return &s, nil
}

// ListByUser returns a user's sessions, newest first.
func (r *SessionRepository) ListByUser(ctx context.Context, userID string) ([]Session, error) {
	var sessions []Session
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC, id DESC").
		Find(&sessions).Error
	return sessions, err
}

// Revoke marks an ACTIVE session revoked and reports whether it did so.
//
// The status is part of the WHERE clause rather than checked beforehand, so
// two concurrent revocations of the same session produce exactly one winner:
// the database decides, not a read the caller performed a moment earlier.
// A false result means the session was already revoked or never existed --
// both of which leave the caller with the outcome it wanted.
func (r *SessionRepository) Revoke(ctx context.Context, id, reason string, at time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, SessionStatusActive).
		Updates(&Session{Status: SessionStatusRevoked, RevokedAt: &at, RevokeReason: reason})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// TouchLastSeen records that the session was used at t.
func (r *SessionRepository) TouchLastSeen(ctx context.Context, id string, t time.Time) error {
	return r.db.WithContext(ctx).
		Where("id = ?", id).
		Updates(&Session{LastSeenAt: t}).Error
}

// RefreshTokenRepository reads and writes the refresh_tokens table.
type RefreshTokenRepository struct {
	db *gorm.DB
}

// NewRefreshTokenRepository binds db.
func NewRefreshTokenRepository(db *gorm.DB) (*RefreshTokenRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewRefreshTokenRepository requires a database handle")
	}
	return &RefreshTokenRepository{db: db}, nil
}

// Create inserts t, filling in its ID when empty.
func (r *RefreshTokenRepository) Create(ctx context.Context, t *RefreshToken) error {
	if t.ID == "" {
		t.ID = newID()
	}
	if t.Status == "" {
		t.Status = RefreshTokenStatusActive
	}
	return r.db.WithContext(ctx).Create(t).Error
}

// FindByHash returns the token row whose stored digest is hash, or
// ErrNotFound. Looking a presented token up by its digest -- rather than
// carrying a row id alongside it -- means the plaintext token is never needed
// for anything but this one comparison, and never stored anywhere.
func (r *RefreshTokenRepository) FindByHash(ctx context.Context, hash string) (*RefreshToken, error) {
	var t RefreshToken
	if err := r.db.WithContext(ctx).Where("token_hash = ?", hash).First(&t).Error; err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

// Consume atomically moves an ACTIVE token to rotated and reports whether it
// won the race.
//
// This is the single most important query in the module. Two refreshes
// arriving with the same token at the same instant must produce exactly one
// new token and exactly one loser, and the loser must be indistinguishable
// from a replay -- because it might be one. Deciding that with a read
// followed by a write would let both callers read "active" and both proceed;
// putting the status in the WHERE clause makes the database the arbiter, on
// both dialects, without a transaction or a row lock (SELECT ... FOR UPDATE
// does not exist on SQLite, so it could not have been the mechanism anyway).
func (r *RefreshTokenRepository) Consume(ctx context.Context, id string, at time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, RefreshTokenStatusActive).
		Updates(&RefreshToken{Status: RefreshTokenStatusRotated, ConsumedAt: &at})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// RevokeFamily invalidates every still-active token descended from one login
// and reports how many it invalidated. It is what replay detection calls: the
// point is to kill the token the thief just rotated for themselves, not only
// the one that was replayed.
func (r *RefreshTokenRepository) RevokeFamily(ctx context.Context, familyID string, at time.Time) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("family_id = ? AND status = ?", familyID, RefreshTokenStatusActive).
		Updates(&RefreshToken{Status: RefreshTokenStatusRevoked, ConsumedAt: &at})
	return res.RowsAffected, res.Error
}

// RevokeBySession invalidates every still-active token bound to a session,
// which is what makes revoking a session take effect on the refresh path
// immediately rather than at the next natural expiry.
func (r *RefreshTokenRepository) RevokeBySession(ctx context.Context, sessionID string, at time.Time) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("session_id = ? AND status = ?", sessionID, RefreshTokenStatusActive).
		Updates(&RefreshToken{Status: RefreshTokenStatusRevoked, ConsumedAt: &at})
	return res.RowsAffected, res.Error
}

// LoginAttemptRepository reads and writes the login_attempts table.
type LoginAttemptRepository struct {
	db *gorm.DB
}

// NewLoginAttemptRepository binds db.
func NewLoginAttemptRepository(db *gorm.DB) (*LoginAttemptRepository, error) {
	if db == nil {
		return nil, errors.New("authn: NewLoginAttemptRepository requires a database handle")
	}
	return &LoginAttemptRepository{db: db}, nil
}

// Create inserts a, filling in its ID when empty.
func (r *LoginAttemptRepository) Create(ctx context.Context, a *LoginAttempt) error {
	if a.ID == "" {
		a.ID = newID()
	}
	return r.db.WithContext(ctx).Create(a).Error
}

// ListByUser returns a user's own login history, newest first, bounded by
// limit. A limit of zero or less uses defaultLoginHistoryLimit: an unbounded
// history query is a table scan waiting to happen, and no caller genuinely
// needs one.
func (r *LoginAttemptRepository) ListByUser(ctx context.Context, userID string, limit int) ([]LoginAttempt, error) {
	if limit <= 0 {
		limit = defaultLoginHistoryLimit
	}
	var attempts []LoginAttempt
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&attempts).Error
	return attempts, err
}

// RegisterPIISerializer wires cipher into GORM's serializer registry under
// SerializerName, so this module's encrypted columns are transparently sealed
// on write and opened on read.
//
// Call it during host bootstrap, BEFORE opening the *gorm.DB authn's models
// are used with. GORM's registry is process-global and is consulted while a
// model's schema is parsed, so registering afterwards leaves the parsed
// schema pointing at nothing. This is deliberately NOT done inside
// Module.Register: by then the database is already open, and Register is also
// forbidden from doing anything but declare.
func RegisterPIISerializer(cipher *dbkit.Cipher) error {
	if cipher == nil {
		return fmt.Errorf("authn: RegisterPIISerializer requires a cipher for %q", SerializerName)
	}
	dbkit.RegisterEncryptedSerializer(SerializerName, cipher)
	return nil
}

// SetCurrentTenant records which tenant a session's access tokens are now
// being issued for. It is what a tenant switch persists; the caller has
// already verified membership in tenantID, because this method does not and
// cannot.
func (r *SessionRepository) SetCurrentTenant(ctx context.Context, id string, tenantID pkgcore.TenantID) error {
	return r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, SessionStatusActive).
		Updates(&Session{CurrentTenantID: string(tenantID)}).Error
}
