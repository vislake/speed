package authn

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// SerializerName is the name authn's models reference in their
// `gorm:"serializer:..."` tags for every field encrypted at rest.
//
// The host registers a dbkit.Cipher under this name with
// [RegisterPIISerializer] BEFORE opening the *gorm.DB these models are used
// with. That ordering is not a style preference: GORM's serializer registry
// is process-global and is consulted while a model's schema is parsed, so a
// registration that happens after the database is open leaves the already
// parsed schema pointing at nothing and the first write fails.
const SerializerName = "authn_pii"

// Account status values for [User.Status].
const (
	// UserStatusActive is a user who may authenticate.
	UserStatusActive = "active"
	// UserStatusSuspended is a user an operator has disabled. Credentials
	// still verify but no session is issued, so a suspended account
	// reports the same generic failure as a wrong password.
	UserStatusSuspended = "suspended"
)

// Session status values for [Session.Status].
const (
	// SessionStatusActive is a session that may still refresh.
	SessionStatusActive = "active"
	// SessionStatusRevoked is a session that was signed out, either by its
	// owner, by an operator, or automatically by refresh-token replay
	// detection.
	SessionStatusRevoked = "revoked"
)

// Refresh-token status values for [RefreshToken.Status].
const (
	// RefreshTokenStatusActive is a token that has not been used yet and
	// is the only status a refresh may consume.
	RefreshTokenStatusActive = "active"
	// RefreshTokenStatusRotated is a token that was consumed by a
	// successful refresh. Presenting one again is the replay signal.
	RefreshTokenStatusRotated = "rotated"
	// RefreshTokenStatusRevoked is a token invalidated without being used,
	// because its session or its whole family was revoked.
	RefreshTokenStatusRevoked = "revoked"
)

// Reasons recorded in [Session.RevokeReason].
const (
	// RevokeReasonLogout is an ordinary sign-out by the session's owner.
	RevokeReasonLogout = "logout"
	// RevokeReasonReplay is an automatic revocation triggered by a
	// consumed refresh token being presented a second time.
	RevokeReasonReplay = "replay_detected"
)

// Results recorded in [LoginAttempt.Result].
const (
	// LoginResultSuccess records an attempt that produced a session.
	LoginResultSuccess = "success"
	// LoginResultFailure records an attempt that did not.
	LoginResultFailure = "failure"
)

// Authentication methods recorded in [LoginAttempt.Method] and, for a
// successful attempt, contributed to the session's AMR.
const (
	// MethodPassword is an email-or-phone plus password sign-in.
	MethodPassword = "password"
	// MethodSocial is a sign-in through a social channel. The session's
	// AMR carries the channel too ("social:google"); this coarser value is
	// what the login history records, so a person reading their own
	// security page sees one row type per way in rather than one per
	// vendor.
	MethodSocial = "social"
	// MethodOIDC is a sign-in through a tenant's enterprise single
	// sign-on.
	MethodOIDC = "oidc"
)

// Failure reasons recorded in [LoginAttempt.FailureReason]. They are for the
// operator's log and the owner's security page; the API response never
// distinguishes them, because telling a caller which of "no such account" and
// "wrong password" happened is exactly the user-enumeration oracle
// [Service.Login] refuses to be.
const (
	// FailureReasonUnknownUser is an identifier that matched no account.
	FailureReasonUnknownUser = "unknown_user"
	// FailureReasonBadPassword is a known account with a wrong password.
	FailureReasonBadPassword = "bad_password"
	// FailureReasonNoPassword is a known account that has no password set
	// at all, for example a social-only account.
	FailureReasonNoPassword = "no_password"
	// FailureReasonSuspended is a known account an operator disabled.
	FailureReasonSuspended = "suspended"
	// FailureReasonNoMembership is a correct credential for a user who is
	// not an active member of any tenant, or of the requested one.
	FailureReasonNoMembership = "no_membership"
	// FailureReasonRequiresBinding is an external identity that resolved
	// to an existing account by email address without satisfying the
	// automatic-linking rule.
	FailureReasonRequiresBinding = "requires_binding"
)

