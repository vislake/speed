// The reference app's smile-simulation HTTP surface: the app-side
// implementation of the spec-derived interface generated from
// internal/smilesim/api/openapi.yaml. This type implements the generated
// smilesimapi.ServerInterface
// (compile-time-checked below), and its routing is registered by the
// generated api.HandlerFromMux helper, which derives this surface's
// method+path patterns from the "paths:" keys of the spec fragment
// itself -- one less copy of path+method truth to keep in step with
// the spec by hand. The fragment joins the merged application document,
// so the operations ship in the generated @speed/api-sdk surface the
// smile gallery calls.
//
// The surface demonstrates go/ai-gateway's Gateway.GenerateImage
// end to end: smilesim_flow_test.go drives it through the composed HTTP
// stack against an httptest.Server standing in for the OpenAI-compatible
// images endpoint.
//
// ai-gateway itself ships no HTTP surface for image generation, which is
// why these routes are the app's own -- like consult.go's chat
// route and the demo-notification route, they are mounted directly on
// mux rather than through reg.Routes/mountModuleRoutes, so none needs
// (and cannot silently skip) an entry in demoRouteGuards' table. consult's
// route remains hand-written (its chat surface has no web consumer to
// render it); this file's routes are the app-owned surfaces that grow a
// fragment.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/examples/reference-app/internal/attestation"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
	smilesimapi "github.com/vislake/speed/examples/reference-app/internal/smilesim/api"
)

// smileSimErrInternal folds any error this file's handlers surface that is
// not itself an *apperr.Error into a stable code, the same fallback
// consult.go's own writeConsultError applies.
var smileSimErrInternal = apperr.Internal("smilesim.internal_error")

// The simulate route's two request-shape envelopes, named declarations so
// the error-mapping audits of the web host (the codes-alignment suite,
// which cites every reachable code to the declaration that defines it)
// can cite stable sites: these are the reachable codes of the
// smile-simulation surface.
var (
	// smilesimErrInvalidRequestBody is the malformed-body answer every
	// body-reading smile route writes, mirroring casesInvalidRequestBody's
	// identical role on the cases surface.
	smilesimErrInvalidRequestBody = apperr.Invalid("smilesim.invalid_request_body")

	// smilesimErrPhotoObjectIDRequired refuses a simulate request that
	// names no photo to generate from.
	smilesimErrPhotoObjectIDRequired = apperr.Invalid("smilesim.photo_object_id_required")
)

// The simulation-content route's coded refusals (see
// SmilesimGetSimulationContent's own doc comment). Each is a NotFound, so
// no refusal of the read ever hints at whether another tenant's
// simulation or object exists behind the caller's question -- the same
// outward shape the cases surface's own photo-content refusal
// (cases.photo_not_found) takes.
var (
	// smileSimErrSimulationNotFound is the answer for a job the named
	// photo has no record of under the caller's tenant.
	smileSimErrSimulationNotFound = apperr.NotFound("smilesim.simulation_not_found")

	// smileSimErrOutputNotReady is the answer for a simulation whose job
	// has not succeeded: no generated image exists to serve yet.
	smileSimErrOutputNotReady = apperr.NotFound("smilesim.output_not_ready")

	// smileSimErrOutputNotFound is the answer when a succeeded
	// simulation's stored bytes no longer exist (the object was deleted
	// or reclaimed by the expiry sweep).
	smileSimErrOutputNotFound = apperr.NotFound("smilesim.output_not_found")
)

// smilesimHandler implements smilesimapi.ServerInterface -- the app-side
// implementation of the spec fragment's four operations: enqueue one
// async simulation (POST /api/v1/smile-simulation/simulate), poll its
// job status (GET /api/v1/smile-simulation/jobs/{jobID}), enumerate
// every simulation generated from one photo (GET
// /api/v1/smile-simulation/photos/{photoObjectID}/simulations) and read
// one simulation's generated image content (GET
// /api/v1/smile-simulation/photos/{photoObjectID}/simulations/
// {jobID}/content, which the before/after comparison view renders),
// backed by svc, queue and the storage objects service.
//
// None of the four operations takes a subject or checks a permission of
// its own: in this app every authenticated member of a tenant may request
// a simulation, and the tenant scoping that actually protects another
// tenant's photo -- go/storage's own ObjectService.OpenContent, read
// inside the job handler from the job's own rebuilt tenant context and
// by the content operation below from the request's own --
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
	// objects is the app's go/storage ObjectService, which the
	// simulation-content operation drives to read one generated image's
	// stored bytes -- the same instance the cases surface's photo routes
	// (cases_photos.go) drive, so both surfaces agree on what an object
	// is and which tenant's rows each read resolves.
	objects *storage.ObjectService

	// attest is the AI-output attestation service (internal/attestation):
	// every surface below that observes a succeeded simulation output --
	// the job-status poll, the per-photo enumeration and the
	// simulation-content read -- registers and attests that output through
	// it (ensureAttestedOutput's own doc comment), making the output
	// shareable through the chain-verified sharing gate. Nil skips the
	// hook (the handler's pre-consumer shape).
	attest *attestation.Service
}

