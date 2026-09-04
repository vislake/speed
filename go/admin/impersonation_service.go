package admin

import (
	"context"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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

	// now is the clock, overridden by tests; time.Now in production.
	now func() time.Time
}

// NewImpersonationService returns an ImpersonationService over repo.
func NewImpersonationService(repo *ImpersonationRepository) *ImpersonationService {
	return &ImpersonationService{repo: repo, now: time.Now}
}

// attach gives the service its host seams, read from the *pkgcore.Registry
// during Module.Register.
func (s *ImpersonationService) attach(bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, notifier Notifier) {
	s.bus = bus
	s.auditActions = actions
	s.notifier = notifier
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
	// Locale is the locale the mandatory security notification renders in
	// -- the ADMINISTRATOR's own negotiated locale is irrelevant here; per
	// root CLAUDE.md's i18n rule, backend-generated content renders in the
	// RECIPIENT's locale, so this should be the target user's own locale
	// when the caller knows it, and is passed through to
	// notification.Dispatch unchanged (an empty value refuses the
	// dispatch there for a user recipient, exactly as any other caller of
	// Dispatch would be refused -- Start does not invent a default on the
	// target's behalf).
	Locale string
}

// Start opens a new impersonation grant, exactly as docs/internal/23-admin.md
// section 4 describes: it is refused (ErrImpersonationReasonRequired,
// ErrImpersonationTargetRequired, ErrImpersonationSelfNotAllowed) before
// anything is written, records admin.impersonation.started as an explicit
// dual-identity audit event once the grant row commits, and dispatches the
// mandatory, non-unsubscribable security notification to the target user.
// Both the audit record and the notification are best-effort side effects
// of an already-successful write (see recordAudit's and notifyStarted's own
// doc comments) -- a failure in either is logged and never turns a
// successfully started grant into an error response.
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
	s.notifyStarted(ctx, in, grant)
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

// notifyStarted sends the target user the mandatory, non-unsubscribable
// security notification (NotificationTypeImpersonationStarted,
// notifications.go) announcing that a platform administrator has started
// an impersonation session against their account.
//
// It runs under an explicit tenancy.WithSystemContext grant, exactly like
// every other cross-tenant operation admin performs (D2): the target
// tenant is very unlikely to be the ambient tenant of the administrator's
// own request (their own session lives in rbac.SystemDomain), so writing
// the notification's delivery job into the TARGET tenant's own queue and
// tables is itself a cross-tenant write this module must not perform
// silently. A failure at either step -- entering the system context, or
// the dispatch itself -- is logged and swallowed: the grant already
// exists and is already usable by the time this runs, so a notification
// failure must not undo it or fail the Start call it rides along with.
func (s *ImpersonationService) notifyStarted(ctx context.Context, in StartInput, grant *ImpersonationGrant) {
	if s.notifier == nil {
		return
	}
	log := obs.FromContext(ctx)

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
			"grant_id", grant.ID, "target_tenant_id", in.TargetTenantID, "error", err)
		return
	}

	_, err = s.notifier.Dispatch(sysCtx, notification.Dispatch{
		TypeKey: NotificationTypeImpersonationStarted,
		Recipient: notification.DispatchRecipient{
			Class:  notification.RecipientClassUser,
			UserID: in.TargetUserID,
		},
		Locale: in.Locale,
		Params: map[string]any{
			"admin_user_id": in.AdminUserID,
			"reason":        in.Reason,
		},
	})
	if err != nil {
		log.Warn("admin could not dispatch the mandatory impersonation-started notification",
			"grant_id", grant.ID, "target_user_id", in.TargetUserID, "error", err)
	}
}
