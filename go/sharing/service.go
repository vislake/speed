package sharing

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"
)

// defaultShareExpiry is the expiry rule 2
// (docs/internal/07-platform-services.md's "default expiry" rule) names
// outright for the UNSPECIFIED default: 30 days, used whenever a caller
// leaves CreateParams.ExpiresAt nil and no TenantConfigReader is wired, or
// the wired one reports the tenant has configured none. It is also this
// module's ConfigDefaultExpiry config item's own declared default
// (module.go), so a host that never touches the config item and never wires
// a TenantConfigReader still gets the documented 30-day behavior.
//
// This constant is deliberately NOT the explicit-expiry ceiling -- that is
// MaxExplicitShareLifetime's role, a separate policy value with a separate
// name (see its own doc comment for why the two roles must not share one
// constant). For a tenant that has configured no default of its own the two
// happen to coincide at 30 days -- which is honest rather than conflated:
// such a tenant's admitted envelope IS the module default, so a caller may
// not demand more explicitly than the module would grant without a request.
const defaultShareExpiry = 30 * 24 * time.Hour

// MaxExplicitShareLifetime is the module's ceiling on a caller-supplied
// EXPLICIT ExpiresAt -- the "never-expiring in disguise" arm of rule 2
// (docs/internal/07-platform-services.md's "default expiry" rule): an
// explicit expiresAt of 9999-12-31 must not smuggle a never-expiring link
// past CreateParams.Forever's refusal, so resolveExpiry refuses an explicit
// ExpiresAt further out than this with ErrExpiryOutOfRange.
//
// For a tenant that has configured a default longer than this (through the
// TenantConfigReader seam / the ConfigDefaultExpiry item) the operative
// ceiling is RAISED to that configured default: the module already admits
// shares of the tenant's own default length on the nil-ExpiresAt path
// (pinned by TestService_Create_TenantConfiguredDefaultBeyondCeiling_StillHonored),
// so refusing an explicit request within the tenant's own admitted envelope
// would be the same-policy contradiction of accepting 90 days by omission
// while refusing 45 by explicitness. The ceiling is never applied to the
// tenant-configured default itself -- that value is the host's own policy
// through a documented seam, not a caller's end-run around the ceiling --
// and never lowered by it: MaxExplicitShareLifetime remains the floor of
// the operative ceiling, so the module's own bounded-lifetime guarantee
// survives a tenant whose configured default is shorter than it.
//
// Exported so a consumer that mints shares with an explicit ExpiresAt of
// its own -- go/compliance's export delivery is the one real one -- can
// clamp its windows against the same value resolveExpiry enforces
// (go/compliance/export.go's exportDeliveryExpiry clamps to this ceiling),
// instead of mirroring a private constant that could drift.
const MaxExplicitShareLifetime = 30 * 24 * time.Hour

// maxRecordViewAttempts bounds Service.Access's retry loop around a lost
// compare-and-swap race in ShareRepository.tryRecordView (see recordView's
// own doc comment). It is not a correctness parameter -- any bound greater
// than the number of goroutines that could plausibly race on one share
// within one process is enough -- so a small, fixed constant is enough
// rather than a config item.
const maxRecordViewAttempts = 8

// viewReservationTimeout is how long a MaxViews-limited share's single
// in-flight view reservation (Share.ViewsReserved) may stand before it is
// presumed interrupted -- the serve that took it gone without resolving
// it -- and is converged by the next access or the expiry sweep (see
// ShareRepository.tryReserveView's takeover clause, tryRecordView's
// stale-or-free clause, and Service.Sweep's reservation arm). It is the
// reservation half of this module's "a view is spent on delivery, never on
// authorization" discipline (AGENTS.md's "Serving an access" section): a
// serve resolves its own reservation the moment the delivery succeeds
// (confirmAccessView) or fails (refundAccessView), so a reservation that
// outlives this timeout is by construction one whose serve died without
// resolving it -- the module's standing model for "interrupted", exactly
// as go/storage treats an uploading row whose upload window closed, and
// go/billing treats a pending reservation its caller never resolves.
//
// The timeout must be far larger than any legitimate serve: a genuine
// delivery (the resolver open plus the streamed response) takes seconds
// for ordinary content and minutes for a large bundle over a slow link,
// and a reservation older than the timeout may be taken over by a newer
// fetch -- the accepted residual of any timeout-based convergence, recorded
// in AGENTS.md's Known limitations (a serve so slow it outlives the
// timeout and a second fetch arriving exactly then can both deliver, with
// the ceiling still enforced at confirm time -- and, symmetrically, a
// fully delivered serve whose confirm could not be recorded is held spent
// only while its reservation stands, a persistent store outage outlasting
// the timeout being the one shape that can still reopen it). 30 minutes
// clears any realistic serve by an order of magnitude while keeping a
// crashed serve's dead reservation from squatting on the share's last view
// for long.
const viewReservationTimeout = 30 * time.Minute

// TenantConfigReader is the structurally-typed seam Service reads a
// tenant's configured default share expiry through -- the same
// no-import-edge shape go/org's FeatureGate and go/org's Scope use to reach
// go/config-shaped or go/org-shaped behavior without an import in either
// direction. A future host's adapter over *config.Service satisfies this
// structurally; see AGENTS.md's "Tenant-configured default expiry" section
// for why sharing declares the config item (module.go's
// ConfigDefaultExpiry) but does not itself depend on go/config, and for the
// honest statement that no host wires a TenantConfigReader yet.
type TenantConfigReader interface {
	// ShareDefaultExpiry returns tenant's configured default share expiry
	// duration. ok is false when the tenant has configured none -- Service
	// then falls back to defaultShareExpiry, exactly as if no
	// TenantConfigReader had been wired at all. err is a genuine read
	// failure: Create refuses rather than guessing at a default.
	ShareDefaultExpiry(ctx context.Context, tenant pkgcore.TenantID) (d time.Duration, ok bool, err error)
}

// CreateParams is what a caller passes to Service.Create.
type CreateParams struct {
	// ResourceRef is the opaque reference to the resource being shared.
	// Required; see Share.ResourceRef's own doc comment.
	ResourceRef string

	// ExpiresAt is the caller's own requested expiry. Nil resolves to the
	// tenant's configured default (TenantConfigReader), falling back to
	// defaultShareExpiry when none is configured or none is wired.
	ExpiresAt *time.Time

	// Forever requests a share that never expires. Always refused with
	// ErrExpiryRequired -- rule 2 (docs/internal/07-platform-services.md's
	// "never-expiring links are not allowed" rule) is explicit that this is
	// the most common source of data leaks and must never be silently
	// allowed. The field
	// exists, rather than simply having no way to ask, so a caller's
	// deliberate attempt to bypass the default fails loudly and
	// specifically instead of being reinterpreted as "use the default".
	Forever bool

	// MaxViews, when non-nil, caps the number of granted accesses. Must be
	// positive when given; nil means unlimited views.
	MaxViews *int

	// Password, when non-nil and non-empty, sets an additional access
	// password. Never stored as given -- see Share.PasswordHash's own doc
	// comment.
	Password *string

	// Sensitive marks the shared resource as carrying sensitive personal
	// information -- rule 4 (docs/internal/07-platform-services.md's
	// "sensitive resource sharing needs confirmation" rule). A
	// caller-supplied flag, not a computed
	// classification: this module deliberately builds no generic
	// sensitivity-classification system, per the round's own scope
	// boundary (AGENTS.md).
	Sensitive bool
}

// CreateResult is Service.Create's return value: the persisted Share row,
// and the raw bearer Token, returned exactly once and never obtainable
// again -- mirroring org.InviteResult's identical shape for its own
// once-only token.
type CreateResult struct {
	Share *Share
	Token string
}

// AccessParams is what a caller passes to Service.Access: the presented
// password (if the share requires one) and the request metadata the access
// log records.
type AccessParams struct {
	// Password is the credential the viewer presented, if any. Compared
	// against Share.PasswordHash only when the share has one; ignored
	// otherwise.
	Password *string

	// IP, UserAgent and Referrer are recorded on the resulting
	// AccessLogEntry as given -- see that type's own doc comment for why
	// this module neither parses nor validates them.
	IP        string
	UserAgent string
	Referrer  string
}