// compile-time check that smilesimHandler implements every operation the
// spec fragment declares -- a spec change whose generated interface
// outgrew this file stops the app from compiling.
var _ smilesimapi.ServerInterface = (*smilesimHandler)(nil)

// wireSmileSim mounts this surface's four routes on mux, backed by svc,
// queue and the storage objects service, through the generated
// api.HandlerFromMux helper: the mount patterns come from
// internal/smilesim/api/openapi.yaml itself, never a second hand-written
// copy. memberships is the store SmilesimSimulate's recipient gate asks
// (see the handler type's own doc comment). attest is the AI-output
// attestation service every succeeded-output read below hooks (see the
// handler type's own field comment); always non-nil in this app's wiring
// (server.go constructs the service before wireSmileSim runs).
func wireSmileSim(mux *http.ServeMux, svc *smilesim.Service, queue jobs.Queue, memberships *signInMemberships, objects *storage.ObjectService, attest *attestation.Service) {
	smilesimapi.HandlerFromMux(&smilesimHandler{svc: svc, queue: queue, memberships: memberships, objects: objects, attest: attest}, mux)
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
		writeSmileSimError(w, smilesimErrInvalidRequestBody.WithCause(err))
		return
	}
	if body.PhotoObjectID == "" {
		writeSmileSimError(w, smilesimErrPhotoObjectIDRequired)
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
	// caller that omits it just polls for the result.
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
// per-photo record exists for the job (every job this app enqueued
// carries one), the answer also carries the effective options the
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
		// This poll is an observation of a succeeded output: register and
		// attest it (internal/attestation), so the output can be shared
		// through the chain-verified gate. The hook is a side channel --
		// a failure is logged and the status read still answers, exactly
		// like NotifyOnCompletion's own swallow rule above; an unattested
		// output is then refused by the sharing gate until a later
		// observation retries.
		h.ensureAttestedOutput(r.Context(), result.OutputObjectID, nil)
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
// and -- once the job succeeded -- its output object id. It is the smile
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
			// CreatedAt renders on the wire in whole-seconds UTC:
			// the stored time is UTC
			// (gorm's autoCreateTime round-trips it so), and truncating
			// the sub-second part keeps encoding/json's RFC3339Nano
			// rendering in a stable whole-seconds form.
			CreatedAt: outcome.CreatedAt.Truncate(time.Second),
		}
		if outcome.OutputObjectID != "" {
			entry.OutputObjectID = &outcome.OutputObjectID
		}
		if outcome.Error != "" {
			entry.Error = &outcome.Error
		}
		// A succeeded enumeration entry is an observation of the output it
		// names: attest it (internal/attestation) so the output is
		// shareable -- and, once a revoked certificate made it
		// unshareable, re-attestable the next time the gallery re-opens
		// this photo. The same swallow rule as the poll route's own hook.
		if outcome.Status == jobs.StatusSucceeded && outcome.OutputObjectID != "" {
			h.ensureAttestedOutput(r.Context(), outcome.OutputObjectID, nil)
		}
		simulations = append(simulations, entry)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(smilesimapi.SmilesimPhotoSimulations{Simulations: simulations})
}

