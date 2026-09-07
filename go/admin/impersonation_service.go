package admin

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// defaultGrantTTL is how long an impersonation grant is usable after it is
// started, with no renewal operation offered at all -- "short-lived,
// explicitly revocable", docs/internal/23-admin.md section 4.1's summary of
// D5, suggests 30 minutes, which this round takes as the fixed value.
const defaultGrantTTL = 30 * time.Minute

// systemEndedByReason is the ImpersonationGrant.EndedBy sentinel value the
// automatic permission-revocation reconciliation below stamps -- see
// endIfNoLongerPermitted's own doc comment (P2-3's fix). It is
// distinguishable from every real user id at a glance without adding a
// column, mirroring the "system:" prefix convention pkgcore.ActorTypeSystem
// names for exactly this kind of non-human actor.
const systemEndedByReason = "system:rbac_permission_revoked"

// impersonatePermissionResource and impersonatePermissionAction are
// PermissionImpersonate's own "<resource>:<action>" halves (module.go's
// `PermissionImpersonate = "admin:impersonate"`), split once here as named
// constants rather than parsed at call time, for endIfNoLongerPermitted's
// rbac.Service.Can re-check.
const (
	impersonatePermissionResource = "admin"
	impersonatePermissionAction   = "impersonate"
)

// Notifier is the narrow view of *notification.DeliveryService Start needs
// to dispatch the mandatory impersonation-started security notification.
// It is declared here, as a structurally-typed interface, rather than
// taking *notification.DeliveryService directly, purely so a test can
// substitute a fake without constructing a whole notification.Module (a
// real db, blind indexers, a jobs.Queue, ...) -- *notification.DeliveryService
// satisfies it with no adapter required.
type Notifier interface {
	Dispatch(ctx context.Context, d notification.Dispatch) (jobs.JobID, error)
}

// MembershipChecker is the narrow view of *org.MemberService Start needs to
// confirm the impersonation target is a genuine member of the tenant the
// grant is scoped to (P3-4's fix) -- declared here, structurally, for the
// identical test-substitution reason Notifier is: *org.MemberService
// satisfies it with no adapter required, and a lightweight unit test can
// stand in a fake without constructing a whole org.Module.
type MembershipChecker interface {
	Get(ctx context.Context, userID string) (*org.Membership, error)
}

// ImpersonationService is D5's full pipeline runtime: starting and ending a
// grant, listing the ones currently in effect, and Lookup -- the one method
// the request-pipeline middleware (pipeline.go) calls on every request.
type ImpersonationService struct {
	repo *ImpersonationRepository

	// bus and auditActions back the explicit audit.Emit calls Start and End
	// make; notifier is what Start dispatches the mandatory security
	// notification through. All three are nil until Module.Register calls
	// attach, and every method that uses them tolerates that by skipping
	// the side effect -- see recordAudit's and dispatchStartNotification's
	// own doc comments for why that is the right failure mode rather than
	// a panic or a request failure.
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	notifier     Notifier

	// authnSvc resolves the impersonation target's own existence and locale
	// for the mandatory security notification (see resolveNotificationLocale)
	// -- the same *authn.Service SearchService (search.go) already holds
	// directly, admin being the one module this codebase's own
	// module-boundary rule permits to import a downstream module's
	// concrete package rather than a structurally-typed seam (AGENTS.md's
	// "admin sits at the top of the module dependency graph" section).
	// Nil only before Module.Register calls attach (see attach's own doc
	// comment) -- WithAuthn is a mandatory option, so this is never nil in
	// a correctly wired production Bootstrap.
	authnSvc *authn.Service

	// members confirms the impersonation target's tenant membership
	// (P3-4's fix, validateTargetMembership). Nil until Module.Register
	// calls attach -- WithOrg is a mandatory option, so this is never nil
	// in a correctly wired production Bootstrap; tolerated as nil
	// otherwise by skipping the check, the same pre-Register tolerance
	// every other seam on this struct documents.
	members MembershipChecker

	// rbacSvc is the *rbac.Service P2-3's fix needs to re-verify, when
	// notified of a role-binding revoke or a role redefinition in
	// rbac.SystemDomain, whether an administrator holding a live
	// impersonation grant still actually holds admin:impersonate. Nil
	// until Module.AttachRBAC is called -- the same post-Bootstrap-only
	// wiring RoleService's own rbacSvc field requires (role.go's own doc
	// comment has the full reasoning for why this cannot be a
	// construction-time option). Tolerated as nil by skipping the
	// automatic-end check entirely: the per-request grant.Active check
	// pipeline.go's ImpersonationMiddleware already performs still gates
	// every request regardless, so the worst case before AttachRBAC has
	// run is "a revoked administrator's grant survives until its natural
	// 30-minute expiry" rather than an unbounded window -- and in a
	// correctly wired host AttachRBAC runs immediately after Bootstrap,
	// before any real traffic, making that window unreachable in practice.
	rbacSvc *rbac.Service

	// now is the clock, overridden by tests; time.Now in production.
	now func() time.Time
}

