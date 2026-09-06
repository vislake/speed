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

// ImpersonationService is D5's full pipeline runtime: starting and ending a
// grant, listing the ones currently in effect, and Lookup -- the one method
// the request-pipeline middleware (pipeline.go) calls on every request.
type ImpersonationService struct {
	repo *ImpersonationRepository

	// bus and auditActions back the explicit audit.Emit calls Start and End
	// make; notifier is what Start dispatches the mandatory security
	// notification through. All three are nil until Module.Register calls
	// attach, and every method that uses them tolerates that by skipping
	// the side effect -- see recordAudit's and notifyStarted's own doc
	// comments for why that is the right failure mode rather than a panic
	// or a request failure.
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	notifier     Notifier

	// authnSvc resolves the impersonation target's own locale for the
	// mandatory security notification (see resolveNotificationLocale) --
	// the same *authn.Service SearchService (search.go) already holds
	// directly, admin being the one module this codebase's own
	// module-boundary rule permits to import a downstream module's
	// concrete package rather than a structurally-typed seam (AGENTS.md's
	// "admin sits at the top of the module dependency graph" section).
	// Nil only before Module.Register calls attach (see attach's own doc
	// comment) -- WithAuthn is a mandatory option, so this is never nil in
	// a correctly wired production Bootstrap.
	authnSvc *authn.Service

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
func (s *ImpersonationService) attach(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, notifier Notifier, authnSvc *authn.Service) {
	s.bus = bus
	s.auditActions = actions
	s.notifier = notifier
	s.authnSvc = authnSvc
}

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
	// no resolution behind it used to cause (P1-1: see notifyStarted's own
	// doc comment). A non-empty value here is trusted verbatim -- an
	// operator who genuinely knows better than the stored profile value is
	// not refused -- and Start's dispatch-or-refuse contract applies
	// identically either way.
	Locale string
}

// Start opens a new impersonation grant, exactly as docs/internal/23-admin.md
// section 4 describes: it is refused (ErrImpersonationReasonRequired,
// ErrImpersonationTargetRequired, ErrImpersonationSelfNotAllowed,
// ErrImpersonationTargetForbidden) before anything is written, dispatches
// the mandatory, non-unsubscribable security notification to the target
// user BEFORE the grant row is ever created, and records
// admin.impersonation.started as an explicit dual-identity audit event
// once the grant row commits.
//
// The notification is dispatched first, and its failure refuses the whole
// call (no grant row is written), because a "mandatory" notification that
// is only attempted after the grant already exists and succeeded cannot
// ever be un-sent if the attempt fails -- P1-1's own finding: the caller
// received 201 while the notification silently never went out. What
// "dispatched" means here is deliberately narrow -- notifyStarted resolves
// a real recipient locale and gets the delivery successfully ENQUEUED
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

	if err := s.notifyStarted(ctx, in); err != nil {
		return nil, err
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

	s.recordAudit(ctx, AuditActionImpersonationStarted, in.AdminUserID, in.TargetUserID, grant.ID, map[string]any{
		"target_tenant_id": grant.TargetTenantID,
		"expires_at":       grant.ExpiresAt,
	})
	return grant, nil
}

// End ends the grant named by id early, refusing with ErrImpersonationGrantEnded
// when it was already ended (by an earlier End, or observed as expired --
// see Lookup, which never mutates a row purely because it noticed
// expiry). endedByUserID is the operator ending it -- the grant's own
// AdminUserID, or a higher-privileged operator -- and becomes both
// EndedBy and the audit event's OnBehalfOf actor.
func (s *ImpersonationService) End(ctx context.Context, id, endedByUserID string) (*ImpersonationGrant, error) {
	grant, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if grant.EndedAt != nil {
		return nil, ErrImpersonationGrantEnded
	}
	now := s.now()
	grant.EndedAt = &now
	grant.EndedBy = endedByUserID
	if err := s.repo.Save(ctx, grant); err != nil {
		return nil, err
	}
	s.recordAudit(ctx, AuditActionImpersonationEnded, endedByUserID, grant.TargetUserID, grant.ID, nil)
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
// and End remains the only path that ever sets EndedAt.
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

// recordAudit emits an admin.impersonation.* audit event with the
// dual-identity shape root CLAUDE.md's impersonation rule requires: Actor
// is the impersonated (or about-to-be, for the started event) user,
// OnBehalfOf is the real administrator performing the action. This is an
// EXPLICIT audit.Emit, never automatic write capture, precisely because
// automatic capture has no way to populate OnBehalfOf
// (docs/internal/23-admin.md section 4.1's own explanation, which this
// round's code realizes).
//
// A publish failure is logged and swallowed: by the time this runs the
// grant row has already committed (Start) or already been marked ended
// (End), so surfacing an audit failure as the caller's own error would
// report a failure that did not happen -- matching go/pki's
// Handler.recordAudit and notes' recordNoteCreatedAudit.
func (s *ImpersonationService) recordAudit(ctx context.Context, action, adminUserID, targetUserID, grantID string, after map[string]any) {
	if s.bus == nil {
		return
	}
	auditCtx := pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: targetUserID})
	auditCtx = pkgcore.WithOnBehalfOf(auditCtx, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: adminUserID})

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