// Service is sharing's public entry point: Create mints a share link,
// Access resolves a bearer token into the share it names (or refuses),
// Revoke withdraws one immediately, and Get/ListAccessLog serve a resource
// owner's own view of a share and who has viewed it.
//
// It is built by NewModule and inert until Module.Register calls attach:
// before then, Create and Revoke still work (they touch no host seam), but
// Create's sensitive-resource audit emission and Access's domain-event
// publish both fail closed with a logged warning rather than a panic.
type Service struct {
	shares     *ShareRepository
	accessLogs *AccessLogRepository

	// cfg is the optional live reader of a tenant's configured default
	// expiry. Nil is a legal, fully supported configuration -- see
	// TenantConfigReader's own doc comment.
	cfg TenantConfigReader

	// now, newShareID, newAccessLogID and newToken are the service's clock
	// and entropy sources, fields for the same reason org.InviteService's
	// identical fields are: a test pins them, production never has to.
	now            func() time.Time
	newShareID     func() string
	newAccessLogID func() string
	newToken       func() (token, hash string, err error)

	// host and auditActions are the registry slice this service reads at
	// call time; see events.go's attach.
	host         hostSeams
	auditActions pkgcore.AuditActionRegistrar

	// limiter overrides ratelimit.go's rateLimiter for tests that need a
	// deterministic KVStore or a fake Limiter without going through
	// Module.Register at all -- never set in production, where
	// rateLimiter always builds one over s.host.KVStore() instead.
	limiter ratelimit.Limiter
}

// NewService returns a Service whose tables live in db. cfg may be nil; see
// TenantConfigReader's own doc comment. Constructing a Service performs no
// I/O: opening and migrating db is the host's responsibility.
func NewService(db *gorm.DB, cfg TenantConfigReader) *Service {
	return &Service{
		shares:         NewShareRepository(db),
		accessLogs:     NewAccessLogRepository(db),
		cfg:            cfg,
		now:            time.Now,
		newShareID:     uuid.NewString,
		newAccessLogID: uuid.NewString,
		newToken:       newShareToken,
	}
}

// Shares returns the service's Share repository, for a host that needs the
// promoted dbkit.Repository[Share] surface directly (seeding, inspection).
func (s *Service) Shares() *ShareRepository { return s.shares }

// AccessLogs returns the service's AccessLogEntry repository.
func (s *Service) AccessLogs() *AccessLogRepository { return s.accessLogs }

// Create mints a new share link for the caller's tenant (read from ctx) and
// returns it together with the raw bearer token -- returned exactly once,
// per CreateResult's own doc comment.
//
// Create first checks the caller tenant's share-creation rate limit
// (ratelimit.go's checkCreateRateLimit, ErrRateLimited on denial) -- the
// round-2 answer to AGENTS.md's former "Create or Access has no rate
// limiting" known limitation -- before any other validation runs, so a
// tenant already over budget pays no further work for a request that was
// never going through.
//
// Every one of rule 1's, rule 2's and rule 4's checks runs here, in this
// order: ResourceRef must be non-empty (ErrResourceRefRequired); a
// never-expiring request is refused outright (ErrExpiryRequired,
// CreateParams.Forever's own doc comment) before an expiry is ever
// resolved; a nil ExpiresAt resolves through TenantConfigReader, falling
// back to defaultShareExpiry, while an explicit ExpiresAt outside rule
// 2's bounds -- not strictly in the future, or beyond the tenant's
// operative explicit-expiry ceiling (MaxExplicitShareLifetime, raised to a
// longer tenant-configured default) -- is refused with ErrExpiryOutOfRange
// (resolveExpiry's own doc comment);
// MaxViews, if given, must be positive
// (ErrInvalidMaxViews); an optional Password is hashed, never stored
// plaintext (password.go); the token is drawn fresh from crypto/rand
// (token.go) and only its hash is persisted, alongside the narrow
// shareTokenIndex row that lets Service.AccessPublic resolve this share's
// tenant from that hash alone (repository.go's createWithTokenIndex writes
// both in one transaction). Once the row is committed,
// EventShareCreated is published (best-effort; a failure is logged, never
// returned -- the share itself is already durable) and, when Sensitive is
// true, the sensitive-share audit action fires through the declarative
// audit.Emit path (emitSensitiveAudit's own doc comment explains why that
// path, not dbkit's automatic AuditBus capture, is the right mechanism
// here).
func (s *Service) Create(ctx context.Context, p CreateParams) (*CreateResult, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if rlErr := s.checkCreateRateLimit(ctx, string(tenant)); rlErr != nil {
		return nil, rlErr
	}

	ref := strings.TrimSpace(p.ResourceRef)
	if ref == "" {
		return nil, ErrResourceRefRequired
	}
	if p.Forever {
		return nil, ErrExpiryRequired
	}
	if p.MaxViews != nil && *p.MaxViews <= 0 {
		return nil, ErrInvalidMaxViews.WithParam("max_views", *p.MaxViews)
	}

	expiresAt, err := s.resolveExpiry(ctx, tenant, p.ExpiresAt)
	if err != nil {
		return nil, err
	}

	var passwordHash *string
	if p.Password != nil && *p.Password != "" {
		h, hErr := hashSharePassword(*p.Password)
		if hErr != nil {
			return nil, ErrInternal.WithCause(hErr)
		}
		passwordHash = &h
	}

	token, hash, err := s.newToken()
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	share := &Share{
		ID:           s.newShareID(),
		TenantModel:  dbkit.TenantModel{TenantID: string(tenant)},
		ResourceRef:  ref,
		TokenHash:    hash,
		ExpiresAt:    &expiresAt,
		MaxViews:     p.MaxViews,
		PasswordHash: passwordHash,
		Sensitive:    p.Sensitive,
	}
	if err := s.shares.createWithTokenIndex(ctx, share); err != nil {
		return nil, err
	}

	if pubErr := s.publish(ctx, pkgcore.Event{
		Type:     EventShareCreated,
		TenantID: tenant,
		Payload:  ShareCreatedPayload{ShareID: share.ID, ResourceRef: share.ResourceRef, Sensitive: share.Sensitive},
	}); pubErr != nil {
		observability.FromContext(ctx).Warn("share-created event publish failed", "share_id", share.ID, "error", pubErr)
	}

	if p.Sensitive {
		if auditErr := s.emitSensitiveAudit(ctx, share); auditErr != nil {
			// Per docs/internal/10-compliance-and-audit.md's rule that an
			// audit-write failure "must alert, must not be silently
			// dropped": the share itself was already committed by this
			// point, so a failure here must not turn an otherwise
			// successful create into an error for the caller -- an
			// Error-level structured log is what "alert" means at this
			// milestone's scope, mirroring the reference app's
			// recordNoteCreatedAudit's identical reasoning.
			observability.FromContext(ctx).Error("sensitive-share audit emit failed", "share_id", share.ID, "error", auditErr)
		}
	}

	return &CreateResult{Share: share, Token: token}, nil
}

// resolveExpiry turns a caller's own request (possibly nil) into a concrete
// expiry time, never returning a zero time.Time: nil resolves through cfg
// (when wired) and finally defaultShareExpiry.
//
// A non-nil request is validated before it is accepted, never stored
// unexamined: it must lie strictly in the future (a share born already
// expired is refused with ErrExpiryOutOfRange rather than created dead --
// and "exactly now" is a share whose first Access at the same instant
// could already refuse it, so the future requirement is strict), and no
// further out than the tenant's OPERATIVE explicit-expiry ceiling from now
// (explicitExpiryCeiling's own doc comment: MaxExplicitShareLifetime,
// raised to the tenant's configured default when that default is longer)
// -- so an expiresAt of 9999-12-31 cannot bypass CreateParams.Forever's
// refusal and create the effectively-never-expiring link rule 2 exists to
// forbid, while a tenant whose own policy admits 90-day shares never
// refuses a 45-day explicit request within that envelope.
func (s *Service) resolveExpiry(ctx context.Context, tenant pkgcore.TenantID, requested *time.Time) (time.Time, error) {
	if requested != nil {
		now := s.now()
		ceiling, err := s.explicitExpiryCeiling(ctx, tenant)
		if err != nil {
			return time.Time{}, err
		}
		if !requested.After(now) || requested.After(now.Add(ceiling)) {
			return time.Time{}, ErrExpiryOutOfRange
		}
		return *requested, nil
	}
	if s.cfg != nil {
		d, ok, err := s.cfg.ShareDefaultExpiry(ctx, tenant)
		if err != nil {
			return time.Time{}, ErrInternal.WithCause(err)
		}
		if ok {
			return s.now().Add(d), nil
		}
	}
	return s.now().Add(defaultShareExpiry), nil
}