// NewImpersonationService returns an ImpersonationService over repo.
func NewImpersonationService(repo *ImpersonationRepository) *ImpersonationService {
	return &ImpersonationService{repo: repo, now: time.Now}
}

// attach gives the service its host seams, read from the *pkgcore.Registry
// (and, for authnSvc, from Module.authnModule.Service()) during
// Module.Register.
func (s *ImpersonationService) attach(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, notifier Notifier, authnSvc *authn.Service, members MembershipChecker) {
	s.bus = bus
	s.auditActions = actions
	s.notifier = notifier
	s.authnSvc = authnSvc
	s.members = members
}

// attachRBAC gives the service the *rbac.Service P2-3's fix needs. Called by
// Module.AttachRBAC alongside RoleService's own attach -- see rbacSvc's own
// doc comment for the wiring contract and why this cannot happen any
// earlier.
func (s *ImpersonationService) attachRBAC(svc *rbac.Service) { s.rbacSvc = svc }

// StartInput is Start's input.
type StartInput struct {
	// AdminUserID is the platform administrator starting the grant --
	// resolved by the HTTP handler from the caller's own verified
	// Principal, never accepted as a request field.
	AdminUserID string
	// TargetUserID is the user to impersonate.
	TargetUserID string
	// TargetTenantID is the tenant the impersonation is scoped to.
	TargetTenantID pkgcore.TenantID
	// Reason is the operator's required justification.
	Reason string
	// Locale OPTIONALLY overrides the locale the mandatory security
	// notification renders in -- the ADMINISTRATOR's own negotiated locale
	// is irrelevant here; per root CLAUDE.md's i18n rule, backend-generated
	// content renders in the RECIPIENT's locale. When empty (the ordinary
	// case: most callers, including admin's own generated HTTP client,
	// have no reason to know the target's locale), Start resolves the
	// target's own authn.User.Locale itself (falling back to
	// authn.DefaultLocale when the user has never chosen one) rather than
	// letting the notification's Locale requirement -- REQUIRED for a
	// RecipientClassUser Dispatch -- silently defeat the "mandatory"
	// notification, which is exactly the bug a caller-optional field with
	// no resolution behind it used to cause (P1-1: see
	// dispatchStartNotification's own doc comment). A non-empty value here
	// is trusted verbatim -- an operator who genuinely knows better than
	// the stored profile value is not refused -- and Start's
	// dispatch-or-refuse contract applies identically either way.
	Locale string
}

