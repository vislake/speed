package org

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// tableInvitations is the org_invitations table name, shared by the model's
// TableName and by the migrations' header comments.
const tableInvitations = "org_invitations"

// EmailSerializerName is the GORM serializer name the Invitation.Email
// column is encrypted under.
//
// A host must register it once during bootstrap, before opening the
// *gorm.DB org's tables live in:
//
//	dbkit.RegisterEncryptedSerializer(org.EmailSerializerName, cipher)
//
// The name is exported precisely so a host never spells it as a literal.
// The cipher's key MUST be a different secret from the HMAC key given to
// WithEmailIndexer: an AES key and a blind-index key that are the same
// 32 bytes weaken both, which is why dbkit takes them through two separate
// constructors rather than one.
const EmailSerializerName = "org_email_enc"

// The lifecycle states an Invitation can be in. Closed set, kept in Go for
// the same reason MembershipStatus* are: PostgreSQL has enum types and
// SQLite does not, so the column is a plain VARCHAR on both engines.
const (
	// InvitationStatusPending is an invitation that has been sent and
	// neither accepted nor withdrawn. It may still expire.
	InvitationStatusPending = "pending"
	// InvitationStatusAccepted is an invitation that produced a membership.
	InvitationStatusAccepted = "accepted"
	// InvitationStatusRevoked is an invitation the tenant withdrew.
	InvitationStatusRevoked = "revoked"
)

// invitationTokenBytes is the entropy of an invitation token. 32 bytes from
// crypto/rand is the same strength dbkit requires of an encryption key, and
// the token is a bearer credential: whoever holds it joins the tenant.
const invitationTokenBytes = 32