// explicitExpiryCeiling returns how far from now a caller-supplied explicit
// ExpiresAt may lie for tenant: MaxExplicitShareLifetime, raised to the
// tenant's configured default when that default is longer. The tenant's
// configured default is read through the same cfg seam the nil-ExpiresAt
// path reads -- a read failure is ErrInternal, an unconfigured tenant
// (or no reader wired) falls back to MaxExplicitShareLifetime -- and is
// NEVER re-capped against the ceiling: the ceiling governs only
// caller-supplied explicit values, while the tenant-configured default is
// the host's own policy and is honored unchanged (see
// MaxExplicitShareLifetime's own doc comment for the reasoning). Because
// the ceiling is a floor that a tenant's own default can only raise,
// every tenant's operative ceiling is at least MaxExplicitShareLifetime --
// the property go/compliance's export-delivery clamp relies on when it
// caps a delivery window at this exported constant.
func (s *Service) explicitExpiryCeiling(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, error) {
	ceiling := MaxExplicitShareLifetime
	if s.cfg != nil {
		d, ok, err := s.cfg.ShareDefaultExpiry(ctx, tenant)
		if err != nil {
			return 0, ErrInternal.WithCause(err)
		}
		if ok && d > ceiling {
			ceiling = d
		}
	}
	return ceiling, nil
}

// emitSensitiveAudit records Create's sensitive-resource confirmation as an
// audit event, through go/dbkit/audit's declarative Emit path rather than
// dbkit's automatic Options.AuditBus write-capture mechanism.
//
// The declarative path is the right one here for the identical reason
// examples/reference-app/internal/notes/handler.go's recordNoteCreatedAudit
// documents: go/dbkit/AGENTS.md's "Audit trail collection" section records
// that AuditBus must never point a persister at the same database a
// dbkit.Repository[T] write's own open transaction is still writing to, or
// the write-capture plugin's synchronous, same-goroutine publish deadlocks.
// Calling audit.Emit here, AFTER s.shares.Create has already returned, means
// Create's own transaction has already committed by the time this runs.
func (s *Service) emitSensitiveAudit(ctx context.Context, share *Share) error {
	if s.host == nil {
		return errShareNoHostRegistry
	}
	bus := s.host.EventBus()
	if bus == nil {
		return errShareNoEventBus
	}
	if s.auditActions == nil {
		return errShareNoAuditActions
	}
	return audit.Emit(ctx, bus, s.auditActions, audit.Input{
		Action:   AuditActionSensitiveShareCreate,
		Resource: audit.Resource{Type: "sharing.share", ID: share.ID, DisplayName: share.ResourceRef},
		Result:   audit.Result{Success: true},
	})
}

// Access resolves token into the share it names, for the caller's tenant
// (read from ctx). See AGENTS.md's "Tenant resolution for an
// unauthenticated viewer" section for a known, unresolved gap this leaves
// for a genuinely anonymous external visitor: unlike
// org.InvitationRepository's otherwise similar byTokenHash construction,
// whose caller (org.InviteService.Accept) is already authenticated and
// already tenant-resolved before it is called, this module's actual
// intended caller holds no access token and therefore no tenant claim to
// resolve ctx's tenant from -- this round does not solve that, and the
// round that adds the HTTP surface must.
//
// Access records exactly one AccessLogEntry and publishes exactly one
// EventShareAccessed on every path below that reaches a recognized token
// -- granted or refused alike, and including the store-failure path, whose
// outcome the log records as denied because that is the only vocabulary
// the log has for "could not be determined" (model.go's AccessOutcome
// comment). An unrecognized token is the one path with no log row: there
// is no ShareID to attribute an entry to (see below).
//
// For a GRANTED access the log row is written in the SAME guarded database
// transaction as the view-count recording itself (recordView's own doc
// comment): the count can never commit in a way a failed log row cannot
// roll back, so a share whose granted access failed to log is never left
// silently exhausted -- a MaxViews=1 share a visitor's log-failed attempt
// 500s against still has its one view available for a retry. Denied rows
// (every other path) carry no share state, so they are written after the
// decision as a plain guarded write.
//
// A failure to WRITE a log row is never swallowed into a Warn. Rule 4
// (docs/internal/07-platform-services.md's "access needs no login, but
// must leave a trail" rule) makes the trail the point of the whole
// exercise, so an Access whose log row did not commit -- granted or
// refused -- returns ErrInternal rather than answering as if the access
// had been processed normally: the recordView store failures below already
// surface as internal errors, and a log-write failure is the same class of
// operational fault, one an operator must see in the error rate rather
// than discover later by auditing a trail with holes in it. The event
// publish stays best-effort (a Warn), as it always was: the event bus is
// not the durable trail.
//
// Every refusal reason -- an unrecognized token hash, a revoked share, an
// expired one, a view-exhausted one, a missing password, or a wrong one --
// answers with the identical ErrNotAccessible and nothing else, per rule 5
// (docs/internal/07-platform-services.md's "the share surface must leak
// nothing about the tenant" rule): an outside caller who cannot tell these
// apart learns nothing by probing, including by timing the response --
// every path below that would otherwise skip password verification
// entirely instead burns an equivalent argon2id check against a dummy hash
// (see burnSharePasswordCheck), so a caller cannot use response latency to
// learn that a token names a password-protected share either.
//
// Genuine store failures are the one deliberate exception to the
// outward-identical answer: when byTokenHash or recordView cannot reach
// the database at all, that is not a refusal reason an outside caller
// produced -- it is this module's own infrastructure failing, and hiding
// it behind a 404-shaped ErrNotAccessible would erase the operational
// signal (and, until this round, did so without even a log line). Those
// paths log an Error and return the internal error, exactly as the
// pre-existing recordView error path always did; rule 5 protects the
// reasons an ACCESS can be refused, never an operator's ability to see
// that the module itself is broken.
//
// A granted access is recorded -- through recordView's compare-and-swap
// retry loop for a limited share, or its single atomic increment for an
// unlimited one, its granted log row committed in the same transaction
// (recordView's own doc comment) -- before this method returns success, so
// the row Access hands back always reflects the view that was actually
// counted and logged.
func (s *Service) Access(ctx context.Context, token string, p AccessParams) (*Share, error) {
	// Access's own callers are the host's in-process code -- authenticated,
	// authorized and tenant-resolved by layers above this module before the
	// call ever lands here -- never the genuinely unauthenticated surface
	// this module's rate limits exist for, so their wrong-credential
	// attempts are deliberately not charged to the per-token wrong-guess
	// budget (chargeTokenBudget false; see accessAuthorized's doc comment
	// and ratelimit.go's checkAccessTokenWrongGuess).
	return s.accessAuthorized(ctx, token, p, false)
}