// Start opens a new impersonation grant, exactly as docs/internal/23-admin.md
// section 4 describes: it is refused (ErrImpersonationReasonRequired,
// ErrImpersonationTargetRequired, ErrImpersonationSelfNotAllowed,
// ErrImpersonationTargetForbidden, ErrImpersonationTargetNotFound,
// ErrImpersonationTargetNotMember) before anything is written, dispatches
// the mandatory, non-unsubscribable security notification to the target
// user BEFORE the grant row is ever created, and records
// admin.impersonation.started as an explicit dual-identity audit event
// once the grant row commits.
//
// Target validation (P3-4's fix) happens in one pass, reusing a single
// cross-tenant system-context grant (D2's mechanism) for both the
// membership check and the notification dispatch that follows it, rather
// than entering system context twice: resolveNotificationLocale's own
// authn.Service lookup (unconditional now -- see its own doc comment for
// why it no longer skips existence checking merely because a caller
// supplied an explicit Locale) answers "does this account exist at all",
// and validateTargetMembership answers "is it actually a member of the
// tenant this grant is scoped to". Both run before the notification is
// ever dispatched and before any grant row is written.
//
// The notification is dispatched after both checks pass, and its failure
// refuses the whole call (no grant row is written), because a "mandatory"
// notification that is only attempted after the grant already exists and
// succeeded cannot ever be un-sent if the attempt fails -- P1-1's own
// finding: the caller received 201 while the notification silently never
// went out. What "dispatched" means here is deliberately narrow --
// dispatchStartNotification gets the delivery successfully ENQUEUED
// (notification.DeliveryService.Dispatch validates and enqueues, nothing
// more); actual transport delivery, its retries and its eventual
// dead-lettering are notification's own asynchronous business, exactly as
// for any other Dispatch caller, and are not and cannot be observed here.
// The audit record remains a best-effort side effect of the
// already-successful grant write (see recordAudit's own doc comment) --
// unlike the notification, a lost audit event does not mean the target was
// never told, so it keeps its original log-and-swallow contract.
func (s *ImpersonationService) Start(ctx context.Context, in StartInput) (*ImpersonationGrant, error) {
	if in.Reason == "" {
		return nil, ErrImpersonationReasonRequired
	}
	if in.TargetUserID == "" || in.TargetTenantID == "" {
		return nil, ErrImpersonationTargetRequired
	}
	if in.TargetUserID == in.AdminUserID {
		return nil, ErrImpersonationSelfNotAllowed
	}
	// rbac.SystemDomain is the platform-operations pseudo-tenant every
	// admin:* permission is evaluated in (D1) -- see
	// ErrImpersonationTargetForbidden's own doc comment for why a grant
	// scoped to it is refused unconditionally rather than merely gated on
	// a stricter permission: this is the fix for a real, previously
	// unguarded privilege-escalation path (an operator holding only
	// admin:impersonate could otherwise pick a MORE-privileged
	// platform-staff account as the target and reach every admin:*
	// permission that account holds).
	if in.TargetTenantID == rbac.SystemDomain {
		return nil, ErrImpersonationTargetForbidden
	}

	// The whole validate-and-notify pass -- existence, tenant membership,
	// and the mandatory notification -- runs only when at least one of its
	// three seams is actually wired. This is the same pre-Module.Register
	// nil-tolerance every seam on this struct already documents (bus,
	// notifier, authnSvc, members): all three are MANDATORY options in a
	// correctly wired production Bootstrap (WithAuthn/WithOrg/
	// WithNotification), so this gate is never what stands between a real
	// deployment and Finding P3-4's validation -- it exists purely so a
	// lightweight unit test exercising unrelated behavior (Lookup/End/
	// ListActive semantics, say) that wires none of the three keeps
	// working exactly as it did before this fix, rather than being forced
	// to stand up a real authn/org/notification stack to call Start at
	// all.
	if s.authnSvc != nil || s.members != nil || s.notifier != nil {
		locale, err := s.resolveNotificationLocale(ctx, in)
		if err != nil {
			return nil, err
		}

		// Membership validation and the notification dispatch both need a
		// cross-tenant system-context grant (D2's mechanism); entering it
		// once here, rather than separately in each, avoids a second,
		// needless tenancy.system_context.entered audit event per Start
		// call.
		sysCtx, err := s.enterTargetSystemContext(ctx, in)
		if err != nil {
			return nil, err
		}
		if err := s.validateTargetMembership(sysCtx, in); err != nil {
			return nil, err
		}
		if err := s.dispatchStartNotification(sysCtx, in, locale); err != nil {
			return nil, err
		}
	}

	now := s.now()
	grant := &ImpersonationGrant{
		AdminUserID:    in.AdminUserID,
		TargetUserID:   in.TargetUserID,
		TargetTenantID: string(in.TargetTenantID),
		Reason:         in.Reason,
		CreatedAt:      now,
		ExpiresAt:      now.Add(defaultGrantTTL),
	}
	if err := s.repo.Create(ctx, grant); err != nil {
		return nil, err
	}

	s.recordAudit(ctx, AuditActionImpersonationStarted, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: in.AdminUserID}, in.TargetUserID, grant.ID, map[string]any{
		"target_tenant_id": grant.TargetTenantID,
		"expires_at":       grant.ExpiresAt,
	})
	return grant, nil
}