// Invitation is a pending offer to join a tenant at a particular node of its
// organization tree.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md): an invitation belongs
// to exactly one tenant and must never be visible from another. It
// implements dbkit.TenantScoped, is reached only through
// InvitationRepository, and its isolation is proven by
// tenancytest.AssertIsolated.
//
// # The token is never stored
//
// The value that goes into the invitation link is 32 random bytes, handed to
// the caller ONCE by InviteService.Invite and never persisted. What the row
// keeps is TokenHash, the SHA-256 of that value. A leaked database backup
// therefore yields no usable invitation link, and acceptance is a hash
// lookup rather than a comparison against a stored secret.
//
// # The address is encrypted, and still queryable
//
// Email is an email address: PII, and encrypted at rest through the
// serializer named by EmailSerializerName. An encrypted column cannot be
// queried, which is exactly the trap the root CLAUDE.md warns about, so
// EmailIndex carries the HMAC blind index dbkit.NewBlindIndexer computes
// over dbkit.NormalizeEmail's canonical form. Every write goes through
// BlindIndexer.Index and every lookup through BlindIndexer.Equal; org
// reimplements neither.
//
// # Cross-module references
//
// InviterUserID names a row in authn's users table with no foreign key, for
// the same reason Membership.UserID does.
type Invitation struct {
	// ID is an application-generated UUID.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID.
	dbkit.TenantModel

	// NodeID is the node the invitee will be bound to on acceptance.
	NodeID string `gorm:"column:node_id;size:36;not null"`

	// Email is the invitee's address, encrypted at rest. It is never logged,
	// never put in an event payload and never echoed in an error parameter.
	Email string `gorm:"column:email;serializer:org_email_enc;not null"`

	// EmailIndex is the HMAC blind index of Email, the only way this table
	// can be searched by address. It is safe to log and to put in an event:
	// it is a keyed digest, not the address.
	EmailIndex string `gorm:"column:email_index;size:64;not null;index"`

	// InviterUserID is the member who issued the invitation.
	InviterUserID string `gorm:"column:inviter_user_id;size:64;not null"`

	// Locale is the language the invitation message was rendered in, chosen
	// when the invitation was created: the OrgCreateInvitationRequest.locale
	// the inviting operator named on the invitee's behalf, or the platform
	// default when they named none. It is deliberately never the operator's
	// own Accept-Language header -- the invitee has made no request of their
	// own yet for the server to read a preference from, and the message must
	// render in the RECIPIENT's language, never the operator's UI language.
	// It is captured on the row rather than looked up at send time because
	// the recipient may not be a user at all and so has no profile to read a
	// preference from either.
	Locale string `gorm:"column:locale;size:16;not null"`

	// TokenHash is the hex-encoded SHA-256 of the invitation token. The
	// token itself is never stored.
	TokenHash string `gorm:"column:token_hash;size:64;not null"`

	// Status is one of the InvitationStatus* constants.
	Status string `gorm:"column:status;size:16;not null"`

	// ExpiresAt is when the invitation stops being acceptable. It is checked
	// at acceptance time against the service clock, so no sweeper is needed
	// for correctness.
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`

	// AcceptedAt is when the invitation was accepted, nil while it was not.
	AcceptedAt *time.Time `gorm:"column:accepted_at"`

	// CreatedAt and UpdatedAt are written by gorm's autoCreateTime /
	// autoUpdateTime.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the org_invitations table.
func (Invitation) TableName() string { return tableInvitations }

// IsPending reports whether the invitation is still open at the given
// moment: neither accepted nor revoked, and not yet expired.
func (i Invitation) IsPending(now time.Time) bool {
	return i.Status == InvitationStatusPending && now.Before(i.ExpiresAt)
}

// compile-time check that Invitation satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Invitation{}

// maxEmailLen is the longest address org accepts, the length limit RFC 5321
// puts on a forward path. Bounded in Go for the same reason node names are:
// SQLite does not enforce a column width under type affinity, and the column
// here holds ciphertext anyway, which is longer than the plaintext.
const maxEmailLen = 254

// validateInviteEmail returns the trimmed address, or ErrInvalidEmail.
//
// The check is deliberately syntactic and minimal -- one "@", a non-empty
// local part, a domain with a dot in it, no whitespace or control
// characters, and a length bound -- rather than an attempt at RFC 5322.
// Anything stricter rejects addresses that genuinely deliver; anything looser
// lets org spend a rate-limit slot and a mail attempt on a value that cannot
// possibly be one.
//
// It exists because dbkit.NormalizeEmail explicitly does NOT do it: that
// function's contract is to normalize consistently on both sides of a lookup
// and it documents structural validity as "the caller's input-quality
// problem". org is that caller, and this is where it holds up its end.
//
// The address is never echoed into the error: an error's parameters are
// rendered, logged and traced, and an address is PII.
func validateInviteEmail(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	if address == "" || len(address) > maxEmailLen {
		return "", ErrInvalidEmail
	}
	local, domain, found := strings.Cut(address, "@")
	if !found || local == "" || domain == "" {
		return "", ErrInvalidEmail
	}
	if strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return "", ErrInvalidEmail
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", ErrInvalidEmail
	}
	for _, r := range address {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", ErrInvalidEmail
		}
	}
	return address, nil
}

// newInvitationToken returns a fresh invitation token and its stored hash.
//
// The token is base64url-encoded so it survives a URL query string without
// escaping; the hash is hex so the column is a fixed 64 characters on both
// engines. A failure of crypto/rand is fatal to the operation and is
// reported, never worked around with a weaker source.
func newInvitationToken() (token, hash string, err error) {
	raw := make([]byte, invitationTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("org: generating an invitation token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashInvitationToken(token), nil
}

// hashInvitationToken returns the hex-encoded SHA-256 of an invitation
// token: what the row stores, and what a lookup is keyed on.
//
// SHA-256 rather than a password hash is deliberate and correct here: the
// input is 32 bytes of full-entropy randomness, not a human-chosen secret,
// so there is no dictionary to slow an attacker down with.
func hashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// InvitationRepository is org's tenant-scoped data-access type for
// Invitation.
//
// Same construction rules as Repository and MembershipRepository: the extra
// query shapes compose on the *gorm.DB the isolation plugin protects,
// against a TenantScoped destination, inside dbkit.WithTenantSession. No
// hand-written tenant predicate, no db.Table / db.Model / db.Raw.
type InvitationRepository struct {
	*dbkit.Repository[Invitation]

	db *gorm.DB
}

// NewInvitationRepository returns an InvitationRepository backed by db.
func NewInvitationRepository(db *gorm.DB) *InvitationRepository {
	return &InvitationRepository{Repository: dbkit.NewRepository[Invitation](db), db: db}
}

// byTokenHash returns the invitation of the caller's tenant whose stored
// hash is hash, or ErrInvitationNotFound.
//
// The tenant scoping is the security property that matters here: a token
// minted for another tenant simply does not match, so nothing about it can
// be learned, and the tenant is never taken from the token itself. See
// InviteService.Accept for why that ordering is not negotiable.
func (r *InvitationRepository) byTokenHash(ctx context.Context, hash string) (*Invitation, error) {
	var inv Invitation
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("token_hash = ?", hash).First(&inv).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrInvitationNotFound
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &inv, nil
}

// acceptIfPending atomically transitions the invitation identified by id,
// scoped to the caller's tenant, from InvitationStatusPending to
// InvitationStatusAccepted, stamping AcceptedAt, and reports whether this
// call is the one that performed the transition.
//
// The "status = ?" condition on the UPDATE below is a compare-and-swap
// guard that dbkit.Repository[T].Update cannot express -- it writes
// unconditionally once id and tenant match. Without this guard, two
// concurrent InviteService.Accept calls racing on the same token could both
// read InvitationStatusPending before either commits, and both would go on
// to create a membership: exactly the single-use violation this method
// exists to close. Gating the write itself on the row still being pending
// means at most one caller ever observes won == true; a caller that
// observes false lost the race (or the invitation was revoked out from
// under it) and MUST NOT create a membership.
func (r *InvitationRepository) acceptIfPending(ctx context.Context, id string, acceptedAt time.Time) (won bool, err error) {
	var rowsAffected int64
	dbErr := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("status = ?", InvitationStatusPending).
			Updates(&Invitation{Status: InvitationStatusAccepted, AcceptedAt: &acceptedAt})
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if dbErr != nil {
		return false, ErrInternal.WithCause(dbErr)
	}
	return rowsAffected == 1, nil
}

// byStatus returns the caller tenant's invitations in the given status,
// newest first and then by id so the order is total and stable.
func (r *InvitationRepository) byStatus(ctx context.Context, status string) ([]Invitation, error) {
	var out []Invitation
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("status = ?", status).
			Order("created_at DESC, id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// pendingByEmail returns the caller tenant's pending invitations for one
// address, looked up through the blind index.
//
// The address never appears in the SQL: indexer.Equal normalizes it exactly
// as the write path did and yields a condition on the index column alone.
// Passing the raw address to Equal rather than a precomputed digest is
// dbkit's own contract -- it is what makes a lookup under a different
// normalization impossible.
func (r *InvitationRepository) pendingByEmail(ctx context.Context, indexer *dbkit.BlindIndexer, email string) ([]Invitation, error) {
	cond, err := indexer.Equal(email)
	if err != nil {
		return nil, ErrInvalidEmail.WithCause(err)
	}
	var out []Invitation
	dbErr := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where(cond).
			Where("status = ?", InvitationStatusPending).
			Order("created_at DESC, id").
			Find(&out).Error
	})
	if dbErr != nil {
		return nil, ErrInternal.WithCause(dbErr)
	}
	return out, nil
}