// accessAuthorized carries Access's own implementation, shared with the
// genuinely anonymous AccessPublic: authorize the attempt, then record the
// granted view immediately (for a caller of this Service-level surface the
// authorization IS the grant -- the module's own HTTP access route is the
// one caller for whom that is NOT true, which is why it uses the
// authorize-without-recording path authorizePublicAccess instead;
// settleGranted's own doc comment has the full reasoning). Access passes
// chargeTokenBudget false; AccessPublic passes true, so that only the
// anonymous surface's wrong-credential refusals pay the per-token budget
// consumed inside authorizeAttempt -- the external attacker the budget
// exists to throttle can reach only that surface, and the host's own
// in-process Access calls must not start answering ErrRateLimited where
// they never did (see Access's own doc comment for the rate limiting that
// does and does not exist on each surface).
func (s *Service) accessAuthorized(ctx context.Context, token string, p AccessParams, chargeTokenBudget bool) (*Share, error) {
	share, err := s.authorizeAttempt(ctx, token, p, chargeTokenBudget)
	if err != nil {
		// Every refusal (or store failure) is already settled by the time it
		// is returned -- one denied log row and one denied event on every
		// recognized-token refusal, exactly as this method's own doc comment
		// promises above.
		return nil, err
	}
	// Authorized: record the granted view immediately, through the guarded
	// single-winner record (settleGranted's own doc comment). For a caller
	// of this Service-level surface, the authorization IS the grant -- there
	// is no further step between the decision and the content changing hands
	// that this module can see fail. The module's own HTTP access route is
	// the one caller for whom that is NOT true: it must deliver the share's
	// content through the host's ResourceResolver first, so it uses the
	// authorize-without-recording path instead (authorizePublicAccess plus
	// settleAccessGranted/settleAccessDenied) -- see authorizeAttempt's own
	// doc comment for why a view consumed at the authorization would be a
	// MaxViews budget spent by a delivery that never happened.
	result, granted, err := s.settleGranted(ctx, share, p)
	if err != nil {
		return nil, err
	}
	if !granted {
		return nil, ErrNotAccessible
	}
	return result, nil
}

// AccessPublic is Access's genuinely unauthenticated entry point: the round-2
// answer to AGENTS.md's former "Tenant resolution for an unauthenticated
// viewer" gap. A caller here holds no tenant claim at all -- that is the
// whole point of this method existing separately from Access -- so ctx is
// not expected to carry one; AccessPublic resolves the tenant itself, from
// the token alone, before anything else runs.
//
// It does exactly two things and nothing more: resolve tenant via
// repository.go's tenantForTokenHash (the narrow, deliberately
// non-tenant-scoped lookup that method's own doc comment justifies in
// full), then re-enter Access's own implementation (accessAuthorized) with
// that tenant attached to ctx via pkgcore.WithTenant -- every one of
// Access's own guarantees (rule 3's immediate revocation, rule 4's access
// logging, rule 5's outward-identical answers, the constant-time password
// check) therefore holds for an anonymous caller exactly as they already
// hold for an authenticated one, because this method does not reimplement
// any of them.
//
// Before Access is ever reached, this method checks the anonymous surface's
// per-IP rate-limit budget (ratelimit.go's checkAccessIPLimit, checked
// unconditionally, ErrRateLimited on denial) and resolves the tenant. An
// unrecognized token hash at this stage answers ErrNotAccessible
// immediately, without ever calling Access -- there is no tenant to attach
// and nothing downstream could do with one anyway.
//
// The surface's per-token wrong-guess budget is deliberately NOT checked
// here, ahead of the password comparison: the rate-limit-timing correction
// (ratelimit.go's checkAccessTokenWrongGuess, its own doc comment) moved
// that dimension's consumption into authorizeAttempt, after a presented
// credential has been judged wrong, so a leaked-link holder who exhausts
// the budget with wrong guesses can never hold the legitimate
// password-holder's correct attempt hostage -- that attempt is never judged
// wrong, never pays, and is never refused. Because the judgment runs in the
// shared authorizeAttempt, whose other callers are the host's own
// authenticated Access calls, AccessPublic marks its re-entry with
// chargeTokenBudget true (accessAuthorized) so that only the genuinely
// unauthenticated surface's wrong guesses pay the per-token budget.
//
// The unrecognized-token refusal is deliberately CHEAP: it pays only the
// rate-limit check and the token-index lookup, never the argon2id burn the
// recognized-token refusal paths inside Access pay. Rule 5's
// constant-time equalization protects the reasons a REFUSAL of a
// recognized share can be produced (so a prober cannot learn, by latency,
// which refusal reason applied, or whether a recognized token names a
// password-protected share); a scanner spraying random tokens never
// reaches any of those paths -- its guesses fail the token-index lookup
// first -- so burning a full ~19 MiB argon2id check on every one of them
// (burnSharePasswordCheck against the dummy hash) bought nothing rule 5
// needs and handed the scanner a memory- and CPU-amplification primitive
// instead: per-token rate limits cannot bind a scanner, since every
// guessed token hashes differently, leaving the per-IP budget as the only
// cap on an attack that cost the platform 19 MiB per request. The cost of
// that decision is that an unrecognized token is now faster to refuse than
// a recognized-but-refused one -- but that timing delta was already
// documented as outside rule 5's scope (tenantForTokenHash's own doc
// comment), because it discloses only "does this token exist at all", a
// fact any valid token's own successful use already reveals to whoever
// holds it, never which of the hidden refusal reasons applied. A token
// that DOES resolve here re-enters the ordinary, unchanged Access path,
// where every recognized-token refusal still pays its argon2id check
// exactly as before.
func (s *Service) AccessPublic(ctx context.Context, token string, p AccessParams) (*Share, error) {
	tenant, err := s.accessPublicPrelude(ctx, token, p)
	if err != nil {
		return nil, err
	}
	return s.accessAuthorized(pkgcore.WithTenant(ctx, tenant), token, p, true)
}

// accessPublicPrelude runs the checks that come before a token is
// recognized at all on the genuinely unauthenticated surface: the caller's
// per-IP access rate limit (ratelimit.go's checkAccessIPLimit, unconditional
// -- an address that exhausts its own budget refuses only itself, so it
// stays up front where it can also keep the unrecognized-token path cheap)
// and the token-to-tenant
// resolution (ShareRepository.tenantForTokenHash, whose unrecognized-hash
// answer is ErrNotAccessible and whose store failures log an Error and
// surface as internal errors, never as a refusal). AccessPublic and
// authorizePublicAccess both start here, so both anonymous entry points
// refuse exactly alike.
//
// The surface's per-token wrong-guess budget deliberately has NO presence
// here: consumption moved after the credential judgment that happens later,
// inside authorizeAttempt (ratelimit.go's checkAccessTokenWrongGuess and
// AccessPublic's own doc comment have the full argument -- a budget spent
// before the password comparison would let a leaked-link holder deny the
// legitimate password-holder's correct attempt with 429s, the defect the
// after-judgment shape removes).
func (s *Service) accessPublicPrelude(ctx context.Context, token string, p AccessParams) (pkgcore.TenantID, error) {
	if err := s.checkAccessIPLimit(ctx, p.IP); err != nil {
		return "", err
	}
	tenant, err := s.shares.tenantForTokenHash(ctx, hashShareToken(token))
	if err != nil {
		// A store failure is not "no such token": log it and surface
		// the internal error, never the outward 404 a genuine refusal
		// answers with -- the same classification Access's own
		// byTokenHash error path applies.
		if !hasCode(err, ErrNotAccessible.Code) {
			observability.FromContext(ctx).Error("sharing tenant-for-token lookup failed", "error", err)
			return "", err
		}
		return "", ErrNotAccessible
	}
	return tenant, nil
}

// authorizePublicAccess is the access route's authorize-without-recording
// phase (Handler.SharingAccessShare): the genuinely unauthenticated entry
// point that runs accessPublicPrelude and then every one of Access's refusal
// checks (authorizeAttempt, with chargeTokenBudget true -- the same
// anonymous-surface per-token wrong-guess charge AccessPublic applies, for
// the same reason: this is one of the two surfaces an external attacker can
// actually reach), settling any refusal of a token that resolved to a
// share as one denied log row and one denied event exactly as AccessPublic
// does -- a refusal answered before a share is on hand (the prelude's
// per-IP 429, an unrecognized token's ErrNotAccessible) settles nothing --
// but recording NO view and NO granted row on success. The route records the view only once the
// share's content was actually delivered (settleAccessGranted) and settles
// every serve failure in between as denied (settleAccessDenied) -- see
// authorizeAttempt's own doc comment for why recording at the authorization
// instead would spend a MaxViews budget on a delivery that never happened.
func (s *Service) authorizePublicAccess(ctx context.Context, token string, p AccessParams) (*Share, error) {
	tenant, err := s.accessPublicPrelude(ctx, token, p)
	if err != nil {
		return nil, err
	}
	return s.authorizeAttempt(pkgcore.WithTenant(ctx, tenant), token, p, true)
}

