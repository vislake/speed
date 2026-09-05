// The reference app's demo glue for go/ai-gateway round 2: the two
// hand-written routes that demonstrate the module's Gateway.GenerateImage
// end to end. smilesim_flow_test.go drives them through the composed HTTP
// stack against an httptest.Server standing in for the OpenAI-compatible
// images endpoint.
//
// ai-gateway itself ships no HTTP surface for image generation either (see
// go/ai-gateway/AGENTS.md's "What this round ships" section), so there is
// no spec fragment for these routes to grow into -- they are mounted by
// hand, outside the OpenAPI machinery, exactly like consult.go's own route
// for round 1's chat surface. Like that route, both are deliberately
// outside demoRouteGuards' table too: they are mounted directly on mux
// rather than through reg.Routes/mountModuleRoutes, so neither needs (and
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

// smileSimulatePath is the smile-simulation module's enqueue route: POST it
// with a JSON body naming an existing, completed go/storage object of the
// caller's own tenant ({"photo_object_id": "..."}), and the response
// carries the async job's id -- the simulation itself has not run yet.
const smileSimulatePath = "/api/v1/smile-simulation/simulate"

// smileJobPathPrefix is the fixed prefix of the smile-simulation module's
// job-status route: GET smileJobPathPrefix+"{id}" polls the same
// jobs.Queue this app shares with go/ai-gateway, and the response carries
// the job's current status -- plus, once it has succeeded, the generated
// image's go/storage object id and the real vendor usage the job recorded.
const smileJobPathPrefix = "/api/v1/smile-simulation/jobs/"

// smileSimErrInternal folds any error this file's handlers surface that is
// not itself an *apperr.Error into a stable code, the same fallback
// consult.go's own writeConsultError applies.
var smileSimErrInternal = apperr.Internal("smilesim.internal_error")

// wireSmileSim mounts smileSimulatePath and smileJobPathPrefix+"{id}" on
// mux, backed by svc and queue.
//
// Like consultSuggestPath, neither route takes a subject or checks a
// permission of its own: in this app every authenticated member of a
// tenant may request a simulation, and the tenant scoping that actually
// protects another tenant's photo -- go/storage's own ObjectService.
// OpenContent, read inside the job handler from the job's own rebuilt
// tenant context -- and another tenant's job id -- go/jobs' own
// Queue.Get, which reports ErrJobNotFound for an id outside ctx's tenant,
// indistinguishable from an unknown one -- are what actually gate access.
func wireSmileSim(mux *http.ServeMux, svc *smilesim.Service, queue jobs.Queue) {
	mux.HandleFunc(http.MethodPost+" "+smileSimulatePath, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PhotoObjectID   string `json:"photo_object_id"`
			RecipientUserID string `json:"recipient_user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeSmileSimError(w, apperr.Invalid("smilesim.invalid_request_body").WithCause(err))
			return
		}
		if body.PhotoObjectID == "" {
			writeSmileSimError(w, apperr.Invalid("smilesim.photo_object_id_required"))
			return
		}

		// RecipientUserID is optional: a caller that supplies one gets an
		// EventSimulationCompleted notification once the job finishes
		// (svc.NotifyOnCompletion, called from the job-status route
		// below); a caller that omits it just polls for the result, same
		// as before this field existed.
		jobID, err := svc.Simulate(r.Context(), body.PhotoObjectID, body.RecipientUserID)
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
