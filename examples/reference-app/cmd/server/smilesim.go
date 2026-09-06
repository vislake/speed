// The reference app's smile-simulation HTTP surface: the app-side
// implementation of the spec-derived interface generated from
// internal/smilesim/api/openapi.yaml. Product round P3a promoted the
// three routes from hand-written registrations (the round-2 enqueue and
// job-status routes, plus the P2a per-photo enumeration) to that
// fragment: this type implements the generated smilesimapi.ServerInterface
// (compile-time-checked below), and its routing is registered by the
// generated api.HandlerFromMux helper, which derives this surface's
// method+path patterns from the "paths:" keys of the spec fragment
// itself -- replacing what used to be a hand-written registration of the
// same patterns, one less copy of path+method truth to keep in step with
// the spec by hand. The fragment joins the merged application document,
// so the operations ship in the generated @speed/api-sdk surface the P3
// gallery calls.
//
// The surface demonstrates go/ai-gateway round 2's Gateway.GenerateImage
// end to end: smilesim_flow_test.go drives it through the composed HTTP
// stack against an httptest.Server standing in for the OpenAI-compatible
// images endpoint.
//
// ai-gateway itself ships no HTTP surface for image generation (see
// go/ai-gateway/AGENTS.md's "What this round ships" section), which is
// why these routes are the app's own -- like consult.go's round-1 chat
// route and the demo-notification route, they are mounted directly on
// mux rather than through reg.Routes/mountModuleRoutes, so none needs
// (and cannot silently skip) an entry in demoRouteGuards' table. consult's
// route remains hand-written for now (its chat surface has no P3 web
// consumer yet); this file's routes are the first app-owned surfaces to
// grow a fragment.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	aigateway "github.com/vislake/speed/go/ai-gateway"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
	smilesimapi "github.com/vislake/speed/examples/reference-app/internal/smilesim/api"
)

// smileSimErrInternal folds any error this file's handlers surface that is
// not itself an *apperr.Error into a stable code, the same fallback
// consult.go's own writeConsultError applies.
var smileSimErrInternal = apperr.Internal("smilesim.internal_error")

// smilesimHandler implements smilesimapi.ServerInterface -- the app-side
// implementation of the spec fragment's three operations: enqueue one
// async simulation (POST /api/v1/smile-simulation/simulate), poll its
// job status (GET /api/v1/smile-simulation/jobs/{jobID}) and enumerate
// every simulation generated from one photo (GET
// /api/v1/smile-simulation/photos/{photoObjectID}/simulations), backed
// by svc and queue.
//
// None of the three operations takes a subject or checks a permission of
// its own: in this app every authenticated member of a tenant may request
// a simulation, and the tenant scoping that actually protects another
// tenant's photo -- go/storage's own ObjectService.OpenContent, read
// inside the job handler from the job's own rebuilt tenant context --
// another tenant's job id -- go/jobs' own Queue.Get, which reports
// ErrJobNotFound for an id outside ctx's tenant, indistinguishable from
// an unknown one -- and another tenant's simulation rows --
// svc.ListSimulationsByPhoto/OptionsForJob, both tenant-scoped the same
// way -- are what actually gate access.
//
// The one exception is the optional recipient_user_id of the simulate
// operation (SmilesimSimulate): a caller may name only a recipient that is
// an active member of the caller's OWN tenant, checked through
// memberships -- the app's membership answer (sign_in_memberships.go,
// the same store authn's MembershipReader reads) -- before Simulate
// enqueues anything. That check exists because the completion
// notification a named recipient triggers (internal/smilesim's
// EventSimulationCompleted, dispatched by demo_notification.go's
// subscription as a RecipientClassUser delivery under the caller's tenant)
// would otherwise let a member of one tenant put an SMS on the phone of a
// member of any other: go/notification resolves a user recipient's
// addresses by user id alone, never checking the tenant a user belongs to
// (that question is the host's, and this is the host's answer).
type smilesimHandler struct {
	svc   *smilesim.Service
	queue jobs.Queue
	// memberships is the membership store buildServer wires authn's
	// MembershipReader to. Always non-nil in this app's wiring; nil would
	// make every named-recipient request fail closed rather than pass.
	memberships *signInMemberships
}