// End ends the grant named by id early, refusing with ErrImpersonationGrantEnded
// when it was already ended (by an earlier End, a concurrent End racing
// this one, or observed as expired -- see Lookup, which never mutates a row
// purely because it noticed expiry). endedByUserID is the operator ending
// it -- the grant's own AdminUserID, or a higher-privileged operator -- and
// becomes both EndedBy and the audit event's OnBehalfOf actor.
func (s *ImpersonationService) End(ctx context.Context, id, endedByUserID string) (*ImpersonationGrant, error) {
	grant, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.endGrant(ctx, grant, endedByUserID, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: endedByUserID})
}

// endGrant is End's (and the automatic permission-revocation
// reconciliation's, below) shared write path: it marks grant ended and
// persists that mark under ImpersonationRepository.SaveGuarded's
// conditional UPDATE, refusing with ErrImpersonationGrantEnded when the
// guard finds the row already ended -- P3-3's CAS fix (half 2). Two
// concurrent callers both handed a grant read while EndedAt was still nil
// (an operator's DELETE racing this module's own automatic end, say) no
// longer both succeed: only the first write actually lands, and the
// second is refused instead of silently producing a duplicate
// admin.impersonation.ended audit trail for one logical end.
//
// onBehalfOf is the audit event's OnBehalfOf actor -- the real
// administrator ending their own grant (pkgcore.ActorTypePlatformAdmin) or
// the system itself reacting to a revoked permission
// (pkgcore.ActorTypeSystem, endIfNoLongerPermitted's own doc comment).
func (s *ImpersonationService) endGrant(ctx context.Context, grant *ImpersonationGrant, endedByUserID string, onBehalfOf pkgcore.Actor) (*ImpersonationGrant, error) {
	if grant.EndedAt != nil {
		return nil, ErrImpersonationGrantEnded
	}
	now := s.now()
	grant.EndedAt = &now
	grant.EndedBy = endedByUserID
	landed, err := s.repo.SaveGuarded(ctx, grant)
	if err != nil {
		return nil, err
	}
	if !landed {
		return nil, ErrImpersonationGrantEnded
	}
	s.recordAudit(ctx, AuditActionImpersonationEnded, onBehalfOf, grant.TargetUserID, grant.ID, nil)
	return grant, nil
}

// ListActive returns every grant currently in effect -- D5's own
// self-audit listing, GET /api/v1/admin/impersonation.
func (s *ImpersonationService) ListActive(ctx context.Context) ([]ImpersonationGrant, error) {
	return s.repo.ListActive(ctx, s.now())
}

// Lookup returns the grant named by id, and true, ONLY when it exists and
// is Active right now; otherwise it returns (nil, false) and NEVER an
// error.
//
// This is the fail-closed contract the request-pipeline middleware
// (pipeline.go's ImpersonationMiddleware) depends on for property (c) of
// D5's five mandatory properties: an invalid, unknown, expired or
// already-ended grant id must fall back to the administrator's own real
// identity, never silently impersonate and never abort the request with an
// error either -- a storage error here is treated exactly like "no such
// grant" for that same fail-closed reason, logged rather than propagated,
// because propagating it would turn a transient read failure into either a
// crashed request (fail open in the wrong direction, denial of service) or
// -- far worse if a caller ever mishandled the error -- a bypass. Lookup
// deliberately never mutates a row it finds inactive-by-expiry: expiry is
// a pure function of ExpiresAt and "now", so there is nothing to persist,
// and End (or the automatic revocation-reaction below) remain the only
// paths that ever set EndedAt.
//
// P2-3's own fix does NOT live here: Lookup answers "is this grant still
// Active", not "does the administrator STILL hold admin:impersonate right
// now" -- re-checking rbac on every impersonated request would be a
// correct, but strictly more expensive, alternative design (see
// onRoleBindingRevoked's own doc comment for why the event-driven
// alternative implemented here was chosen instead). Once a live grant's
// administrator is reconciled by that mechanism, Lookup reports it exactly
// as any other ended grant -- (nil, false) -- with no special case needed.
func (s *ImpersonationService) Lookup(ctx context.Context, id string) (*ImpersonationGrant, bool) {
	if id == "" {
		return nil, false
	}
	grant, err := s.repo.Get(ctx, id)
	if err != nil {
		if !isGrantNotFound(err) {
			obs.FromContext(ctx).Warn("admin could not look up an impersonation grant; treating it as absent",
				"grant_id", id, "error", err)
		}
		return nil, false
	}
	if !grant.Active(s.now()) {
		return nil, false
	}
	return grant, true
}