// User is a person who can authenticate. It is IDENTITY-domain data: a person
// may belong to several tenants, so the row carries no tenant_id and this type
// deliberately does not implement dbkit.TenantScoped. Adding a TenantID field
// -- or embedding dbkit.TenantModel -- would silently turn every user into a
// per-tenant duplicate and is caught by the AssertNotTenantScoped suite in
// model_test.go.
//
// Email and Phone are plain strings with "" meaning "this account has none",
// while EmailIndex and PhoneIndex are POINTERS so that an absent identifier
// stores SQL NULL. The split is forced by dbkit's encrypted serializer, which
// accepts a string or a []byte field and rejects a *string outright, and it
// works out correctly because the index column is the authoritative answer to
// "does this account have an email": both dialects allow any number of NULLs
// in a unique index, so every account without a phone number coexists, while
// an empty string in that column would collide on the second one.
//
// The consequence to know: for an account with no email the ciphertext column
// holds a sealed empty string rather than NULL. Nothing reads it -- lookups
// go through the index column -- so it costs a few bytes and no correctness.
type User struct {
	// ID is an application-generated UUID (root CLAUDE.md bans
	// gen_random_uuid(), which exists on only one of the two dialects).
	ID string `gorm:"primaryKey;size:36"`

	// Email is the account's email address, encrypted at rest, or "" when
	// the account has none. It cannot be filtered on; EmailIndex is what a
	// lookup uses.
	Email string `gorm:"serializer:authn_pii"`

	// EmailIndex is the HMAC blind index of the canonical (lowercased,
	// trimmed) email, unique across accounts. Never set it by hand -- it
	// is written by UserRepository from the same plaintext that gets
	// encrypted, through dbkit's blind indexer.
	EmailIndex *string `gorm:"column:email_index;size:64;uniqueIndex"`

	// Phone is the account's phone number, encrypted at rest, or "" when
	// the account has none.
	Phone string `gorm:"serializer:authn_pii"`

	// PhoneIndex is the HMAC blind index of the E.164 form of Phone,
	// unique across accounts.
	PhoneIndex *string `gorm:"column:phone_index;size:64;uniqueIndex"`

	// PasswordHash is a PHC-encoded argon2id digest, or the empty string
	// for an account with no password at all (social-only or SSO-only).
	PasswordHash string `gorm:"size:255;not null"`

	// DisplayName is the name shown in the product. It is not an
	// identifier and is not unique.
	DisplayName string `gorm:"size:128;not null"`

	// Locale is the recipient locale backend-generated content is rendered
	// in -- emails and notifications follow the RECIPIENT's language, not
	// the operator's UI language. Empty means "not chosen yet"; the caller
	// falls back to the platform default.
	Locale string `gorm:"size:16;not null"`

	// Status is one of the UserStatus* constants.
	Status string `gorm:"size:32;not null"`

	// EmailVerified reports whether the address completed a verification
	// round trip here. It is never inherited from a social provider's own
	// claim without the trusted-list check that governs account linking.
	EmailVerified bool `gorm:"not null"`

	// PhoneVerified reports whether the phone number completed a
	// verification round trip here.
	PhoneVerified bool `gorm:"not null"`

	// CreatedAt and UpdatedAt are maintained by GORM rather than by a SQL
	// default, because NOW() is banned across the two dialects.
	CreatedAt time.Time `gorm:"autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"autoUpdateTime;not null"`
}

// TableName pins the table name so a rename of the Go type cannot silently
// repoint the model at a table the migrations never created.
func (User) TableName() string { return "users" }