// compile-time check that smilesimHandler implements every operation the
// spec fragment declares -- a spec change whose generated interface
// outgrew this file stops the app from compiling.
var _ smilesimapi.ServerInterface = (*smilesimHandler)(nil)

// wireSmileSim mounts this surface's three routes on mux, backed by svc
// and queue, through the generated api.HandlerFromMux helper: the mount
// patterns come from internal/smilesim/api/openapi.yaml itself, never a
// second hand-written copy. memberships is the store SmilesimSimulate's
// recipient gate asks (see the handler type's own doc comment).
func wireSmileSim(mux *http.ServeMux, svc *smilesim.Service, queue jobs.Queue, memberships *signInMemberships) {
	smilesimapi.HandlerFromMux(&smilesimHandler{svc: svc, queue: queue, memberships: memberships}, mux)
}

// SmilesimSimulate implements smilesimapi.ServerInterface: it handles
// POST /api/v1/smile-simulation/simulate, decoding the body into the
// spec-generated smilesimapi.SmilesimSimulateRequest -- whose only
// required field is photo_object_id, with a deliberately absent
// tenant_id field anywhere on the request, the tenant being the one
// tenancy.Middleware already resolved into the request context
// (encoding/json's default decoder silently ignores any unknown field a
// client does send, such as a forged tenant_id). The 202 answer carries
// the async job's id -- the simulation itself has not run yet.
func (h *smilesimHandler) SmilesimSimulate(w http.ResponseWriter, r *http.Request) {
	var body smilesimapi.SmilesimSimulateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeSmileSimError(w, apperr.Invalid("smilesim.invalid_request_body").WithCause(err))
		return
	}
	if body.PhotoObjectID == "" {
		writeSmileSimError(w, apperr.Invalid("smilesim.photo_object_id_required"))
		return
	}

	// Each explicitly present option becomes a functional option for
	// svc.Simulate; omitted fields are left to the service's own
	// defaults. An invalid explicitly-present value is refused inside
	// Simulate -- before any credit is reserved and before any job is
	// enqueued -- with the coded smilesim.* error this handler passes
	// through verbatim.
	simOptions := make([]smilesim.SimulateOption, 0, 3)
	if body.Options != nil {
		if body.Options.SmileStyle != nil {
			simOptions = append(simOptions, smilesim.WithSmileStyle(smilesim.SmileStyle(*body.Options.SmileStyle)))
		}
		if body.Options.ToothShade != nil {
			simOptions = append(simOptions, smilesim.WithToothShade(smilesim.ToothShade(*body.Options.ToothShade)))
		}
		if body.Options.Strength != nil {
			simOptions = append(simOptions, smilesim.WithStrength(*body.Options.Strength))
		}
	}

	// RecipientUserID is optional: a caller that supplies one gets an
	// EventSimulationCompleted notification once the job finishes
	// (svc.NotifyOnCompletion, called from SmilesimGetJob below); a
	// caller that omits it just polls for the result, same as before
	// this field existed.
	//
	// A named recipient must be an active member of the caller's own
	// tenant -- the completion delivery would otherwise go out under this
	// tenant to a user of any other (see the handler type's own doc
	// comment). The check runs BEFORE Simulate is called, so a refused
	// recipient enqueues nothing: no job, no credit reservation, no
	// per-photo record, no eventual SMS.
	recipientUserID := ""
	if body.RecipientUserID != nil {
		recipientUserID = *body.RecipientUserID
	}
	if recipientUserID != "" {
		if err := validateSimulateRecipient(r.Context(), h.memberships, recipientUserID); err != nil {
			writeSmileSimError(w, err)
			return
		}
	}
	jobID, err := h.svc.Simulate(r.Context(), body.PhotoObjectID, recipientUserID, simOptions...)
	if err != nil {
		writeSmileSimError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(smilesimapi.SmilesimJobRef{JobID: string(jobID)})
}