// isGrantNotFound reports whether err is ErrGrantNotFound, classifying by
// Code through apperr.As rather than by pointer identity -- every WithParam
// call on an *apperr.Error derives a new value, so the exported sentinels
// are templates, never singletons that == or errors.Is could match.
func isGrantNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == ErrGrantNotFound.Code
}

// isImpersonationGrantEnded is isGrantNotFound's sibling for
// ErrImpersonationGrantEnded, used by endIfNoLongerPermitted to tell an
// ordinary "already ended" outcome (nothing to warn about -- a concurrent
// End, or this same reconciliation racing an earlier delivery of the
// identical event, already did the job) apart from a genuine failure.
func isImpersonationGrantEnded(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == ErrImpersonationGrantEnded.Code
}

// recordAudit emits an admin.impersonation.* audit event with the
// dual-identity shape root CLAUDE.md's impersonation rule requires: Actor
// is the impersonated (or about-to-be, for the started event) user,
// onBehalfOf is whoever performed the action on the impersonated user's
// behalf -- the real administrator for Start and an ordinary End, or the
// system itself for an automatic permission-revocation end (P2-3's fix).
// This is an EXPLICIT audit.Emit, never automatic write capture, precisely
// because automatic capture has no way to populate OnBehalfOf
// (docs/internal/23-admin.md section 4.1's own explanation, which this
// round's code realizes).
//
// P2-pkgcore-actor-1: both identities are resolved against the users
// table here, at record time (resolveActorName), so a dual-identity row
// carries both display names -- the impersonated target's on Actor and
// the real administrator's on OnBehalfOf -- and stays readable after
// either account is renamed or deleted. A system onBehalfOf (the
// automatic end) and any id with no user row behind it stay id-only,
// per resolveActorName's own policy.
//
// A publish failure is logged and swallowed: by the time this runs the
// grant row has already committed (Start) or already been marked ended
// (End), so surfacing an audit failure as the caller's own error would
// report a failure that did not happen -- matching go/pki's
// Handler.recordAudit and notes' recordNoteCreatedAudit.
func (s *ImpersonationService) recordAudit(ctx context.Context, action string, onBehalfOf pkgcore.Actor, targetUserID, grantID string, after map[string]any) {
	if s.bus == nil {
		return
	}
	auditCtx := pkgcore.WithActor(ctx, resolveActorName(ctx, s.authnSvc,
		pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: targetUserID}))
	auditCtx = pkgcore.WithOnBehalfOf(auditCtx, resolveActorName(ctx, s.authnSvc, onBehalfOf))

	var diff *audit.Diff
	if after != nil {
		diff = &audit.Diff{After: after}
	}
	err := audit.Emit(auditCtx, s.bus, s.auditActions, audit.Input{
		Action:   action,
		Resource: audit.Resource{Type: "admin.impersonation_grant", ID: grantID},
		Result:   audit.Result{Success: true},
		Changes:  diff,
	})
	if err != nil {
		obs.FromContext(ctx).Warn("admin failed to record an impersonation audit event",
			"grant_id", grantID, "action", action, "error", err)
	}
}

// enterTargetSystemContext opens the cross-tenant system-context grant
// (D2's mechanism) both validateTargetMembership and
// dispatchStartNotification run under -- entered exactly once per Start
// call (Start's own doc comment explains why sharing it matters).
func (s *ImpersonationService) enterTargetSystemContext(ctx context.Context, in StartInput) (context.Context, error) {
	sysCtx, err := tenancy.WithSystemContext(
		pkgcore.WithTenant(ctx, in.TargetTenantID),
		s.bus,
		pkgcore.SystemReason{
			Actor:   in.AdminUserID,
			Purpose: SystemPurposeAdminCrossTenant,
		},
	)
	if err != nil {
		obs.FromContext(ctx).Warn("admin could not enter a system context to validate or notify an impersonation target",
			"target_tenant_id", in.TargetTenantID, "error", err)
		return ctx, ErrImpersonationTargetValidationUnavailable.WithCause(err)
	}
	return sysCtx, nil
}

