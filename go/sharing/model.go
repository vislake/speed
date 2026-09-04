package sharing

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// Table names, shared between the models' TableName methods and the
// migrations' own header comments.
const (
	tableShares    = "sharing_shares"
	tableAccessLog = "sharing_access_log"
)

// The AccessOutcome vocabulary access log rows record. Kept in Go, not a
// database enum, for the same dual-dialect reason every other status column
// in this codebase is a plain VARCHAR: PostgreSQL has enum types and SQLite
// does not.
const (
	// AccessOutcomeGranted records a request that resolved to a live share,
	// passed its password check (if any), and was allowed to count as a
	// view.
	AccessOutcomeGranted = "granted"
	// AccessOutcomeDenied records every other outcome -- an unknown token,
	// a revoked, expired or view-exhausted share, or a missing/wrong
	// password -- deliberately without distinguishing which. Access's own
	// doc comment explains why the reasons collapse into one outward
	// answer; the log keeps the same non-distinction rather than leaking
	// the distinction into an owner-facing report that could itself be
	// scraped.
	AccessOutcomeDenied = "denied"
)

// Share is one public share link: a controlled, unauthenticated entry point
// onto one internal resource.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md): a share link belongs
// to the tenant whose resource it exposes. Share implements
// dbkit.TenantScoped (via the embedded dbkit.TenantModel), is reached only
// through ShareRepository (which embeds dbkit.Repository[Share]), and its
// isolation is proven by tenancytest.AssertIsolated.
//
// # The token is never stored
//
// The bearer credential a viewer presents -- 32 bytes of crypto/rand,
// base64url-encoded -- is returned to the caller of Service.Create EXACTLY
// ONCE and never persisted, the identical discipline org.Invitation's own
// doc comment describes for its own token. What this row keeps is
// TokenHash, the hex-encoded SHA-256 of that value: a leaked database
// backup yields no usable share link, and Service.Access's lookup is a hash
// comparison rather than a comparison against a stored secret. See token.go.
//
// # ExpiresAt is never nil at rest
//
// The design sketch this type implements
// (docs/internal/07-platform-services.md) shows ExpiresAt as an optional
// *time.Time, and the Go type here matches that shape -- Service.Create's
// CreateParams.ExpiresAt really is optional, a caller may leave it nil and
// get the tenant's configured default. But this row's own ExpiresAt is
// NEVER actually nil once a Share has been created: rule 2
// (docs/internal/07-platform-services.md's "default expiry" rule) requires
// every share to carry an expiry, and Service.Create resolves a nil request into
// a concrete time before the row is ever written, refusing outright
// (ErrExpiryRequired) a caller that explicitly asks for one that never
// expires. The column itself is declared NOT NULL as a second,
// database-level line of defense: an accidental nil reaching Create would
// fail loudly at INSERT rather than silently persisting a foreverShare.
//
// # PasswordHash is hashed, never plaintext
//
// An optional per-share access password is stored only as its argon2id PHC
// digest (password.go) -- the identical discipline authn.User.PasswordHash
// uses for account passwords, at fixed, un-configurable cost parameters
// this round (see password.go's own doc comment for why that is an
// accepted, documented limitation rather than an oversight).
//
// # Cross-module references
//
// ResourceRef is an opaque string this module never interprets: it is
// whatever the caller of Create supplied, typically a reference another
// module's own key scheme produces (e.g. go/storage's object id), stored
// as plain data with no foreign key -- cross-module foreign keys are
// forbidden in this codebase (docs/internal/04-data-and-tenancy.md rule 4),
// and sharing does not import go/storage or any other resource-owning
// module to interpret it. Resolving ResourceRef into actual bytes is a
// later round's job (AGENTS.md's Known limitations).
type Share struct {
	// ID is an application-generated UUID.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID.
	dbkit.TenantModel

	// ResourceRef is the opaque reference to the resource this share
	// exposes. Never an internal database id exposed raw -- see the type's
	// own doc comment.
	ResourceRef string `gorm:"column:resource_ref;size:512;not null"`

	// TokenHash is the hex-encoded SHA-256 of the share token. The token
	// itself is never stored; see the type's own doc comment.
	TokenHash string `gorm:"column:token_hash;size:64;not null"`

	// ExpiresAt is when this share stops being accessible. Never nil once
	// the row exists -- see the type's own doc comment for the rule this
	// enforces and why the Go type still carries a pointer.
	ExpiresAt *time.Time `gorm:"column:expires_at;not null"`

	// MaxViews, when non-nil, caps the number of granted accesses this
	// share may ever record. Nil means unlimited views (still subject to
	// ExpiresAt).
	MaxViews *int `gorm:"column:max_views"`

	// ViewCount is how many accesses have been granted so far. It is
	// incremented only by a granted access (service.go's Service.Access),
	// through a compare-and-swap guard (repository.go's
	// ShareRepository.tryRecordView) rather than a raw SQL increment -- see
	// that method's own doc comment for why.
	ViewCount int `gorm:"column:view_count;not null;default:0"`

	// PasswordHash, when non-nil, is the argon2id PHC digest of an
	// additional access password a viewer must present. Never plaintext --
	// see the type's own doc comment.
	PasswordHash *string `gorm:"column:password_hash;size:255"`

	// Sensitive records whether the caller of Create declared the shared
	// resource as carrying sensitive personal information -- rule 4
	// (docs/internal/07-platform-services.md's "sensitive resource sharing
	// needs confirmation" rule). A true value is what makes Create fire the
	// sensitive-share audit action (module.go's
	// AuditActionSensitiveShareCreate). Kept on the row itself (rather than
	// only in the audit trail) so an owner-facing
	// listing can flag it without a second lookup.
	Sensitive bool `gorm:"column:sensitive;not null;default:false"`

	// RevokedAt is nil for a live share, and the moment Service.Revoke
	// withdrew it otherwise. Once set, Service.Access refuses every access
	// on the very next call -- there is no cache to invalidate on this
	// module's own side; see AGENTS.md's "Revocation and caching" section
	// for the obligation this places on any future HTTP layer.
	RevokedAt *time.Time `gorm:"column:revoked_at"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the sharing_shares table.
func (Share) TableName() string { return tableShares }

// isLive reports whether the share may still be accessed as of now: not
// revoked, not expired, and (if MaxViews is set) not yet exhausted. It is
// the single predicate Service.Access and the expiry sweep both evaluate,
// so "what makes a share accessible" has exactly one definition in this
// module.
func (s Share) isLive(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	if s.ExpiresAt == nil || !now.Before(*s.ExpiresAt) {
		return false
	}
	if s.MaxViews != nil && s.ViewCount >= *s.MaxViews {
		return false
	}
	return true
}

// compile-time check that Share satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Share{}

// AccessLogEntry is one recorded access attempt against a Share -- granted
// or denied alike, per rule 4 (docs/internal/07-platform-services.md's
// "access needs no login, but must leave a trail" rule): a resource owner
// reads this back through Service.ListAccessLog to answer "who viewed this
// and how many times", and
// recording denied attempts too gives the owner the fuller picture (an
// exhausted or revoked link still being probed) at no extra cost.
//
// # Data domain
//
// Tenant data, for the identical reason Share is: an access log entry
// belongs to the tenant whose share was accessed. AccessLogEntry implements
// dbkit.TenantScoped (via the embedded dbkit.TenantModel), is reached only
// through AccessLogRepository (which embeds
// dbkit.Repository[AccessLogEntry]), and its isolation is proven by
// tenancytest.AssertIsolated.
//
// # Append-only
//
// Like dbkit/audit.AuditEvent, this table is written once per row and never
// updated or deleted by this module's own code -- AccessLogRepository
// exposes no update or delete method, and OccurredAt is the row's only
// timestamp (no separate CreatedAt), since the row's creation IS the event
// it records.
//
// # ShareID is not a foreign key
//
// ShareID names a Share row in this same module, deliberately without a SQL
// FOREIGN KEY constraint -- this codebase avoids FK constraints even for
// same-module references (see go/storage's object_derivatives.object_id and
// go/pki's pki_certificates.authority_id for the identical, cross-module
// precedent this follows even though ShareID is not cross-module) so that
// dual-dialect migrations and any future soft-delete of Share stay simple.
type AccessLogEntry struct {
	// ID is an application-generated UUID.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID.
	dbkit.TenantModel

	// ShareID names the Share this access was attempted against. See the
	// type's own doc comment for why this is not a SQL foreign key.
	ShareID string `gorm:"column:share_id;size:36;not null"`

	// OccurredAt is when the access was attempted, stamped by the service's
	// own clock (never database NOW(), per the dual-dialect rules) --
	// deliberately the row's only timestamp; see the type's own doc
	// comment.
	OccurredAt time.Time `gorm:"column:occurred_at;not null"`

	// IP is the viewer's address as the caller of Service.Access observed
	// it. Free-form: this module neither parses nor validates it, since
	// how a caller learns the address (a direct connection, a
	// caller-trusted X-Forwarded-For) is an HTTP-layer decision this round
	// does not make.
	IP string `gorm:"column:ip;size:64;not null;default:''"`

	// UserAgent is the viewer's User-Agent header value, as given.
	UserAgent string `gorm:"column:user_agent;size:512;not null;default:''"`

	// Referrer is the viewer's Referer header value, as given. Spelled
	// correctly here despite the HTTP header's own historic misspelling.
	Referrer string `gorm:"column:referrer;size:512;not null;default:''"`

	// Outcome is one of the AccessOutcome constants.
	Outcome string `gorm:"column:outcome;size:16;not null"`
}

// TableName names the sharing_access_log table.
func (AccessLogEntry) TableName() string { return tableAccessLog }

// compile-time check that AccessLogEntry satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = AccessLogEntry{}
