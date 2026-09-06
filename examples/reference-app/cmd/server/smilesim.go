// The reference app's demo glue for go/ai-gateway round 2: the hand-written
// routes that demonstrate the module's Gateway.GenerateImage end to end.
// Round 2 itself shipped two (the enqueue route and the job-status route);
// the P2a parameterization round added an options field to the enqueue
// route's body, an options echo on the job-status route, and a third route
// -- the per-photo enumeration that is the P3 gallery's data source.
// smilesim_flow_test.go drives them through the composed HTTP stack against
// an httptest.Server standing in for the OpenAI-compatible images endpoint.
//
// ai-gateway itself ships no HTTP surface for image generation either (see
// go/ai-gateway/AGENTS.md's "What this round ships" section), so there is
// no spec fragment for these routes to grow into -- they are mounted by
// hand, outside the OpenAPI machinery, exactly like consult.go's own route
// for round 1's chat surface. Like that route, all three are deliberately
// outside demoRouteGuards' table too: they are mounted directly on mux
// rather than through reg.Routes/mountModuleRoutes, so none needs (and
// cannot silently skip) an entry there.
package main

import (
	"encoding/json"
	"net/http"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

// smileSimulateRequestBody is the enqueue route's JSON body: an existing,
// completed go/storage object of the caller's own tenant, an optional
// recipient to notify on completion, and an optional options object naming
// the parameterized option set the simulation is generated with
// (smile_style, tooth_shade, strength). When options is absent or empty,
// the service's documented defaults apply.
type smileSimulateRequestBody struct {
	PhotoObjectID   string                    `json:"photo_object_id"`
	RecipientUserID string                    `json:"recipient_user_id"`
	Options         *smileSimulateBodyOptions `json:"options"`
}

// smileSimulateBodyOptions is the wire shape of one simulation's option
// set. Every field is a pointer so "omitted" and "explicitly the zero
// value" stay distinguishable at the boundary: an omitted field inherits
// the service's documented default, while an explicitly present field --
// including a strength of 0, which is out of range -- is passed through
// verbatim and refused by the service's own validation with a coded
// smilesim.* error rather than being silently replaced by a default.
type smileSimulateBodyOptions struct {
	SmileStyle *smilesim.SmileStyle `json:"smile_style"`
	ToothShade *smilesim.ToothShade `json:"tooth_shade"`
	Strength   *float64             `json:"strength"`
}

// smileSimulatePath is the smile-simulation module's enqueue route. POST
// smileSimulatePath with a JSON smileSimulateRequestBody, and the response
// carries the async job's id -- the simulation itself has not run yet.
const smileSimulatePath = "/api/v1/smile-simulation/simulate"

// smileJobPathPrefix is the fixed prefix of the smile-simulation module's
// job-status route: GET smileJobPathPrefix+"{id}" polls the same
// jobs.Queue this app shares with go/ai-gateway, and the response carries
// the job's current status -- plus, once it has succeeded, the generated
// image's go/storage object id and the real vendor usage the job recorded.
// When a durable per-photo record exists for the job (every job this app
// enqueued since the P2a round), the response also carries the effective
// options the simulation was generated with.
const smileJobPathPrefix = "/api/v1/smile-simulation/jobs/"

// smilePhotoSimulationsPath is the smile-simulation module's per-photo
// enumeration route: GET it with a photo object id in place of
// "{photoObjectID}" lists every simulation generated from that photo under
// the caller's tenant, newest first, each entry carrying its options, its
// live status and -- once the job succeeded -- its output object id. It is
// the P3 gallery's data source (see internal/smilesim's package doc
// comment's "Per-photo result index" section).
const smilePhotoSimulationsPath = "/api/v1/smile-simulation/photos/{photoObjectID}/simulations"

// smileSimErrInternal folds any error this file's handlers surface that is
// not itself an *apperr.Error into a stable code, the same fallback
// consult.go's own writeConsultError applies.
var smileSimErrInternal = apperr.Internal("smilesim.internal_error")

// wireSmileSim mounts smileSimulatePath, smileJobPathPrefix+"{id}" and
// smilePhotoSimulationsPath on mux, backed by svc and queue.
//
// Like consultSuggestPath, none of these routes takes a subject or checks a
// permission of its own: in this app every authenticated member of a
// tenant may request a simulation, and the tenant scoping that actually
// protects another tenant's photo -- go/storage's own ObjectService.
// OpenContent, read inside the job handler from the job's own rebuilt
// tenant context -- another tenant's job id -- go/jobs' own Queue.Get,
// which reports ErrJobNotFound for an id outside ctx's tenant,
// indistinguishable from an unknown one -- and another tenant's simulation
// rows -- svc.ListSimulationsByPhoto/OptionsForJob, both tenant-scoped the
// same way -- are what actually gate access.
func wireSmileSim(mux *http.ServeMux, svc *smilesim.Service, queue jobs.Queue) {
	mux.HandleFunc(http.MethodPost+" "+smileSimulatePath, func(w http.ResponseWriter, r *http.Request) {
		var body smileSimulateRequestBody
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
				simOptions = append(simOptions, smilesim.WithSmileStyle(*body.Options.SmileStyle))
			}
			if body.Options.ToothShade != nil {
				simOptions = append(simOptions, smilesim.WithToothShade(*body.Options.ToothShade))
			}
			if body.Options.Strength != nil {
				simOptions = append(simOptions, smilesim.WithStrength(*body.Options.Strength))
			}
		}

		// RecipientUserID is optional: a caller that supplies one gets an
		// EventSimulationCompleted notification once the job finishes
		// (svc.NotifyOnCompletion, called from the job-status route
		// below); a caller that omits it just polls for the result, same
		// as before this field existed.
		jobID, err := svc.Simulate(r.Context(), body.PhotoObjectID, body.RecipientUserID, simOptions...)
		if err != nil {
			writeSmileSimError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": string(jobID)})
	})

	mux.HandleFunc(http.MethodGet+" "+smileJobPathPrefix+"{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeSmileSimError(w, apperr.Invalid("smilesim.job_id_required"))
			return
		}

		job, err := queue.Get(r.Context(), jobs.JobID(id))
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
		if notifyErr := svc.NotifyOnCompletion(r.Context(), job); notifyErr != nil {
			observability.FromContext(r.Context()).Warn("smilesim completion notification failed",
				"job_id", id, "error", notifyErr)
		}

		resp := map[string]any{"status": string(job.Status)}

		// The effective options this job was generated with, when a
		// durable per-photo record exists for it (see the package doc
		// comment's "Per-photo result index" section). Attaching them is a
		// secondary convenience of the status read: a store failure is
		// logged and the options are simply omitted, never allowed to turn
		// an otherwise-successful status read into an error response --
		// the same swallow rule as the notification call just above.
		if opts, has, optsErr := svc.OptionsForJob(r.Context(), job.ID); optsErr != nil {
			observability.FromContext(r.Context()).Warn("smilesim reading recorded options failed, omitting them from the status response",
				"job_id", id, "error", optsErr)
		} else if has {
			resp["options"] = opts
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
			resp["output_object_id"] = result.OutputObjectID
			resp["usage"] = map[string]any{
				"image_count":     result.Usage.ImageCount,
				"steps":           result.Usage.Steps,
				"resolution_tier": result.Usage.ResolutionTier,
			}
		case jobs.StatusDeadLetter, jobs.StatusCancelled, jobs.StatusRetrying:
			resp["error"] = job.Error
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc(http.MethodGet+" "+smilePhotoSimulationsPath, func(w http.ResponseWriter, r *http.Request) {
		photoObjectID := r.PathValue("photoObjectID")
		if photoObjectID == "" {
			writeSmileSimError(w, apperr.Invalid("smilesim.photo_object_id_required"))
			return
		}

		outcomes, err := svc.ListSimulationsByPhoto(r.Context(), photoObjectID)
		if err != nil {
			writeSmileSimError(w, err)
			return
		}

		simulations := make([]map[string]any, 0, len(outcomes))
		for _, outcome := range outcomes {
			entry := map[string]any{
				"job_id":          string(outcome.JobID),
				"photo_object_id": outcome.PhotoObjectID,
				"options":         outcome.Options,
				"status":          string(outcome.Status),
				"created_at":      outcome.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			}
			if outcome.OutputObjectID != "" {
				entry["output_object_id"] = outcome.OutputObjectID
			}
			if outcome.Error != "" {
				entry["error"] = outcome.Error
			}
			simulations = append(simulations, entry)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"simulations": simulations})
	})
}

// writeSmileSimError writes err to w as a JSON {code, params} body, the
// same structured-error envelope shape consult.go's own writeConsultError
// produces.
func writeSmileSimError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = smileSimErrInternal
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	envelope := map[string]any{"code": appErr.Code}
	if appErr.Params != nil {
		envelope["params"] = appErr.Params
	}
	_ = json.NewEncoder(w).Encode(envelope)
}