// validateTargetMembership confirms TargetUserID is a genuine member of
// TargetTenantID -- P3-4's fix: before this, Start never checked that the
// target belonged to the tenant a grant scoped to it, so a real-but-
// unrelated user id paired with an arbitrary tenant id still returned 201.
// ctx must already carry TargetTenantID's system-context grant
// (enterTargetSystemContext's own return value) -- org's MemberService,
// like every other tenant-scoped repository, reads the tenant from ctx,
// never from a parameter.
//
// A nil members (the org seam not wired yet -- pre-Module.Register, or a
// lightweight unit test) is tolerated by skipping the check entirely:
// WithOrg is mandatory in a correctly wired production Bootstrap, so this
// is unreachable there.
func (s *ImpersonationService) validateTargetMembership(ctx context.Context, in StartInput) error {
	if s.members == nil {
		return nil
	}
	if _, err := s.members.Get(ctx, in.TargetUserID); err != nil {
		if isMembershipNotFound(err) {
			return ErrImpersonationTargetNotMember.
				WithParam("target_user_id", in.TargetUserID).
				WithParam("target_tenant_id", string(in.TargetTenantID))
		}
		return ErrImpersonationTargetValidationUnavailable.WithCause(err)
	}
	return nil
}

// dispatchStartNotification gets the mandatory, non-unsubscribable
// security notification (NotificationTypeImpersonationStarted, module.go's
// Register call) ENQUEUED, announcing that a platform administrator has
// started an impersonation session against the target's account. ctx must
// already carry TargetTenantID's system-context grant, exactly like
// validateTargetMembership -- writing the notification's delivery job into
// the TARGET tenant's own queue and tables is itself a cross-tenant write
// this module must not perform silently. There is deliberately no grant id
// to log or to hand to the dispatch: the notification's Params carry only
// admin_user_id and reason, so nothing here depends on one.
//
// A nil notifier is tolerated by returning nil (no error, nothing
// attempted): this is the pre-Module.Register state, exactly as
// recordAudit's own nil-bus tolerance, and is unreachable in production,
// since WithNotification is a mandatory Register option. Every OTHER
// failure here -- the dispatch itself was refused -- is P1-1's own fix: it
// is returned as a real error rather than logged and swallowed, so Start
// refuses the whole call instead of returning success over a notification
// nobody will ever receive.
func (s *ImpersonationService) dispatchStartNotification(ctx context.Context, in StartInput, locale string) error {
	if s.notifier == nil {
		return nil
	}
	if _, err := s.notifier.Dispatch(ctx, notification.Dispatch{
		TypeKey: NotificationTypeImpersonationStarted,
		Recipient: notification.DispatchRecipient{
			Class:  notification.RecipientClassUser,
			UserID: in.TargetUserID,
		},
		Locale: locale,
		Params: map[string]any{
			"admin_user_id": in.AdminUserID,
			"reason":        in.Reason,
		},
	}); err != nil {
		obs.FromContext(ctx).Warn("admin could not dispatch the mandatory impersonation-started notification",
			"target_user_id", in.TargetUserID, "error", err)
		return ErrImpersonationNotificationUnavailable.WithCause(err)
	}
	return nil
}

// resolveNotificationLocale answers the locale dispatchStartNotification
// dispatches in, and doubles as Start's target-existence check (P3-4's
// fix): it now ALWAYS resolves the target through authnSvc when that seam
// is wired, regardless of whether the caller supplied an explicit Locale --
// before this fix, an explicit Locale skipped the lookup entirely, so a
// caller who happened to supply one could start a grant for a target that
// did not even exist. Reusing this single lookup for both existence and
// locale resolution, rather than adding a second call, is deliberate (see
// Start's own doc comment).
//
// in.Locale is still trusted verbatim once the target is confirmed to
// exist -- the target's own authn.User.Locale (falling back to
// authn.DefaultLocale when the user has never chosen one -- the identical
// fallback authn's own verification-code delivery already applies, for the
// identical "empty means not chosen yet" reason User.Locale's own doc
// comment gives) is used only when the caller supplied none.
//
// A target that cannot be found at all (ErrNotFound) is refused with
// ErrImpersonationTargetNotFound rather than
// ErrImpersonationTargetValidationUnavailable -- a ghost user id is a bad
// request, not an infrastructure failure -- and any other resolution
// failure (a storage error) is ErrImpersonationTargetValidationUnavailable,
// since in every such case neither existence nor the mandatory
// notification can be guaranteed.
//
// A nil authnSvc (the seam not wired yet) is tolerated: an explicit Locale
// is trusted verbatim exactly as before this fix (existence cannot be
// checked without the seam, and WithAuthn is mandatory in a correctly
// wired production Bootstrap, so this branch is unreachable there), and an
// empty Locale with no way to resolve one refuses with
// ErrImpersonationTargetValidationUnavailable.
func (s *ImpersonationService) resolveNotificationLocale(ctx context.Context, in StartInput) (string, error) {
	if s.authnSvc == nil {
		if in.Locale != "" {
			return in.Locale, nil
		}
		return "", ErrImpersonationTargetValidationUnavailable
	}
	user, err := s.authnSvc.Users().FindByID(ctx, in.TargetUserID)
	if err != nil {
		if errors.Is(err, authn.ErrNotFound) {
			return "", ErrImpersonationTargetNotFound.WithCause(err)
		}
		return "", ErrImpersonationTargetValidationUnavailable.WithCause(err)
	}
	if in.Locale != "" {
		return in.Locale, nil
	}
	if user.Locale == "" {
		return authn.DefaultLocale, nil
	}
	return user.Locale, nil
}

