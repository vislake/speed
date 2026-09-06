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
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()

	share, err := s.shares.byTokenHash(ctx, hashShareToken(token))
	if err != nil {
		// A store failure is not a refusal reason: log it and surface
		// the internal error, never the outward 404 a genuine refusal
		// answers with (see the doc comment above).
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

	var (
		granted   bool
		result    = share
		recordErr error
	)
	if passwordOK {
		// The granted log row travels with the view recording, committed in
		// the SAME guarded transaction when the guard wins (recordView's
		// own doc comment) -- so a granted access whose log row cannot be
		// written rolls the count back with it instead of leaving the share
		// exhausted by an access that failed. The entry is only ever
		// inserted by the attempt that actually records the view; every
		// other outcome below writes its own denied row instead.
		pending := s.accessLogEntry(tenant, share.ID, AccessOutcomeGranted, p)
		result, granted, recordErr = s.recordView(ctx, share, now, pending)
	}

	// Exactly one log row and one event per call, on every path below --
	// a recordView store failure included: its attempt is recorded as
	// denied (the only outcome vocabulary the log has for an access whose
	// decision could not be determined) and announced on the bus, and the
	// failure itself is what Access returns afterwards. A granted outcome
	// needs no further write here: its row already committed inside
	// recordView's transaction.
	var logErr error
	if !granted {
		logErr = s.writeAccessLog(ctx, s.accessLogEntry(tenant, share.ID, AccessOutcomeDenied, p))
	}
	if pubErr := s.publish(ctx, pkgcore.Event{
		Type:     EventShareAccessed,
		TenantID: tenant,
		Payload:  ShareAccessedPayload{ShareID: share.ID, Granted: granted, IP: p.IP, Referrer: p.Referrer},
	}); pubErr != nil {
		observability.FromContext(ctx).Warn("share-accessed event publish failed", "share_id", share.ID, "error", pubErr)
	}

	if recordErr != nil {
		return nil, recordErr
	}
	if logErr != nil {
		return nil, logErr
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
// full), then re-enter the ordinary, unchanged Service.Access with that
// tenant attached to ctx via pkgcore.WithTenant -- every one of Access's
// own guarantees (rule 3's immediate revocation, rule 4's access logging,
// rule 5's outward-identical answers, the constant-time password check)
// therefore holds for an anonymous caller exactly as they already hold for
// an authenticated one, because this method does not reimplement any of
// them.
//
// Before Access is ever reached, this method checks the caller's rate-limit
// budget (ratelimit.go's checkAccessRateLimit, keyed on p.IP and the
// hashed token, ErrRateLimited on denial) and resolves the tenant. An
// unrecognized token hash at this stage answers ErrNotAccessible
// immediately, without ever calling Access -- there is no tenant to attach
// and nothing downstream could do with one anyway.
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
	if err := s.checkAccessRateLimit(ctx, p.IP, hashShareToken(token)); err != nil {
		return nil, err
	}

	tenant, err := s.shares.tenantForTokenHash(ctx, hashShareToken(token))
	if err != nil {
		// A store failure is not "no such token": log it and surface
		// the internal error, never the outward 404 a genuine refusal
		// answers with -- the same classification Access's own
		// byTokenHash error path applies.
		if !hasCode(err, ErrNotAccessible.Code) {
			observability.FromContext(ctx).Error("sharing tenant-for-token lookup failed", "error", err)
			return nil, err
		}
		return nil, ErrNotAccessible
	}
	return s.Access(pkgcore.WithTenant(ctx, tenant), token, p)
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
// still has its view for a genuine retry). The caller's grantedEntry is
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
		if !current.isLive(now) {
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
