package sharing

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"
)

// defaultShareExpiry is the ceiling rule 2
// (docs/internal/07-platform-services.md's "default expiry" rule) names
// outright: 30 days, used whenever a caller leaves CreateParams.ExpiresAt nil and no
// TenantConfigReader is wired, or the wired one reports the tenant has
// configured none. It is also this module's ConfigDefaultExpiry config
// item's own declared default (module.go), so a host that never touches
// the config item and never wires a TenantConfigReader still gets the
// documented 30-day behavior.
const defaultShareExpiry = 30 * 24 * time.Hour

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
// back to defaultShareExpiry; MaxViews, if given, must be positive
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
func (s *Service) resolveExpiry(ctx context.Context, tenant pkgcore.TenantID, requested *time.Time) (time.Time, error) {
	if requested != nil {
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
// EventShareAccessed regardless of outcome.
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
// A granted access is recorded through recordView's compare-and-swap retry
// loop before this method returns success, so the row Access hands back
// always reflects the view that was actually counted.
func (s *Service) Access(ctx context.Context, token string, p AccessParams) (*Share, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()

	share, err := s.shares.byTokenHash(ctx, hashShareToken(token))
	if err != nil {
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
		granted bool
		result  = share
	)
	if passwordOK {
		result, granted, err = s.recordView(ctx, share, now)
		if err != nil {
			return nil, err
		}
	}

	s.logAccess(ctx, tenant, share.ID, granted, p)
	if pubErr := s.publish(ctx, pkgcore.Event{
		Type:     EventShareAccessed,
		TenantID: tenant,
		Payload:  ShareAccessedPayload{ShareID: share.ID, Granted: granted, IP: p.IP, Referrer: p.Referrer},
	}); pubErr != nil {
		observability.FromContext(ctx).Warn("share-accessed event publish failed", "share_id", share.ID, "error", pubErr)
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
// unrecognized token hash at this stage burns one password check
// (burnSharePasswordCheck) and answers ErrNotAccessible immediately,
// without ever calling Access -- there is no tenant to attach and nothing
// downstream could do with one anyway. See tenantForTokenHash's own doc
// comment for the one timing property this collapsing does NOT hide (an
// unrecognized token is cheaper to refuse than a recognized-but-refused
// one), and why that is not a rule-5 violation.
func (s *Service) AccessPublic(ctx context.Context, token string, p AccessParams) (*Share, error) {
	if err := s.checkAccessRateLimit(ctx, p.IP, hashShareToken(token)); err != nil {
		return nil, err
	}

	tenant, err := s.shares.tenantForTokenHash(ctx, hashShareToken(token))
	if err != nil {
		burnSharePasswordCheck(p.Password)
		return nil, ErrNotAccessible
	}
	return s.Access(pkgcore.WithTenant(ctx, tenant), token, p)
}

// recordView drives ShareRepository.tryRecordView's compare-and-swap guard
// to a definitive outcome: granted (the view was counted, and the returned
// Share reflects it) or refused (the share was not live at the moment this
// call observed it).
//
// A single CAS attempt can lose to ordinary concurrency -- two viewers
// racing the same share both read the same ViewCount and only one UPDATE
// can win -- which is NOT the same thing as the share genuinely being
// exhausted. This loop re-reads the row on a lost race and retries against
// its latest state, up to maxRecordViewAttempts times, so a benign
// concurrency loss is never misreported as "not accessible"; only a share
// that is genuinely no longer live (isLive returns false on the freshly
// re-read row) short-circuits the loop early as a real refusal.
func (s *Service) recordView(ctx context.Context, share *Share, now time.Time) (*Share, bool, error) {
	current := share
	for attempt := 0; attempt < maxRecordViewAttempts; attempt++ {
		if !current.isLive(now) {
			return current, false, nil
		}
		won, err := s.shares.tryRecordView(ctx, current, now)
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
	// this many concurrent viewers on one share within one CAS window is
	// not a case this round optimizes for.
	return current, false, nil
}

// logAccess best-effort records one AccessLogEntry. A write failure is
// logged and swallowed, never returned: it must not turn an otherwise
// correctly resolved Access call (granted or refused) into a different
// answer for the caller.
func (s *Service) logAccess(ctx context.Context, tenant pkgcore.TenantID, shareID string, granted bool, p AccessParams) {
	outcome := AccessOutcomeDenied
	if granted {
		outcome = AccessOutcomeGranted
	}
	entry := &AccessLogEntry{
		ID:          s.newAccessLogID(),
		TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
		ShareID:     shareID,
		OccurredAt:  s.now(),
		IP:          p.IP,
		UserAgent:   p.UserAgent,
		Referrer:    p.Referrer,
		Outcome:     outcome,
	}
	if err := s.accessLogs.Create(ctx, entry); err != nil {
		observability.FromContext(ctx).Warn("sharing access log write failed", "share_id", shareID, "error", err)
	}
}

// Revoke withdraws share immediately: the very next Access call against it
// refuses with ErrNotAccessible, per rule 3
// (docs/internal/07-platform-services.md's "revocation takes effect
// immediately" rule) -- there is no
// cache anywhere on this module's own side to invalidate, so this method
// need do nothing beyond persisting RevokedAt. Revoking an already-revoked
// share is idempotent and reports success.
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
	share.RevokedAt = &now
	if err := s.shares.Update(ctx, share); err != nil {
		return err
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