// onRoleBindingRevoked is P2-3's fix: rbac publishes EventRoleBindingRevoked
// synchronously (the same reliable-delivery path its own cross-replica
// cache-invalidation property already depends on -- go/rbac/AGENTS.md's own
// Testing section proves it converges against a real distributed bus)
// whenever a role binding is withdrawn. A revoke inside rbac.SystemDomain
// -- the pseudo-tenant every admin:* permission is evaluated in (D1) -- may
// have just taken away the very admin:impersonate permission that let its
// subject start an impersonation grant in the first place; Lookup's own
// contract never re-consults rbac (its own doc comment explains why),
// which used to mean a grant already issued would keep substituting the
// target's identity for the rest of its 30-minute TTL no matter how
// thoroughly the administrator's own permissions were revoked in the
// meantime -- docs/internal/23-admin.md section 4.1 never listed
// permission revocation among the ways a grant ends at all.
//
// It re-checks through a real rbac.Service.Can call rather than assuming
// the answer from the event alone, since the revoked binding may not have
// been the subject's ONLY source of admin:impersonate (a second role could
// still grant it) -- and ends every live grant the now-checked
// administrator no longer qualifies for. A decode failure or an event
// outside rbac.SystemDomain is dropped (Warn/no-op, no error): the
// identical four-case resilience contract go/rbac's own org.member.removed
// reap documents for a subscriber of a foreign event with no bearing here.
func (s *ImpersonationService) onRoleBindingRevoked(ctx context.Context, evt pkgcore.Event) error {
	payload, ok := decodeRoleBindingChangedPayload(evt.Payload)
	if !ok {
		obs.FromContext(ctx).Warn("admin ignored a rbac.role_binding.revoked event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}
	if payload.TenantID != string(rbac.SystemDomain) {
		return nil
	}
	s.reviewGrantsForAdmin(ctx, payload.UserID)
	return nil
}

// onRoleChanged is onRoleBindingRevoked's counterpart for a role
// REDEFINITION (rbac.EventRoleChanged) rather than a binding revoke: an
// administrator can lose admin:impersonate just as completely by having
// the role they hold edited to no longer grant it, with their own binding
// left untouched. The event carries no single subject -- every binding to
// the changed role is affected, rbac.RoleChangedEvent's own doc comment --
// so this re-checks EVERY administrator currently holding a live grant
// rather than one, bounded by however many impersonation grants are live
// platform-wide at the moment, ordinarily a small number.
func (s *ImpersonationService) onRoleChanged(ctx context.Context, evt pkgcore.Event) error {
	payload, ok := decodeRoleChangedPayload(evt.Payload)
	if !ok {
		obs.FromContext(ctx).Warn("admin ignored a rbac.role.changed event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}
	if payload.TenantID != string(rbac.SystemDomain) {
		return nil
	}
	s.reviewAllLiveGrants(ctx)
	return nil
}

// reviewGrantsForAdmin re-checks every live grant belonging to adminUserID
// against rbac's current state, ending whichever ones the administrator no
// longer qualifies for. A listing failure is Warn-logged and dropped
// rather than propagated -- the caller is an event-bus subscriber, which
// must never fail the publisher's own Publish call (rbac's own
// org.member.removed reap documents the identical constraint).
func (s *ImpersonationService) reviewGrantsForAdmin(ctx context.Context, adminUserID string) {
	if s.rbacSvc == nil || adminUserID == "" {
		return
	}
	grants, err := s.repo.ListActive(ctx, s.now())
	if err != nil {
		obs.FromContext(ctx).Warn("admin could not list active impersonation grants to re-check a revoked administrator's permission",
			"admin_user_id", adminUserID, "error", err)
		return
	}
	for i := range grants {
		if grants[i].AdminUserID != adminUserID {
			continue
		}
		s.endIfNoLongerPermitted(ctx, &grants[i])
	}
}

// reviewAllLiveGrants is reviewGrantsForAdmin's whole-ledger counterpart
// for onRoleChanged, which names no single administrator to narrow the
// scan to.
func (s *ImpersonationService) reviewAllLiveGrants(ctx context.Context) {
	if s.rbacSvc == nil {
		return
	}
	grants, err := s.repo.ListActive(ctx, s.now())
	if err != nil {
		obs.FromContext(ctx).Warn("admin could not list active impersonation grants to re-check a role-permission change",
			"error", err)
		return
	}
	for i := range grants {
		s.endIfNoLongerPermitted(ctx, &grants[i])
	}
}

// endIfNoLongerPermitted re-checks grant.AdminUserID's CURRENT
// admin:impersonate permission (never trusting the triggering event alone)
// and ends grant, with the system named as OnBehalfOf, when the
// administrator no longer holds it. Ending through endGrant's own
// SaveGuarded path means a concurrent End (an operator's own DELETE, or
// this same reconciliation racing an earlier delivery of the identical
// event) is a benign, silently-dropped no-op here -- isImpersonationGrantEnded
// -- never a Warn-logged failure.
func (s *ImpersonationService) endIfNoLongerPermitted(ctx context.Context, grant *ImpersonationGrant) {
	sub := rbac.Subject{TenantID: rbac.SystemDomain, UserID: grant.AdminUserID}
	allowed, err := s.rbacSvc.Can(ctx, sub, impersonatePermissionAction, impersonatePermissionResource)
	if err != nil {
		obs.FromContext(ctx).Warn("admin could not re-check an impersonating administrator's current permission",
			"grant_id", grant.ID, "admin_user_id", grant.AdminUserID, "error", err)
		return
	}
	if allowed {
		return
	}
	if _, err := s.endGrant(ctx, grant, systemEndedByReason, pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: systemEndedByReason}); err != nil {
		if !isImpersonationGrantEnded(err) {
			obs.FromContext(ctx).Warn("admin could not automatically end an impersonation grant after its administrator's permission was revoked",
				"grant_id", grant.ID, "admin_user_id", grant.AdminUserID, "error", err)
		}
	}
}

// decodeRoleBindingChangedPayload recovers a rbac.RoleBindingChangedEvent
// from whatever the EventBus delivered, the identical JSON-round-trip
// technique tenant_service.go's decodeEventPayload already uses for org's
// event (both the in-process struct and the distributed bus's decoded
// map[string]any shape work identically through encoding/json). TenantID
// and UserID are mandatory -- without both there is nothing to address a
// review by.
func decodeRoleBindingChangedPayload(payload any) (rbac.RoleBindingChangedEvent, bool) {
	var evt rbac.RoleBindingChangedEvent
	if err := decodeEventPayload(payload, &evt); err != nil {
		return rbac.RoleBindingChangedEvent{}, false
	}
	if evt.TenantID == "" || evt.UserID == "" {
		return rbac.RoleBindingChangedEvent{}, false
	}
	return evt, true
}

// decodeRoleChangedPayload is decodeRoleBindingChangedPayload's counterpart
// for rbac.RoleChangedEvent. Only TenantID is mandatory: a role change has
// no single subject (rbac.RoleChangedEvent's own doc comment).
func decodeRoleChangedPayload(payload any) (rbac.RoleChangedEvent, bool) {
	var evt rbac.RoleChangedEvent
	if err := decodeEventPayload(payload, &evt); err != nil {
		return rbac.RoleChangedEvent{}, false
	}
	if evt.TenantID == "" {
		return rbac.RoleChangedEvent{}, false
	}
	return evt, true
}