// SmilesimGetJob implements smilesimapi.ServerInterface: it handles GET
// /api/v1/smile-simulation/jobs/{jobID}, polling the same jobs.Queue this
// app shares with go/ai-gateway and answering the job's live status --
// plus, once it has succeeded, the generated image's go/storage object
// id and the real vendor usage the job recorded. When a durable
// per-photo record exists for the job (every job this app enqueued since
// the P2a round), the answer also carries the effective options the
// simulation was generated with.
func (h *smilesimHandler) SmilesimGetJob(w http.ResponseWriter, r *http.Request, jobID string) {
	job, err := h.queue.Get(r.Context(), jobs.JobID(jobID))
	if err != nil {
		writeSmileSimError(w, err)
		return
	}

	// A no-op unless Simulate was given a recipient for this job and
	// job has just reached a terminal status for the first time --
	// see NotifyOnCompletion's own doc comment for why this poll
	// route is where that check happens. A failure here is logged and
	// swallowed: the notification side channel must never turn an
	// otherwise-successful status read into an error response.
	if notifyErr := h.svc.NotifyOnCompletion(r.Context(), job); notifyErr != nil {
		observability.FromContext(r.Context()).Warn("smilesim completion notification failed",
			"job_id", jobID, "error", notifyErr)
	}

	resp := smilesimapi.SmilesimJobStatus{Status: smilesimapi.SmilesimJobStatusStatus(job.Status)}

	// The effective options this job was generated with, when a
	// durable per-photo record exists for it (see the package doc
	// comment's "Per-photo result index" section). Attaching them is a
	// secondary convenience of the status read: a store failure is
	// logged and the options are simply omitted, never allowed to turn
	// an otherwise-successful status read into an error response --
	// the same swallow rule as the notification call just above.
	if opts, has, optsErr := h.svc.OptionsForJob(r.Context(), job.ID); optsErr != nil {
		observability.FromContext(r.Context()).Warn("smilesim reading recorded options failed, omitting them from the status response",
			"job_id", jobID, "error", optsErr)
	} else if has {
		resp.Options = toSmilesimOptions(opts)
	}

	switch job.Status {
	case jobs.StatusSucceeded:
		var result aigateway.ImageJobResult
		if job.Result != nil {
			if decErr := json.Unmarshal(job.Result.Data, &result); decErr != nil {
				writeSmileSimError(w, smileSimErrInternal.WithCause(decErr))
				return
			}
		}
		resp.OutputObjectID = &result.OutputObjectID
		resp.Usage = &smilesimapi.SmilesimUsage{
			ImageCount:     result.Usage.ImageCount,
			Steps:          result.Usage.Steps,
			ResolutionTier: result.Usage.ResolutionTier,
		}
	case jobs.StatusDeadLetter, jobs.StatusCancelled, jobs.StatusRetrying:
		resp.Error = &job.Error
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// SmilesimListPhotoSimulations implements smilesimapi.ServerInterface: it
// handles GET /api/v1/smile-simulation/photos/{photoObjectID}/simulations,
// listing every simulation generated from that photo under the caller's
// tenant, newest first, each entry carrying its options, its live status
// and -- once the job succeeded -- its output object id. It is the P3
// gallery's data source (see internal/smilesim's package doc comment's
// "Per-photo result index" section).
func (h *smilesimHandler) SmilesimListPhotoSimulations(w http.ResponseWriter, r *http.Request, photoObjectID string) {
	outcomes, err := h.svc.ListSimulationsByPhoto(r.Context(), photoObjectID)
	if err != nil {
		writeSmileSimError(w, err)
		return
	}

	simulations := make([]smilesimapi.SmilesimSimulation, 0, len(outcomes))
	for _, outcome := range outcomes {
		entry := smilesimapi.SmilesimSimulation{
			JobID:         string(outcome.JobID),
			PhotoObjectID: outcome.PhotoObjectID,
			Options:       *toSmilesimOptions(outcome.Options),
			Status:        smilesimapi.SmilesimSimulationStatus(outcome.Status),
			// CreatedAt renders on the wire in the same whole-seconds UTC
			// form the hand-written route used: the stored time is UTC
			// (gorm's autoCreateTime round-trips it so), and truncating
			// the sub-second part keeps encoding/json's RFC3339Nano
			// rendering byte-identical to the old RFC3339 output.
			CreatedAt: outcome.CreatedAt.Truncate(time.Second),
		}
		if outcome.OutputObjectID != "" {
			entry.OutputObjectID = &outcome.OutputObjectID
		}
		if outcome.Error != "" {
			entry.Error = &outcome.Error
		}
		simulations = append(simulations, entry)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(smilesimapi.SmilesimPhotoSimulations{Simulations: simulations})
}

// toSmilesimOptions converts the service's resolved option set to its
// spec-generated wire type, the shape both the job-status route's options
// echo and the enumeration route's per-entry options field share. Every
// field of the resolved set is always set, hence always present on the
// wire, exactly as the hand-written mirror emitted them.
func toSmilesimOptions(o smilesim.SimulationOptions) *smilesimapi.SmilesimSimulationOptions {
	smileStyle := smilesimapi.SmilesimSimulationOptionsSmileStyle(o.SmileStyle)
	toothShade := smilesimapi.SmilesimSimulationOptionsToothShade(o.ToothShade)
	return &smilesimapi.SmilesimSimulationOptions{
		SmileStyle: &smileStyle,
		ToothShade: &toothShade,
		Strength:   &o.Strength,
	}
}

// smilesimErrRecipientNotInTenant is the coded refusal SmilesimSimulate
// answers when a request names a simulation recipient who is not an active
// member of the caller's tenant -- the app's model of who a tenant may
// notify (validateSimulateRecipient's own doc comment, below). A 400, in
// the same family as the surface's other request-shape refusals
// (smilesim.invalid_request_body, smilesim.photo_object_id_required, the
// option-validation codes): the recipient_user_id value is simply not
// acceptable for this caller.
var smilesimErrRecipientNotInTenant = apperr.Invalid("smilesim.recipient_not_in_tenant")

// validateSimulateRecipient is the recipient gate SmilesimSimulate runs
// before it calls Simulate: recipientUserID must be an active member of
// the tenant ctx carries, answered through memberships -- the app's own
// membership store (sign_in_memberships.go), the same org-rows-first,
// roster-second answer authn's MembershipReader gives. "Active member of
// the caller's tenant" is this app's model of a user its tenant may
// legitimately notify: the completion delivery demo_notification.go's
// subscription dispatches goes out as a RecipientClassUser delivery under
// the SIMULATE caller's tenant, and go/notification itself never checks
// which tenant a user recipient belongs to -- that membership question is
// the host's, and this is the host's answer (the handler type's own doc
// comment has the full argument). An empty tenant or an unanswerable
// membership question fails closed with the internal error rather than
// guessing; an honest non-member answer is the coded 400 refusal.
func validateSimulateRecipient(ctx context.Context, memberships *signInMemberships, recipientUserID string) error {
	if memberships == nil {
		return smileSimErrInternal
	}
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return smileSimErrInternal
	}
	isMember, err := memberships.ActiveMembership(ctx, recipientUserID, tenant)
	if err != nil {
		return smileSimErrInternal.WithCause(err)
	}
	if !isMember {
		return smilesimErrRecipientNotInTenant
	}
	return nil
}

// writeSmileSimError writes err to w as a JSON {code, params} body, the
// same structured-error envelope shape consult.go's own writeConsultError
// produces, encoded through the spec fragment's SmilesimError type.
func writeSmileSimError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = smileSimErrInternal
	}
	envelope := smilesimapi.SmilesimError{Code: &appErr.Code}
	// Params stays nil (and thus omitted, per its omitempty tag) unless
	// the error actually carries parameters: a pointer to a nil map would
	// marshal as "params": null instead of the key being absent, which is
	// not the shape the hand-written envelope produced.
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}