// Session is one login. It belongs to a USER, never to a tenant: the user
// switches tenants inside one session, and each switch mints a new access
// token carrying the new tenant while this row and its refresh-token family
// stay as they are. Identity-domain data, like User.
type Session struct {
	// ID is an application-generated UUID. It travels in the access
	// token's "sid" claim, which is what makes immediate revocation
	// checkable without a database read.
	ID string `gorm:"primaryKey;size:36"`

	// UserID is the owning user's User.ID. It is deliberately not a
	// foreign key: cross-module foreign keys are banned repository-wide,
	// and even same-module ones would make independently released
	// migrations and cascading deletes unmanageable.
	UserID string `gorm:"column:user_id;size:36;not null;index:idx_sessions_user_id,priority:1"`

	// Status is one of the SessionStatus* constants.
	Status string `gorm:"size:32;not null"`

	// CurrentTenantID is the tenant this session's access tokens are
	// CURRENTLY issued for. It is NOT a tenant-scoping column and it does
	// not make this model tenant data: the session still belongs to the
	// user and still spans every tenant they are a member of. It exists
	// because a refresh has to know which tenant to mint the next access
	// token for, and a tenant switch has to record itself somewhere other
	// than in the token it just replaced.
	//
	// Membership is re-verified against it on every refresh rather than
	// trusted, which is what makes removing someone from a tenant end
	// their access to it instead of waiting for the session to expire.
	CurrentTenantID string `gorm:"column:current_tenant_id;size:64;not null"`

	// AMR is the space-delimited authentication-method list this session
	// was established with. Read and write it through AMRList and SetAMR
	// rather than parsing the string at a call site.
	AMR string `gorm:"column:amr;size:255;not null"`

	// Device, UserAgent and IP describe the client, for the owner's device
	// list. They are recorded as received.
	Device    string `gorm:"size:255;not null"`
	UserAgent string `gorm:"column:user_agent;size:512;not null"`
	IP        string `gorm:"column:ip;size:45;not null"`

	// IPRegion is the resolved region of IP. It ships empty: the GeoIP
	// database that would fill it needs a licence review first, so the
	// column exists and the resolver lands later.
	IPRegion string `gorm:"column:ip_region;size:128;not null"`

	CreatedAt  time.Time `gorm:"autoCreateTime;not null;index:idx_sessions_user_id,priority:2"`
	LastSeenAt time.Time `gorm:"column:last_seen_at;not null"`

	// ExpiresAt bounds the session itself, independently of any one
	// refresh token: a session that is never used still stops working.
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`

	// RevokedAt is nil while Status is active.
	RevokedAt *time.Time `gorm:"column:revoked_at"`

	// RevokeReason is one of the RevokeReason* constants, or empty.
	RevokeReason string `gorm:"column:revoke_reason;size:64;not null"`
}

// TableName pins the table name.
func (Session) TableName() string { return "sessions" }

// RefreshToken is one issued refresh credential. The opaque secret itself is
// never stored: TokenHash holds its SHA-256 digest and is what a presented
// token is looked up by. Identity-domain data.
type RefreshToken struct {
	// ID is an application-generated UUID.
	ID string `gorm:"primaryKey;size:36"`

	// SessionID binds the token to a session. Revoking the session
	// invalidates every token bound to it.
	SessionID string `gorm:"column:session_id;size:36;not null;index"`

	// UserID is denormalized from the session so that revoking a whole
	// family never needs a join.
	UserID string `gorm:"column:user_id;size:36;not null"`

	// FamilyID groups every token descended from one login. Replay
	// detection revokes the family, not just the presented token.
	FamilyID string `gorm:"column:family_id;size:36;not null;index"`

	// RotatedFrom is the RefreshToken.ID this one replaced, or empty for
	// the first token of a family.
	RotatedFrom string `gorm:"column:rotated_from;size:36;not null"`

	// TokenHash is the hex SHA-256 digest of the opaque token. It is
	// unique, which is what makes a lookup by presented token a single
	// indexed read.
	TokenHash string `gorm:"column:token_hash;size:64;not null;uniqueIndex"`

	// Status is one of the RefreshTokenStatus* constants.
	Status string `gorm:"size:32;not null"`

	CreatedAt time.Time `gorm:"autoCreateTime;not null"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`

	// ConsumedAt is when a refresh rotated this token away, nil while it
	// is still active.
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
}

// TableName pins the table name.
func (RefreshToken) TableName() string { return "refresh_tokens" }

// LoginAttempt is one recorded authentication attempt, successful or not.
// Identity-domain data.
//
// It stores IdentifierIndex rather than the attempted email or phone. An
// attempt against an address matching no account still has to be countable
// per address -- that is how credential stuffing is spotted -- but recording
// the plaintext would turn this table into a log of every address anyone ever
// typed at the login form, most of which never belonged to a user here.
type LoginAttempt struct {
	// ID is an application-generated UUID.
	ID string `gorm:"primaryKey;size:36"`

	// UserID is the matched account, or empty when the identifier matched
	// none.
	UserID string `gorm:"column:user_id;size:36;not null;index:idx_login_attempts_user_id,priority:1"`

	// IdentifierIndex is the blind index of the attempted identifier,
	// computed with the same indexer the users table is looked up by, so
	// an attempt is countable per account without storing the address.
	IdentifierIndex string `gorm:"column:identifier_index;size:64;not null;index:idx_login_attempts_identifier,priority:1"`

	// Method is one of the Method* constants.
	Method string `gorm:"size:32;not null"`

	// Result is one of the LoginResult* constants.
	Result string `gorm:"size:16;not null"`

	// FailureReason is one of the FailureReason* constants for a failure,
	// empty for a success. It never reaches an API response.
	FailureReason string `gorm:"column:failure_reason;size:64;not null"`

	// SessionID is the session a successful attempt created, empty
	// otherwise.
	SessionID string `gorm:"column:session_id;size:36;not null"`

	IP        string `gorm:"column:ip;size:45;not null"`
	IPRegion  string `gorm:"column:ip_region;size:128;not null"`
	UserAgent string `gorm:"column:user_agent;size:512;not null"`

	CreatedAt time.Time `gorm:"autoCreateTime;not null;index:idx_login_attempts_user_id,priority:2;index:idx_login_attempts_identifier,priority:2"`
}

// TableName pins the table name.
func (LoginAttempt) TableName() string { return "login_attempts" }

// AMRList returns the session's authentication methods as a slice.
//
// The stored form is whitespace-delimited, following the JWT and OAuth
// convention for "amr" and "scope" rather than a PostgreSQL native array
// (dialect-specific, banned) or a JSON document that would then have to be
// filtered with JSONB operators (also banned). A consequence worth stating
// plainly: a method name containing whitespace is not representable, which is
// fine because every name in the closed Method* set is a single token.
func (s *Session) AMRList() []string {
	return strings.Fields(s.AMR)
}

// SetAMR stores methods in the session's delimited AMR field, dropping empty
// entries and splitting any entry that itself contains whitespace, so the
// round trip through AMRList is always exact.
func (s *Session) SetAMR(methods []string) {
	fields := make([]string, 0, len(methods))
	for _, m := range methods {
		fields = append(fields, strings.Fields(m)...)
	}
	s.AMR = strings.Join(fields, " ")
}

// newID returns a fresh application-generated identifier for a new row.
//
// IDs are generated here rather than by the database because
// gen_random_uuid() exists on only one of the two supported dialects, and a
// schema that depends on it cannot run in the standalone deployment mode at
// all.
func newID() string {
	return uuid.NewString()
}