// authorizeAttempt is the no-record half of an access decision: every one of
// Access's refusal checks runs here, in the identical order and with the
// identical outward answers -- the tenant-scoped token lookup (byTokenHash),
// the constant-time password check with its argon2id burns on every path a
// recognized token can take, and the share's own current liveness
// (Share.isLive: not revoked, not expired, not exhausted). A refusal settles
// the attempt before it is returned, exactly as Access's refusal paths
// always settled one: one denied log row and one EventShareAccessed with
// Granted false, with a log-row write failure surfacing as the internal
// error rule 4 demands rather than a refusal that leaves no trail. An
// unrecognized token is the one refusal with no settle at all -- there is no
// Share row to attribute an entry to (Access's own doc comment).
//
// chargeTokenBudget is the genuinely unauthenticated surface's own flag,
// passed true only by AccessPublic and authorizePublicAccess -- the two
// entries an external attacker can reach -- and false by Access's own
// in-process host callers (see Access's own doc comment for why). On a
// wrong-credential refusal of a password-protected share, a true flag
// additionally charges the share token's per-token wrong-guess budget
// (ratelimit.go's checkAccessTokenWrongGuess), settling the refusal first
// -- one denied row and one denied event, exactly as rule 4 demands of
// every recognized-token refusal -- and then answering ErrRateLimited
// instead of ErrNotAccessible once the budget is spent. Charging at this
// point, after the credential comparison judged the attempt wrong rather
// than before it ran, is what makes the budget unable to hold the
// legitimate password-holder's correct attempt hostage: that attempt is
// never judged wrong, never reaches the charge, and is never refused by it
// (the rate-limit-timing correction; checkAccessTokenWrongGuess's own doc
// comment has the full argument, the same one go/authn already applied to
// its own per-target wrong-guess dimension).
//
// That substitution -- 429 where every other refusal this method can
// produce answers ErrNotAccessible -- is itself a recognized-token-path
// disclosure in the scope of rule 5's outward-identical answers, and it is
// recorded here on the mechanism's own site rather than claimed by that
// guarantee. Only a caller presenting a token that resolves to a
// password-protected share ever reaches the charge, so only such a caller
// can provoke the 429, and it discloses exactly three facts: the token
// exists (it names a share at all), the share is password-protected, and
// the wrong-guess budget is exhausted. It discloses nothing else -- no
// tenant, no share id, and nothing about the share's liveness, since this
// branch runs before Share.isLive and a revoked, expired or
// view-exhausted share's wrong guesses charge and answer the same way.
// (The per-IP dimension answers the same code earlier, in the prelude,
// before any token is resolved, identically for a garbage token and a
// resolving one; its dimension param is what tells the two answers
// apart.) The disclosure is accepted because the budget is the protection
// against guessing: a spent budget that answered indistinguishably from a
// wrong credential could not pace the guesser it exists to slow, who
// would have no way to know further guessing is futile until the window
// recovers -- the information the 429's retry_after_seconds param exists
// to carry -- while every futile attempt would still settle one denied
// row and one denied event, rule 4's own cost, against a budget that
// cannot admit it. The outward identity of every remaining answer is
// preserved: unrecognized tokens, under-budget wrong credentials, and
// revoked, expired and view-exhausted shares all answer the identical
// ErrNotAccessible, and the legitimate password-holder is never the party
// disclosed -- a correct attempt is never judged wrong, never reaches the
// charge, and never answers the 429.
//
// On success NOTHING is recorded: no view, no granted row. Recording is the
// caller's own next step, and the two callers differ deliberately in WHEN
// that step is honest:
//
//   - Access calls settleGranted immediately. Its contract is
//     "authorization IS the grant": there is no step this module can see
//     between the decision and the content changing hands, so the view is
//     recorded at the decision.
//
//   - The HTTP access route does NOT record at the decision. Between the
//     authorization and the content actually reaching the viewer stand the
//     resolver open and the whole response stream -- both of which can fail
//     (or, for an unwired resolver, be absent entirely) -- and a view
//     consumed by an access whose content was never delivered is a MaxViews
//     budget permanently spent by a failure. The route records only after
//     the content was fully delivered (settleAccessGranted) and settles
//     every serve failure in between as denied (settleAccessDenied), so the
//     budget and the log both tell the truth: a MaxViews=1 share survives a
//     serve that fails at the resolver or dies mid-stream, for a genuine
//     retry.
func (s *Service) authorizeAttempt(ctx context.Context, token string, p AccessParams, chargeTokenBudget bool) (*Share, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	tokenHash := hashShareToken(token)

	share, err := s.shares.byTokenHash(ctx, tokenHash)
	if err != nil {
		// A store failure is not a refusal reason: log it and surface
		// the internal error, never the outward 404 a genuine refusal
		// answers with (see Access's own doc comment).
		if !hasCode(err, ErrNotAccessible.Code) {
			observability.FromContext(ctx).Error("sharing access token lookup failed", "error", err)
			return nil, err
		}
		// No row to log against -- nothing in this tenant matches the
		// token at all, so there is no ShareID to attribute a log entry
		// to. Still pay the argon2id cost a real password check would
		// pay, so an unrecognized token is not distinguishable by timing
		// from a known, password-protected share.
		burnSharePasswordCheck(p.Password)
		return nil, ErrNotAccessible
	}

	passwordOK := true
	if share.PasswordHash != nil {
		if p.Password == nil {
			// Pay the same argon2id cost a supplied guess would pay,
			// rather than returning in the time a missing-field check
			// takes.
			burnSharePasswordCheck(nil)
			passwordOK = false
		} else if ok, verr := verifySharePassword(*share.PasswordHash, *p.Password); verr != nil || !ok {
			passwordOK = false
		}
	} else {
		// No password configured at all -- still burn the check so a
		// prober cannot tell "no password required" apart from "wrong
		// password" by response latency.
		burnSharePasswordCheck(p.Password)
	}
	if !passwordOK {
		if err := s.settleDenied(ctx, tenant, share, p); err != nil {
			return nil, err
		}
		// The per-token wrong-guess budget is charged HERE, after this
		// attempt has been judged wrong -- never before the comparison,
		// and only when the attempt arrived through the genuinely
		// unauthenticated surface (see this method's own doc comment and
		// ratelimit.go's checkAccessTokenWrongGuess). A wrong guess that
		// spends the last of the window's budget is answered 429; every
		// wrong guess is still settled as one denied row first, so no
		// recognized-token refusal ever skips its trail.
		if chargeTokenBudget {
			if err := s.checkAccessTokenWrongGuess(ctx, tokenHash); err != nil {
				return nil, err
			}
		}
		return nil, ErrNotAccessible
	}

	// A share that is no longer live at decision time is refused here -- the
	// read-only twin of the write-time liveness guard recordView's own WHERE
	// clauses enforce. For Access this is a harmless pre-check (the guarded
	// record would refuse the same share a moment later); for the HTTP route
	// it is the enforcement point itself: without it, a request for an
	// exhausted, expired or revoked share would authorize, stream the whole
	// resource, and only THEN discover there was no view left to record -- a
	// MaxViews ceiling that never refused anyone.
	if !share.isLive(now) {
		if err := s.settleDenied(ctx, tenant, share, p); err != nil {
			return nil, err
		}
		return nil, ErrNotAccessible
	}
	return share, nil
}

