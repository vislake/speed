package sharing

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// Table names, shared between the models' TableName methods and the
// migrations' own header comments.
const (
	tableShares     = "sharing_shares"
	tableAccessLog  = "sharing_access_log"
	tableTokenIndex = "sharing_token_index"
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
	// a revoked, expired or view-exhausted share, a missing/wrong password,
	// and, on the module's HTTP access route (handler.go), every serve that
	// was authorized but never delivered: an unwired or failing resource
	// resolver, a content stream that died partway through, or a delivery
	// whose share ceased to be live before its view could be recorded --
	// deliberately without distinguishing which. Access's own
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
// Tenant data: a share link belongs to the tenant whose resource it
// exposes. Share implements dbkit.TenantScoped (via the embedded
// dbkit.TenantModel), is reached only through ShareRepository (which embeds
// dbkit.Repository[Share]), and its isolation is proven by
// tenancytest.AssertIsolated.
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
// The Go type is an optional *time.Time -- Service.Create's
// CreateParams.ExpiresAt really is optional, a caller may leave it nil and
// get the tenant's configured default. But this row's own ExpiresAt is
// NEVER actually nil once a Share has been created: every share must carry
// an expiry, and Service.Create resolves a nil request into
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
// (password.go's own doc comment records why that is an accepted
// limitation rather than an oversight).
//
// # Cross-module references
//
// ResourceRef is an opaque string this module never interprets: it is
// whatever the caller of Create supplied, typically a reference another
// module's own key scheme produces (e.g. go/storage's object id), stored
// as plain data with no foreign key -- cross-module foreign keys are
// forbidden in this codebase, and sharing does not import go/storage or
// any other resource-owning module to interpret it. Turning ResourceRef
// into actual bytes is the host's job through the ResourceResolver seam
// (resolver.go), never this module's.
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
	// that method's own doc comment for why. On the module's HTTP access
	// route (handler.go) a MaxViews-limited share's view is incremented
	// only by the confirm half of the reserve/confirm/refund shape
	// (Service.confirmAccessView, repository.go's tryConfirmView) -- never
	// by the reservation that precedes the delivery.
	ViewCount int `gorm:"column:view_count;not null;default:0"`

	// ViewsReserved is this share's single in-flight view reservation: 1
	// while the access route (handler.go) is serving one viewer of a
	// MaxViews-limited share, 0 otherwise. It exists so the route can
	// reserve the view BEFORE any bytes are delivered (service.go's
	// reserveAccessView, repository.go's tryReserveView -- see
	// viewReservationTimeout's doc comment for the full protocol): the
	// share's ceiling already accounts for the serve in flight -- a
	// concurrent second fetch is refused up front instead of being
	// delivered and then losing a settlement race -- and the reservation is
	// resolved after the delivery (confirmed into a spent view) or after a
	// failed one (refunded). Deliberately a count-shaped column that only
	// ever holds 0 or 1: a MaxViews-limited share serves one viewer at a
	// time -- see service.go's viewReservationTimeout
	// doc comment for the per-share single-flight reasoning and the recorded
	// alternative it rejects. Only ever set on a row whose MaxViews is
	// non-nil: an unlimited share has no finite allowance for a reservation
	// to draw against, so its serves are never reserved (and never refused
	// for being in use).
	ViewsReserved int `gorm:"column:views_reserved;not null;default:0"`

	// ViewsReservedAt is when the in-flight reservation (ViewsReserved)
	// began -- the age that lets a stale reservation be told apart from a
	// live serve (viewReservationTimeout, service.go). Written together
	// with ViewsReserved by every guarded write, never independently: a row
	// with ViewsReserved = 1 and ViewsReservedAt = NULL is an invariant
	// violation, treated defensively as a live reservation (never
	// auto-converged).
	ViewsReservedAt *time.Time `gorm:"column:views_reserved_at"`

	// PasswordHash, when non-nil, is the argon2id PHC digest of an
	// additional access password a viewer must present. Never plaintext --
	// see the type's own doc comment.
	PasswordHash *string `gorm:"column:password_hash;size:255"`

	// Sensitive records whether the caller of Create declared the shared
	// resource as carrying sensitive personal information. A true value is
	// what makes Create fire the sensitive-share audit action (module.go's
	// AuditActionSensitiveShareCreate). Kept on the row itself (rather than
	// only in the audit trail) so an owner-facing
	// listing can flag it without a second lookup.
	Sensitive bool `gorm:"column:sensitive;not null;default:false"`

	// RevokedAt is nil for a live share, and the moment Service.Revoke
	// withdrew it otherwise. Once set, Service.Access refuses every access
	// on the very next call -- there is no cache to invalidate on this
	// module's own side, and handler.go's route answers every response with
	// Cache-Control: no-store so no cached copy can outlive the revocation.
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
//
// isLive deliberately does NOT consider the in-flight reservation
// (ViewsReserved): the reservation is enforced by the guarded writes that
// draw against it (repository.go's tryReserveView and tryRecordView), each
// of which re-evaluates the row's real state at write time, because the
// reservation's one job is to arbitrate exactly that write-time race.
// authorizeAttempt (service.go) therefore lets a request for an in-use
// share through to the guarded reserve, which is the one place that can
// distinguish "the share is serving someone right now" (a refusal) from
// "the share's last view is free" (a grant) without a read-then-decide
// race.
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

// hasLiveReservation reports whether the share holds a view reservation
// that has not yet outlived viewReservationTimeout as of now -- i.e. a
// serve that is presumed still in flight, as opposed to a stale
// reservation a dead serve left behind (which the next access converges
// instead: repository.go's tryReserveView takeover clause and tryRecordView's
// stale-or-free clause, and cleanup.go's sweep). An unlimited share never
// holds a reservation, and a ViewsReservedAt = NULL mark (an invariant
// violation, per that field's own doc comment) is treated as live forever
// rather than ever being auto-converged.
func (s Share) hasLiveReservation(now time.Time) bool {
	if s.MaxViews == nil || s.ViewsReserved <= 0 {
		return false
	}
	if s.ViewsReservedAt == nil {
		return true
	}
	// Live strictly while younger than the timeout: a reservation exactly
	// viewReservationTimeout old is stale, the same boundary the guarded
	// writes' takeover clauses use (views_reserved_at <= now - timeout), so
	// the two predicates can never disagree about one row.
	return now.Before(s.ViewsReservedAt.Add(viewReservationTimeout))
}

// compile-time check that Share satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Share{}

// The column bounds of sharing_access_log's caller-supplied VARCHAR
// columns, declared by the migrations that create the table
// (migrations/{sqlite,postgres}/0002_create_sharing_access_log.sql) --
// THE MIGRATIONS ARE THE AUTHORITY: these constants exist so the write
// boundary (service.go's truncateAccessLogValue, called from logAccess)
// can cut caller-controlled values to the column's width in Go, and they
// must track the migration files' VARCHAR(n) declarations and the gorm
// size tags on AccessLogEntry below (both dialects, both files) -- a
// mismatch means a value this module believes it truncated is still
// rejected by one dialect. PostgreSQL enforces VARCHAR(n) at the database
// and refuses an over-long value with error 22001, failing the INSERT; the
// access would then leave no trail at all, which is why the cut
// happens here rather than being left to the database. SQLite ignores the
// bound entirely, which is why the SQLite-only unit tier cannot see an
// over-long value fail -- the truncation tests in service_test.go pin the
// Go-side cut instead.
const (
	// accessLogIPLen bounds AccessLogEntry.IP.
	accessLogIPLen = 64
	// accessLogUserAgentLen bounds AccessLogEntry.UserAgent.
	accessLogUserAgentLen = 512
	// accessLogReferrerLen bounds AccessLogEntry.Referrer.
	accessLogReferrerLen = 512
)

// AccessLogEntry is one recorded access attempt against a Share -- granted
// or denied alike: a resource owner reads this back through
// Service.ListAccessLog to answer "who viewed this and how many times",
// and
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
// updated or deleted by this module's own code: the only writes are the two
// appends of the module's access flow -- createWithRetry for a denied
// attempt, the grantedEntry insert inside ShareRepository's own
// view-recording transaction for a granted one -- and no route, service
// method or call site touches a row after that append. OccurredAt is the
// row's only timestamp (no separate CreatedAt), since the row's creation IS
// the event it records.
//
// That immutability is a convention this module maintains, not a type
// property, and the difference from dbkit/audit is deliberate.
// AccessLogRepository embeds dbkit.Repository[AccessLogEntry], so the
// base's exported Update and Delete are part of its method set -- the same
// promoted surface Service.Shares documents for Share rows (seeding,
// inspection) -- and a host holding the repository Service.AccessLogs
// returns could call them. The convention suffices because the module
// itself never does: every row is appended and then only read, so a
// mutation requires the host to actively reach past that surface, a
// deliberate violation of the contract this comment states rather than an
// accident the shape makes easy. dbkit/audit's AuditEvent deliberately
// chose the stronger form instead -- its Repository's hand-written method
// set is exactly Insert, InsertIdempotent, Get and ListByTenant, with no
// embedded base to widen, and its own migrations add rejecting triggers as
// a second backstop -- because audit rows are platform evidence a
// compliance process relies on, while an access log is tenant data whose
// append-only property matters but is not evidence.
//
// The stronger form is also the wrong form HERE, for a second, decisive
// reason: what AuditEvent's trigger pair protects is undeletability, and
// audit_events must be undeletable while this table must not be.
// audit_events is platform data whose erasure paths are closed by design
// -- no Update or Delete method, a Repository[T] that cannot even be
// instantiated against it (HardDelete, the compliance-erasure path, needs
// TenantScoped, which AuditEvent deliberately does not implement), and
// the rejecting triggers as the database-level backstop -- it is the
// durable record a compliance process reads but never erases.
// sharing_access_log, by contrast, is tenant data a compliance regime
// must be able to erase: retention sweeps and right-to-erasure reach it
// through a pkgcore.RetentionParticipant whose callbacks run
// dbkit.Repository[T].HardDelete (that interface's own doc comment
// describes the contract), and AccessLogEntry's TenantScoped shape is
// exactly what makes that path instantiable. A DELETE-rejecting trigger
// here would block precisely that path, so the table carries none --
// nothing in sharing_access_log's migrations rejects an UPDATE or
// DELETE, which is precisely why the convention this comment states is
// the whole of its immutability. The trigger pair belongs on the one
// table that must outlive every erasure regime, not on a table an
// erasure regime must be able to reach.
//
// # ShareID is not a foreign key
//
// ShareID names a Share row in this same module, deliberately without a SQL
// FOREIGN KEY constraint -- this codebase avoids FK constraints even for
// same-module references (see go/storage's object_derivatives.object_id and
// go/pki's pki_certificates.authority_id for the identical, cross-module
// precedent this follows even though ShareID is not cross-module) so that
// the dual-dialect migrations stay simple and deleting a Share row never
// drags its access log with it.
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
	// caller-trusted X-Forwarded-For) is an HTTP-layer decision made
	// outside this module.
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

// shareTokenIndex is the narrow, deliberately non-tenant-scoped row that
// resolves a bearer token's owning tenant before any tenant is known at
// all -- see repository.go's (*ShareRepository).tenantForTokenHash, the
// one method that reads it, and service.go's AccessPublic doc comment for
// the reasoning behind the resolution direction.
//
// # Data domain
//
// Platform data, NOT tenant data, and deliberately so: a genuinely
// unauthenticated visitor holds no tenant claim for dbkit's tenant-scope
// GORM plugin to filter by, and that plugin fails every tenant-scoped
// query closed when the context carries none (go/dbkit's tenant_scope.go,
// tenantScopeBeforeQuery) -- correctly, since it has no way to tell "this
// caller is allowed to look this up with no tenant" apart from an
// ordinary forgotten-tenant bug. The repository-wide rule for
// identity/platform data applies here without exception: this table
// implements no dbkit.TenantScoped, is reached through dbkit.Open()'s
// plain *gorm.DB (never dbkit.Repository[T], whose generic constraint
// requires TenantScoped, which this type must NOT implement), and its
// isolation suite is tenancytest.AssertNotTenantScoped, not
// AssertIsolated -- the identical treatment go/authn's users table,
// go/jobs's jobRecord and go/config's row already get for the same
// reason: something that must be resolvable before a tenant is known
// cannot itself be tenant-scoped.
//
// # Deliberately narrow
//
// This is NOT a general cross-tenant query capability, and carries nothing
// that would make it one: two columns, a token hash and the tenant it
// belongs to, nothing else -- no ResourceRef, no ShareID, no ViewCount, no
// password state. A row here answers exactly one question ("which tenant
// does this hash belong to") and nothing further; every other question
// about the share it names -- is it revoked, expired, view-exhausted,
// password-protected -- is still answered exclusively by the ordinary
// tenant-scoped Share row, reached only after this lookup hands back a
// tenant to attach to ctx. Compare repository.go's byTokenHash, which reads
// the full, tenant-scoped Share row and remains the only path that can ever
// grant access to a share's content.
//
// # Written alongside its Share, read only by AccessPublic
//
// (*ShareRepository).createWithTokenIndex inserts this row in the same
// database transaction as its Share, so a share is never left reachable by
// its owner (an authenticated tenant caller, via Get/Revoke/ListAccessLog)
// while being permanently unreachable by an anonymous visitor holding the
// same token -- an inconsistency a two-step, non-transactional write could
// otherwise leave behind indefinitely, since nothing else in this module
// ever repairs a missing index row. It is never updated: Revoke sets only
// Share.RevokedAt, leaving this row in place, because Service.AccessPublic
// needs it to resolve a tenant and reach the ordinary Access path even for
// a token whose share has since been revoked, expired or exhausted --
// exactly how Access is meant to answer that case (ErrNotAccessible, not a
// dead end before Access is ever reached).
type shareTokenIndex struct {
	// TokenHash is the hex-encoded SHA-256 of the share token -- the exact
	// same value Share.TokenHash stores, and the primary key here: two
	// tokens hashing to the same value is cryptographically negligible (see
	// token.go), so the constraint is cheap insurance, not a meaningfully
	// defended invariant.
	TokenHash string `gorm:"column:token_hash;primaryKey;size:64"`

	// TenantID is the plain, unenforced tenant identifier this row exists
	// to answer -- unenforced in the identical sense every other
	// platform-data table's tenant_id column is (go/jobs's jobRecord,
	// go/config's row, go/dbkit/audit's AuditEvent): a real column, never
	// filtered or populated by dbkit's tenant-scope plugin, because this
	// type implements no TenantScoped.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`
}

// TableName names the sharing_token_index table.
func (shareTokenIndex) TableName() string { return tableTokenIndex }
