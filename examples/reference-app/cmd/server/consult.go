// The reference app's demo glue for go/ai-gateway: the one hand-written
// route that demonstrates the module's Gateway.Chat end to end.
// consult_flow_test.go drives it through the composed HTTP stack against an
// httptest.Server standing in for the OpenAI-compatible endpoint.
//
// ai-gateway itself ships no HTTP surface this round (go/ai-gateway/
// AGENTS.md's "What this round ships" section), so there is no spec
// fragment for this route to grow into -- it is mounted by hand, outside
// the OpenAPI machinery, the same pattern demo_notification.go's own demo
// patient-message route already establishes in this app. Like that route,
// it is deliberately outside demoRouteGuards' table too: it is mounted
// directly on mux rather than through reg.Routes/mountModuleRoutes, so it
// never needs (and cannot silently skip) an entry there.
package main

import (
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/examples/reference-app/internal/consult"
)

// consultSuggestPath is the consult module's one hand-written route: POST
// it with a JSON body naming an existing note of the caller's own tenant
// ({"note_id": "..."}), and the response carries a short AI-generated
// consultation-suggestion summary of that note's text.
const consultSuggestPath = "/api/v1/consult/suggest"

// consultErrInternal folds any error consult.Service.Suggest returns that
// is not itself an *apperr.Error into a stable code, the same fallback
// notes' own writeError applies (internal/notes/handler.go) -- a caller
// never sees raw Go error text either way.
var consultErrInternal = apperr.Internal("consult.internal_error")

// wireConsult mounts consultSuggestPath on mux, backed by svc.
//
// The route takes no subject and checks no permission of its own -- the
// identical choice demo_notification.go's own demo patient-message route
// makes, and its doc comment's reasoning applies here unchanged: in this
// app every authenticated member of a tenant may ask for a consultation
// suggestion, and svc.Suggest's own tenant scoping (through
// dbkit.Repository[Note], reading the tenant tenancy.Middleware already
// resolved into the request context) is what actually protects another
// tenant's notes from being read here -- an unknown or another tenant's
// note id answers exactly like an unknown one, never a cross-tenant leak.
// A real deployment would gate its own trigger route however its
// permission model requires; the Suggest call itself is all ai-gateway
// asks of it.
func wireConsult(mux *http.ServeMux, svc *consult.Service) {
	mux.HandleFunc(http.MethodPost+" "+consultSuggestPath, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NoteID string `json:"note_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeConsultError(w, apperr.Invalid("consult.invalid_request_body").WithCause(err))
			return
		}
		if body.NoteID == "" {
			writeConsultError(w, apperr.Invalid("consult.note_id_required"))
			return
		}

		suggestion, err := svc.Suggest(r.Context(), body.NoteID)
		if err != nil {
			writeConsultError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"suggestion": suggestion})
	})
}

// writeConsultError writes err to w as a JSON {code, params} body, the same
// structured-error envelope shape notes' own writeError produces
// (internal/notes/handler.go) -- a stable code plus structured parameters,
// never localized text (backend coding standard §6.2).
func writeConsultError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		// dbkit.ErrRecordNotFound (an unknown or another tenant's note id)
		// and aigateway's own sentinels (ErrCredentialNotFound,
		// ErrUnroutedModel, ErrEntitlementDenied, ...) are already
		// *apperr.Error values, so this fallback only ever catches a
		// genuinely unclassified failure -- for example a raw transport
		// error the OpenAI-compatible provider did not itself wrap.
		appErr = consultErrInternal
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	envelope := map[string]any{"code": appErr.Code}
	if appErr.Params != nil {
		envelope["params"] = appErr.Params
	}
	_ = json.NewEncoder(w).Encode(envelope)
}