// settleGranted records one authorized access's view: it drives the guarded
// view recording (recordView -- the compare-and-swap retry loop for a
// limited share, the atomic server-side increment for an unlimited one) with
// a granted log entry that commits in the SAME guarded transaction, then
// settles the outcome:
//
//   - The guard won: the view is counted, the granted row committed, and one
//     EventShareAccessed with Granted true is published. The returned *Share
//     reflects the counted view and granted is true.
//
//   - The guard refused -- the share was revoked, expired, or exhausted by a
//     concurrent viewer between the authorization (authorizeAttempt) and
//     this settle, which on the HTTP route means DURING the delivery. The
//     attempt is settled as denied (one denied row, one denied event) and no
//     view is consumed: a delivery can only be recorded as granted while the
//     share is still live enough to hold the view it would consume. On the
//     route the response has already been committed by then; the log row is
//     the record. granted is false and err is nil.
//
//   - The store failed (recordView's own error, or the denied settle's log
//     row failing to write on the refused branch). The attempt still leaves
//     its trail -- settleDenied's row and event are attempted before the
//     error is returned -- and the store error is what the caller surfaces,
//     exactly as Access's own store-failure path always did.
//
// ctx need not carry the share's tenant: it is derived from the share's own
// row, so the HTTP route can settle with its bare request context.
func (s *Service) settleGranted(ctx context.Context, share *Share, p AccessParams) (*Share, bool, error) {
	tenant := pkgcore.TenantID(share.GetTenantID())
	grantedCtx := pkgcore.WithTenant(ctx, tenant)
	now := s.now()
	pending := s.accessLogEntry(tenant, share.ID, AccessOutcomeGranted, p)

	result, granted, recordErr := s.recordView(grantedCtx, share, now, pending)
	if recordErr != nil {
		// The attempt still leaves its trail before the store error
		// surfaces: one denied row and one denied event. A denied-row write
		// failure here is logged inside settleDenied's own writeAccessLog;
		// the recordView store error is the one the caller sees, exactly as
		// Access's store-failure path always returned it first.
		_ = s.settleDenied(grantedCtx, tenant, share, p)
		return nil, false, recordErr
	}
	if !granted {
		if err := s.settleDenied(grantedCtx, tenant, share, p); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	s.publishAccessEvent(grantedCtx, tenant, share.ID, true, p)
	return result, true, nil
}

// settleDenied records one access attempt's denied outcome: exactly one
// denied log row -- a write failure returns the ErrInternal rule 4 demands
// rather than a refusal that leaves no trail -- and exactly one
// EventShareAccessed with Granted false, published best-effort AFTER the row
// attempt, so a log-write failure never silences the event either (the event
// bus is not the durable trail). ctx must carry the share's tenant (Access
// and authorizeAttempt already hold it; settleAccessDenied derives it from
// the share for the HTTP route); tenant is the same value, passed
// separately because the entry, the event and the caller's own accounting
// all need it and a MustTenantFromContext here would re-read what the caller
// already holds.
func (s *Service) settleDenied(ctx context.Context, tenant pkgcore.TenantID, share *Share, p AccessParams) error {
	writeErr := s.writeAccessLog(ctx, s.accessLogEntry(tenant, share.ID, AccessOutcomeDenied, p))
	s.publishAccessEvent(ctx, tenant, share.ID, false, p)
	return writeErr
}

// publishAccessEvent announces one access attempt's outcome on the bus --
// best-effort exactly as Access's own inline publish always was: a failure
// is a logged Warn, never a returned error, because the durable fact the
// event announces (the attempt's log row) was already written or attempted
// by the caller.
func (s *Service) publishAccessEvent(ctx context.Context, tenant pkgcore.TenantID, shareID string, granted bool, p AccessParams) {
	if pubErr := s.publish(ctx, pkgcore.Event{
		Type:     EventShareAccessed,
		TenantID: tenant,
		Payload:  ShareAccessedPayload{ShareID: shareID, Granted: granted, IP: p.IP, Referrer: p.Referrer},
	}); pubErr != nil {
		observability.FromContext(ctx).Warn("share-accessed event publish failed", "share_id", shareID, "error", pubErr)
	}
}

// settleAccessDenied settles an access the route authorized but never
// delivered -- the no-resolver answer, the resolver's own failure to open
// the resource, or the content stream dying partway through the response --
// as one denied log row and one denied event, consuming nothing (the share
// keeps every view it had). The share's tenant comes from its own row, so
// the route's bare request context suffices. A log-write failure returns the
// ErrInternal rule 4 demands; the route then answers that instead of the
// underlying serve failure, exactly as Access refuses rather than answering
// when its own denied row cannot be written.
func (s *Service) settleAccessDenied(ctx context.Context, share *Share, p AccessParams) error {
	tenant := pkgcore.TenantID(share.GetTenantID())
	return s.settleDenied(pkgcore.WithTenant(ctx, tenant), tenant, share, p)
}

// settleAccessGranted records the view for an access whose content was fully
// delivered: the guarded view recording and the granted log row commit in
// one transaction (settleGranted's own doc comment), exactly once per
// genuinely successful serve. A share that ceased to be live while the
// delivery was in flight is settled inside settleGranted as denied, with no
// view consumed. The response has already been committed by the time this
// runs, so a returned error (a store failure) can only be logged, never
// answered.
//
// The access route (handler.go) calls this only for an UNLIMITED share
// (MaxViews nil): a limited share's post-delivery settle is
// confirmAccessView, the confirm half of the reserve/confirm/refund shape
// that method's own doc comment describes.
func (s *Service) settleAccessGranted(ctx context.Context, share *Share, p AccessParams) error {
	_, _, err := s.settleGranted(ctx, share, p)
	return err
}

// reserveAccessView is the reserve half of the access route's
// reserve/confirm/refund shape (Handler.SharingAccessShare): it takes a
// MaxViews-limited share's single in-flight view reservation BEFORE any
// delivery can begin, so the share's ceiling already accounts for the
// serve in flight. A MaxViews=1 share whose one serve is being delivered
// right now is refused by this call -- the identical outward answer a
// share that had simply exhausted its views answers with -- instead of
// being authorized, delivered in full, and only then losing the settlement
// race the pre-ee20d37 flow lost at the front of the request and the
// post-ee20d37 flow can still lose when the post-delivery settle write
// fails (AGENTS.md's "Serving an access" section has the full reasoning).
//
// An unlimited share (MaxViews nil) is returned from unchanged -- no
// reservation, and no way to be refused for being in use.
//
// The refusal paths settle the attempt as one denied log row and one
// denied event before they return, exactly as authorizeAttempt settles its
// own refusals: a lost reservation means the share was revoked, expired,
// exhausted or taken by a concurrent serve between the authorization and
// this write (all refused identically per rule 5), and a store failure on
// the reservation itself attempts the same denied settle before surfacing
// the internal error -- nothing has been delivered either way, so the
// caller's answer is the whole story.
func (s *Service) reserveAccessView(ctx context.Context, share *Share, p AccessParams) (*Share, error) {
	if share.MaxViews == nil {
		return share, nil
	}
	tenant := pkgcore.TenantID(share.GetTenantID())
	grantedCtx := pkgcore.WithTenant(ctx, tenant)
	now := s.now()
	won, err := s.shares.tryReserveView(grantedCtx, share, now)
	if err != nil {
		_ = s.settleDenied(grantedCtx, tenant, share, p)
		return nil, err
	}
	if !won {
		if deniedErr := s.settleDenied(grantedCtx, tenant, share, p); deniedErr != nil {
			return nil, deniedErr
		}
		return nil, ErrNotAccessible
	}
	updated := *share
	updated.ViewsReserved = 1
	updated.ViewsReservedAt = &now
	return &updated, nil
}

// refundAccessView releases the share's in-flight view reservation after a
// serve failed before its content was delivered -- the no-resolver answer,
// the resolver's own failure, a stream dying partway -- so the share is
// left exactly as it was: the reservation is returned and no view is spent,
// and a later fetch of a MaxViews=1 share can still succeed (the
// delivery-failure half of ee20d37's direction, preserved under the
// reservation shape). It is the route's partner to settleAccessDenied,
// which logs the same failed serve as denied. Idempotent under the guarded
// write: a reservation already resolved (or cleared by a revoke) makes the
// refund affect zero rows and report success. Only a MaxViews-limited share
// can hold a reservation; an unlimited share is returned from unchanged. A
// returned error is a store failure -- the route logs it, and the stale
// reservation is left for the convergence owners (viewReservationTimeout's
// own doc comment) rather than for a refund that cannot land.
func (s *Service) refundAccessView(ctx context.Context, share *Share) error {
	if share.MaxViews == nil {
		return nil
	}
	tenant := pkgcore.TenantID(share.GetTenantID())
	grantedCtx := pkgcore.WithTenant(ctx, tenant)
	_, err := s.shares.tryRefundView(grantedCtx, share.ID, s.now())
	return err
}

// confirmAccessView settles a MaxViews-limited serve whose content was FULLY
// delivered: the reserved view is confirmed into a spent view -- one guarded
// write increments view_count, clears the reservation and commits the
// granted log row in the same transaction (tryConfirmView's own doc
// comment) -- and one EventShareAccessed with Granted true is published.
// It is the reserve/confirm/refund shape's confirm-after-success arm, and
// the two directions it must hold are exactly the two this module's
// "a view is spent on delivery, never on authorization" discipline
// guarantees:
//
//   - Bytes not delivered never spend the view: a serve that fails is
//     refunded (refundAccessView), never confirmed -- this method only runs
//     after io.Copy returned without error.
//
//   - Bytes delivered are never given away unspent: this method never
//     refunds. If the confirm write itself fails (a store failure after the
//     response was committed), the reservation is NOT released -- the view
//     stays held in use, and every subsequent fetch is refused while it
//     stands, exactly as go/billing's Confirm-after-success semantics leave
//     a reservation resolved only by its own confirm, never by a refund the
//     delivered work did not earn (see go/billing's CreditService.Confirm/
//     Refund, whose already-resolved refusal is this shape's billing-side
//     twin). A delivered-but-unconfirmable serve is settled as a denied log
//     row (best-effort -- the attempt's trail), the error is logged, and
//     the held reservation is the share's own record that the view is
//     spent; only the reservation-timeout convergence
//     (viewReservationTimeout's doc comment) can later release it, the
//     recorded residual of any timeout-based convergence.
//
// A share that ceased to be live while the delivery was in flight (revoked
// or expired mid-stream) refuses the confirm at its WHERE clause; the serve
// is then settled as denied and the reservation released -- the same
// settle-time liveness semantics ee20d37 established for its own
// post-delivery record. The response has already been committed by the time
// this runs, so a returned error can only be logged, never answered.
func (s *Service) confirmAccessView(ctx context.Context, share *Share, p AccessParams) error {
	tenant := pkgcore.TenantID(share.GetTenantID())
	grantedCtx := pkgcore.WithTenant(ctx, tenant)
	now := s.now()
	pending := s.accessLogEntry(tenant, share.ID, AccessOutcomeGranted, p)

	won, recordErr := s.shares.tryConfirmView(grantedCtx, share, now, pending)
	if recordErr != nil {
		// The delivery succeeded but the spend could not be recorded. The
		// reservation is deliberately NOT refunded (see this method's own
		// doc comment): the best-effort denied row is the attempt's trail,
		// and the reservation is what keeps the delivered view spent.
		_ = s.settleDenied(grantedCtx, tenant, share, p)
		return recordErr
	}
	if !won {
		// The reservation was lost to a concurrent revoke or expiry (or, in
		// the recorded stale-takeover corner, to a newer serve): release the
		// slot if it is still standing and settle the delivered serve as
		// denied, exactly as settleGranted settles a delivery whose share
		// ceased to be live before its view could be recorded.
		if refundErr := s.refundAccessView(grantedCtx, share); refundErr != nil {
			observability.FromContext(ctx).Warn("share reservation could not be refunded after a lost confirm",
				"share_id", share.ID, "error", refundErr)
		}
		if err := s.settleDenied(grantedCtx, tenant, share, p); err != nil {
			return err
		}
		return nil
	}
	s.publishAccessEvent(grantedCtx, tenant, share.ID, true, p)
	return nil
}

// recordView drives the share's view-recording guard to a definitive
// outcome: granted (the view was counted -- and grantedEntry, the access's
// granted log row, was committed alongside it in the SAME guarded
// transaction -- and the returned Share reflects the view) or refused (the
// share was not live at the moment this call observed it, in which case
// grantedEntry is never written and the caller records a denied row of its
// own).
//
// The count and the granted log row sharing one transaction is what makes
// Access's log-write failure semantics honest: if the log row cannot be
// written, the whole transaction rolls back, so the count did not commit in
// a way the failed log cannot undo -- a log-failed attempt can never
// permanently exhaust a MaxViews=1 share (the log-write failure surfaces as
// the attempt's store error and Access returns ErrInternal, and the share
// still has its view for a genuine retry). That promise has one boundary,
// inherited from dbkit's commit-time-failure cell (WithTenantSession's own
// doc comment): when the COMMIT itself is reported failed after the count
// and the row actually stuck, the failure is not an in-transaction log
// failure, and the retried attempt can neither roll back what it cannot
// see nor double-spend what it can. The repository answers that cell by
// recognizing its own committed residue -- a retried attempt that finds
// its own granted row already durably recorded reports won == true rather
// than won == false (tryRecordView's own doc comment) -- so this loop
// serves the access whose view its committed attempt already consumed
// instead of refusing it and burning the view with no mechanism to reclaim
// it. The caller's grantedEntry is
// ONLY inserted by the specific attempt whose guarded UPDATE actually won,
// so a CAS loss that hands the view to a concurrent viewer never writes a
// duplicate log row.
//
// The two shapes of share take two different paths:
//
//   - A LIMITED share (MaxViews set) goes through
//     ShareRepository.tryRecordView's compare-and-swap guard. A single CAS
//     attempt can lose to ordinary concurrency -- two viewers racing the
//     same share both read the same ViewCount and only one UPDATE can win
//     -- which is NOT the same thing as the share genuinely being
//     exhausted. The loop re-reads the row on a lost race and retries
//     against its latest state, up to maxRecordViewAttempts times, so a
//     benign concurrency loss is never misreported as "not accessible";
//     only a share that is genuinely no longer live (isLive returns false
//     on the freshly re-read row) short-circuits the loop early as a real
//     refusal. A retry bound is the right shape here because the CAS is
//     arbitrating something real -- the last slots of a ceiling -- and
//     sustained contention that exhausts the bound is a genuine refusal.
//
//   - An UNLIMITED share (MaxViews nil -- the common public-link shape)
//     skips the CAS path entirely: there is no ceiling for a
//     compare-and-swap to arbitrate, so a CAS loss would carry no
//     information -- and a viewer of an unlimited share must never be
//     refused just because it lost too many races against other viewers
//     who each happened to commit between its read and its write (the
//     bounded retry loop above would otherwise starve some legitimate
//     viewer into a false 404 once it lost maxRecordViewAttempts times in
//     a row under concurrency). Instead the view is recorded by one atomic
//     server-side increment (ShareRepository.tryIncrementView) whose own
//     WHERE clause evaluates liveness at write time: every viewer that
//     observed a live row wins exactly once, and a lost update can only
//     mean a concurrent revocation or expiry, which is a genuine refusal.
func (s *Service) recordView(ctx context.Context, share *Share, now time.Time, grantedEntry *AccessLogEntry) (*Share, bool, error) {
	if share.MaxViews == nil {
		if !share.isLive(now) {
			return share, false, nil
		}
		won, err := s.shares.tryIncrementView(ctx, share, now, grantedEntry)
		if err != nil {
			return nil, false, err
		}
		if won {
			updated := *share
			updated.ViewCount++
			return &updated, true, nil
		}
		// Lost the increment to a concurrent revocation or expiry -- re-read
		// and refuse against the row's honest current state.
		latest, err := s.shares.byTokenHash(ctx, share.TokenHash)
		if err != nil {
			return nil, false, err
		}
		return latest, false, nil
	}

	current := share
	for attempt := 0; attempt < maxRecordViewAttempts; attempt++ {
		// A share that is no longer live at this moment is refused here --
		// and so is one whose last view a concurrent serve of the access
		// route currently holds in flight (a live reservation,
		// Share.hasLiveReservation): the CAS below would refuse it anyway
		// (tryRecordView's own reservation clause), and refusing now,
		// before the attempt, is what keeps a serve that is genuinely
		// mid-delivery from costing this loop its whole retry budget. A
		// STALE reservation is not refused -- the CAS's own stale-or-free
		// clause (tryRecordView) converges it, which is the "converged by
		// the next access" half of an interrupted reservation's lifecycle.
		if !current.isLive(now) || current.hasLiveReservation(now) {
			return current, false, nil
		}
		won, err := s.shares.tryRecordView(ctx, current, now, grantedEntry)
		if err != nil {
			return nil, false, err
		}
		if won {
			updated := *current
			updated.ViewCount++
			return &updated, true, nil
		}
		latest, err := s.shares.byTokenHash(ctx, current.TokenHash)
		if err != nil {
			return nil, false, err
		}
		current = latest
	}
	// Exhausted every retry against genuine, sustained contention -- treat
	// as a refusal rather than looping forever; a real deployment racing
	// this many concurrent viewers on one limited share within one CAS
	// window is not a case this round optimizes for.
	return current, false, nil
}

// accessLogEntry builds one AccessLogEntry for shareID with the given
// outcome, ready to be persisted -- the row Service.Access commits
// alongside a granted view (recordView), or writes on its own for every
// denied outcome. The caller-supplied metadata fields
// (AccessParams.IP/UserAgent/Referrer) are cut to their column bounds
// HERE, before the row is built -- see model.go's column-bound constants
// and truncateAccessLogValue for why the cut happens in Go rather than
// being left to the database.
func (s *Service) accessLogEntry(tenant pkgcore.TenantID, shareID, outcome string, p AccessParams) *AccessLogEntry {
	return &AccessLogEntry{
		ID:          s.newAccessLogID(),
		TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
		ShareID:     shareID,
		OccurredAt:  s.now(),
		IP:          truncateAccessLogValue(p.IP, accessLogIPLen),
		UserAgent:   truncateAccessLogValue(p.UserAgent, accessLogUserAgentLen),
		Referrer:    truncateAccessLogValue(p.Referrer, accessLogReferrerLen),
		Outcome:     outcome,
	}
}

// writeAccessLog persists one already-built access log row and returns an
// internal error when the row could not be written -- never swallowed:
// Access treats a log-write failure as a failed call (see Access's own doc
// comment for the rule-4 reasoning), since an access that leaves no trail
// is exactly the failure mode rule 4 exists to forbid.
//
// The write runs through AccessLogRepository.createWithRetry -- the same
// withTxRetry envelope this module's guarded writes run under -- so a
// transient, contention-only database failure (SQLITE_BUSY, a PostgreSQL
// serialization conflict) retries the insert up to txRetryBudget times
// rather than failing a denied access over a momentary conflict. A denied
// row carries no share state, so it deliberately does NOT join
// ShareRepository's own writeMu ordering: only the granted row -- which
// travels inside the view-recording transaction (recordView's doc comment)
// -- needs to be ordered against the module's other writers.
func (s *Service) writeAccessLog(ctx context.Context, entry *AccessLogEntry) error {
	if err := s.accessLogs.createWithRetry(ctx, entry); err != nil {
		observability.FromContext(ctx).Error("sharing access log write failed", "share_id", entry.ShareID, "error", err)
		return ErrInternal.WithCause(err)
	}
	return nil
}

// truncateAccessLogValue cuts a caller-supplied access-log field value to
// maxRunes runes and guarantees valid UTF-8, the two properties the column
// it is about to be stored in requires on PostgreSQL: VARCHAR(n) counts
// CHARACTERS, so a cut by rune (not byte) is what actually fits, and a
// UTF-8-encoded database refuses a value carrying invalid byte sequences
// outright (SQL error 22021). A caller-controlled User-Agent or Referer
// can carry either hazard -- HTTP headers are free-form bytes -- and either
// one failing the INSERT would make the whole access leave no trail (the
// exact failure rule 4 exists to forbid), which is why the value is made
// safe HERE, at the write boundary, rather than relying on the database to
// reject it after the fact. Invalid bytes are rendered as the Unicode
// replacement character, never silently dropped (dropping them could
// concatenate two arbitrary byte runs into a different valid value); the
// cut happens after that sanitization, on the resulting runes.
func truncateAccessLogValue(v string, maxRunes int) string {
	if len(v) <= maxRunes && utf8.ValidString(v) {
		return v
	}
	runes := []rune(strings.ToValidUTF8(v, "\uFFFD"))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

// Revoke withdraws share immediately: the very next Access call against it
// refuses with ErrNotAccessible, per rule 3
// (docs/internal/07-platform-services.md's "revocation takes effect
// immediately" rule) -- there is no
// cache anywhere on this module's own side to invalidate, so this method
// need do nothing beyond persisting RevokedAt. Revoking an already-revoked
// share is idempotent and reports success.
//
// The RevokedAt write is a narrow, guarded UPDATE (ShareRepository's
// markRevoked), never a whole-row write-back of the Share read above:
// between that read and the write, a concurrent granted access can commit
// its view-count increment through recordView's compare-and-swap, and a
// full-row Save of the stale read would silently roll that increment back
// -- a granted view vanishing from the owner's count. The guarded update
// touches only revoked_at (and the auto timestamps), so a concurrent view
// count survives a revoke. EventShareRevoked is published exactly once per
// actual transition -- only by the caller whose guarded update was the one
// that set RevokedAt -- so two racing Revoke calls never announce one
// revocation twice; a loser (or a sequential second revoke, caught by the
// pre-read below) publishes nothing, exactly as the pre-existing
// already-revoked early return always did.
func (s *Service) Revoke(ctx context.Context, shareID string) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	share, err := s.shares.FindByID(ctx, shareID)
	if err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return ErrShareNotFound.WithParam("id", shareID)
		}
		return err
	}
	if share.RevokedAt != nil {
		return nil
	}
	now := s.now()
	won, err := s.shares.markRevoked(ctx, shareID, now)
	if err != nil {
		return err
	}
	if !won {
		// Lost the guarded update to a concurrent revoke -- that caller
		// published; idempotent success here, no second event.
		return nil
	}
	if pubErr := s.publish(ctx, pkgcore.Event{
		Type:     EventShareRevoked,
		TenantID: tenant,
		Payload:  ShareRevokedPayload{ShareID: share.ID},
	}); pubErr != nil {
		observability.FromContext(ctx).Warn("share-revoked event publish failed", "share_id", share.ID, "error", pubErr)
	}
	return nil
}