// SmilesimGetSimulationContent implements smilesimapi.ServerInterface: it
// handles GET /api/v1/smile-simulation/photos/{photoObjectID}/simulations/
// {jobID}/content, serving one simulation's generated image's stored
// bytes -- base64-encoded with the media type go/storage's probe
// assigned, the same JSON-only transport the cases surface's
// photo-content route speaks (cases_photos.go) -- for the comparison view
// to render the result beside the original photo.
//
// The gate is the enumeration the list route already answers, narrowed to
// one job: ListSimulationsByPhoto resolves the tenant from the request
// context itself, so its answer can only ever name simulations of the
// caller's own tenant -- an unknown photo, or a job that photo has no
// record of under this tenant, is the same simulation_not_found refusal,
// never a probe of whether another tenant's generation exists. A
// simulation whose job has not succeeded has no image to serve
// (output_not_ready), and a succeeded one whose stored bytes no longer
// exist answers output_not_found.
func (h *smilesimHandler) SmilesimGetSimulationContent(w http.ResponseWriter, r *http.Request, photoObjectID string, jobID string) {
	outcomes, err := h.svc.ListSimulationsByPhoto(r.Context(), photoObjectID)
	if err != nil {
		writeSmileSimError(w, err)
		return
	}
	// Index scan rather than a per-id lookup: the enumeration is this
	// surface's own tenant-scoped answer for the photo, and the record
	// it carries for the job (its live status and output object, read
	// from the queue by ListSimulationsByPhoto) is exactly the outcome
	// this route needs.
	var match *smilesim.SimulationOutcome
	for i := range outcomes {
		if outcomes[i].JobID == jobs.JobID(jobID) {
			match = &outcomes[i]
			break
		}
	}
	if match == nil {
		writeSmileSimError(w, smileSimErrSimulationNotFound)
		return
	}
	if match.Status != jobs.StatusSucceeded || match.OutputObjectID == "" {
		writeSmileSimError(w, smileSimErrOutputNotReady)
		return
	}

	obj, rc, err := h.objects.OpenContent(r.Context(), match.OutputObjectID)
	if err != nil {
		if hasCasesPhotoCode(err, storage.ErrObjectNotFound.Code) {
			writeSmileSimError(w, smileSimErrOutputNotFound)
			return
		}
		writeSmileSimError(w, smileSimErrInternal.WithCause(err))
		return
	}
	defer func() { _ = rc.Close() }()

	// The serve bound mirrors the cases photo-content route's own: an
	// honest simulation image stays far below it, and a larger one is
	// refused rather than buffered in full. An over-bound read is an
	// internal surprise, not a client fact, so it folds to the internal
	// envelope with the reason in params.
	raw, err := io.ReadAll(io.LimitReader(rc, maxPhotoBytes+1))
	if err != nil {
		writeSmileSimError(w, smileSimErrInternal.WithCause(err))
		return
	}
	if int64(len(raw)) > maxPhotoBytes {
		writeSmileSimError(w, smileSimErrInternal.WithParam("reason", "simulation content exceeds the serve bound"))
		return
	}
	// Serving a succeeded output is an observation of it: attest it
	// (internal/attestation), reusing the bytes already read so the
	// digest needs no second storage open. The same swallow rule as the
	// poll and enumeration hooks -- an unattested output is refused by
	// the sharing gate until a later observation retries.
	h.ensureAttestedOutput(r.Context(), match.OutputObjectID, raw)

	mediaType := "application/octet-stream"
	if obj.MIME != nil && *obj.MIME != "" {
		mediaType = *obj.MIME
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(smilesimapi.SmilesimSimulationContent{
		MediaType:     mediaType,
		ContentBase64: base64.StdEncoding.EncodeToString(raw),
	})
}

// toSmilesimOptions converts the service's resolved option set to its
// spec-generated wire type, the shape both the job-status route's options
// echo and the enumeration route's per-entry options field share. Every
// field of the resolved set is always set, hence always present on the
// wire.
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

// ensureAttestedOutput registers and attests one succeeded simulation
// output through the app's attestation service (internal/attestation) --
// the observation hook every surface that makes an output visible to its
// own tenant runs (see the three call sites). content, when non-nil, is
// the output's bytes the caller already read (the content route passes
// them so the digest needs no second storage open); nil makes the
// service open the object itself.
//
// The hook is durable bookkeeping on a job that is already terminal, so
// it runs on a cancel-free derivation of ctx -- the same
// context.WithoutCancel boundary NotifyOnCompletion draws -- and its
// failure is logged and swallowed: the status/content/enumeration read
// that drove the observation must never turn into an error response over
// this side channel. A failed attestation leaves the output refused by
// the sharing gate (an unattested output is not shareable), and the very
// next observation of the output retries it -- the same retry-by-later-
// observation shape NotifyOnCompletion's rollback latch provides for
// completion notifications.
func (h *smilesimHandler) ensureAttestedOutput(ctx context.Context, outputObjectID string, content []byte) {
	if h.attest == nil || outputObjectID == "" {
		return
	}
	persistCtx := context.WithoutCancel(ctx)
	var err error
	if content != nil {
		err = h.attest.EnsureAttestedContent(persistCtx, outputObjectID, content)
	} else {
		err = h.attest.EnsureAttested(persistCtx, outputObjectID)
	}
	if err != nil {
		observability.FromContext(ctx).Error("smilesim: attesting a succeeded simulation output failed -- the output is not shareable until a later observation attests it",
			"output_object_id", outputObjectID,
			"error", err,
		)
	}
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
	// not the envelope shape the API documents for a parameter-less
	// answer.
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}