// notifyStarted resolves the target user's own locale and gets the
// mandatory, non-unsubscribable security notification
// (NotificationTypeImpersonationStarted, notifications.go) ENQUEUED,
// announcing that a platform administrator has started an impersonation
// session against their account. It runs BEFORE any grant row exists (see
// Start's own doc comment for why) -- there is deliberately no grant id to
// log or to hand to the dispatch: notifications.go's Params carry only
// admin_user_id and reason, so nothing here depends on one.
//
// It runs under an explicit tenancy.WithSystemContext grant, exactly like
// every other cross-tenant operation admin performs (D2): the target
// tenant is very unlikely to be the ambient tenant of the administrator's
// own request (their own session lives in rbac.SystemDomain), so writing
// the notification's delivery job into the TARGET tenant's own queue and
// tables is itself a cross-tenant write this module must not perform
// silently.
//
// A nil notifier is tolerated by returning nil (no error, nothing
// attempted): this is the pre-Module.Register state -- attach has not run
// yet, exactly as recordAudit's own nil-bus tolerance -- and is
// unreachable in production, since WithNotification is a mandatory Register
// option. Every OTHER failure here -- the target's locale could not be
// resolved, the system-context grant could not be entered, or the
// dispatch itself was refused -- is P1-1's own fix: it is returned as a
// real error rather than logged and swallowed, so Start refuses the whole
// call instead of returning success over a notification nobody will ever
// receive.
func (s *ImpersonationService) notifyStarted(ctx context.Context, in StartInput) error {
	if s.notifier == nil {
		return nil
	}
	log := obs.FromContext(ctx)

	locale, err := s.resolveNotificationLocale(ctx, in)
	if err != nil {
		log.Warn("admin could not resolve the impersonation target's locale for the mandatory security notification",
			"target_user_id", in.TargetUserID, "error", err)
		return err
	}

	sysCtx, err := tenancy.WithSystemContext(
		pkgcore.WithTenant(ctx, in.TargetTenantID),
		s.bus,
		pkgcore.SystemReason{
			Actor:   in.AdminUserID,
			Purpose: SystemPurposeAdminCrossTenant,
		},
	)
	if err != nil {
		log.Warn("admin could not enter a system context to notify an impersonation target",
			"target_tenant_id", in.TargetTenantID, "error", err)
		return ErrImpersonationNotificationUnavailable.WithCause(err)
	}

	if _, err := s.notifier.Dispatch(sysCtx, notification.Dispatch{
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
		log.Warn("admin could not dispatch the mandatory impersonation-started notification",
			"target_user_id", in.TargetUserID, "error", err)
		return ErrImpersonationNotificationUnavailable.WithCause(err)
	}
	return nil
}

// resolveNotificationLocale answers the locale notifyStarted dispatches
// in: in.Locale verbatim when the caller supplied one, otherwise the
// target's own authn.User.Locale (falling back to authn.DefaultLocale when
// the user has never chosen one -- the identical fallback authn's own
// verification-code delivery already applies, for the identical "empty
// means not chosen yet" reason User.Locale's own doc comment gives).
//
// A target that cannot be found at all (ErrNotFound) is refused with
// ErrImpersonationTargetNotFound rather than ErrImpersonationNotificationUnavailable
// -- a ghost user id is a bad request, not an infrastructure failure -- and
// any other resolution failure (a storage error, or authnSvc never having
// been wired) is ErrImpersonationNotificationUnavailable, the same code a
// downstream dispatch or system-context failure reports, since in every
// case the mandatory notification cannot be guaranteed.
func (s *ImpersonationService) resolveNotificationLocale(ctx context.Context, in StartInput) (string, error) {
	if in.Locale != "" {
		return in.Locale, nil
	}
	if s.authnSvc == nil {
		return "", ErrImpersonationNotificationUnavailable
	}
	user, err := s.authnSvc.Users().FindByID(ctx, in.TargetUserID)
	if err != nil {
		if errors.Is(err, authn.ErrNotFound) {
			return "", ErrImpersonationTargetNotFound.WithCause(err)
		}
		return "", ErrImpersonationNotificationUnavailable.WithCause(err)
	}
	if user.Locale == "" {
		return authn.DefaultLocale, nil
	}
	return user.Locale, nil
}