// Get returns the caller tenant's share by id, translating dbkit's
// not-found into ErrShareNotFound -- an owner-facing lookup, safe to
// disclose non-existence for, unlike Access's ErrNotAccessible.
func (s *Service) Get(ctx context.Context, shareID string) (*Share, error) {
	share, err := s.shares.FindByID(ctx, shareID)
	if err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return nil, ErrShareNotFound.WithParam("id", shareID)
		}
		return nil, err
	}
	return share, nil
}

// List returns every share of the caller's tenant (read from ctx), newest
// first -- the round-3 owner-facing HTTP surface's sharing_listShares
// operation is a thin translation of this method and nothing more.
//
// This is a deliberately minimal addition: it is not a new business rule,
// only a tenant-scoped read over the repository surface Create, Revoke, Get
// and ListAccessLog already use (repository.go's listByTenant), added
// because no existing Service method served "every share of this tenant" --
// Get and ListAccessLog both require a caller-known shareID. Unlike
// Service.Get, an entry here is never filtered by liveness: a revoked or
// expired share still belongs to its tenant, and RevokedAt on the returned
// row is exactly how a caller learns it is gone.
func (s *Service) List(ctx context.Context) ([]Share, error) {
	return s.shares.listByTenant(ctx)
}

// ListAccessLog returns every recorded access attempt against the caller
// tenant's share shareID, newest first -- the owner-facing "who viewed
// this and how many times" answer rule 4
// (docs/internal/07-platform-services.md's "access needs no login, but
// must leave a trail" rule)
// requires. It first confirms the share exists in the caller's tenant
// (ErrShareNotFound otherwise), so a caller cannot learn anything about
// another tenant's share id by probing this method either.
func (s *Service) ListAccessLog(ctx context.Context, shareID string) ([]AccessLogEntry, error) {
	if _, err := s.Get(ctx, shareID); err != nil {
		return nil, err
	}
	return s.accessLogs.listByShare(ctx, shareID)
}
