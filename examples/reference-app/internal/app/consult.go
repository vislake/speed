// The reference app's demo glue for go/ai-gateway: the one hand-written
// route that demonstrates the module's Gateway.Chat end to end.
// flowtests/consult_flow_test.go drives it through the composed HTTP stack against an
// httptest.Server standing in for the OpenAI-compatible endpoint.
//
// ai-gateway itself ships no HTTP surface for chat, so there is no spec
// fragment for this route to live in -- it is mounted by hand, outside
// the OpenAPI machinery, the same pattern internal/app/demo/demo_notification.go's own demo
// patient-message route establishes in this app. Like that route,
// it is deliberately outside DemoRouteRules' table too: it is mounted by
// hand on the protected mux (composeFace, routes.go) rather than through
// the registry, so it never needs (and cannot silently skip) an entry
// there.

package app

import (
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/httpapi"

	"github.com/vislake/speed/examples/reference-app/internal/consult"
)

// ConsultSuggestPath is the consult module's one hand-written route: POST
// it with a JSON body naming an existing note of the caller's own tenant
// ({"note_id": "..."}), and the response carries a short AI-generated
// consultation-suggestion summary of that note's text.
const ConsultSuggestPath = "/api/v1/consult/suggest"

// consultErrInternal folds any error consult.Service.Suggest returns that
// is not itself an *apperr.Error into a stable code, the fallback
// writeConsultError applies -- a caller never sees raw Go error text
// either way. dbkit.ErrRecordNotFound (an unknown or another tenant's note
// id) and aigateway's own sentinels (ErrCredentialNotFound,
// ErrUnroutedModel, ErrEntitlementDenied, ...) are already *apperr.Error
// values, so the fallback only ever catches a genuinely unclassified
// failure -- for example a raw transport error the OpenAI-compatible
// provider did not itself wrap.
var consultErrInternal = apperr.Internal("consult.internal_error")

// consultMaxRequestBodyBytes bounds the suggest route's request body
// BEFORE it is decoded, the same 64 KiB notes' own maxRequestBodyBytes and
// the cases surface's casesMaxRequestBodyBytes use: the body is one JSON
// object naming a note id, and without the bound it would feed an
// unbounded json.Decoder before any validation had a chance to refuse it.
// A body any legitimate request can produce stays far below the bound.
const consultMaxRequestBodyBytes = 1 << 16

// wireConsult mounts ConsultSuggestPath on mux, backed by svc.
//
// The route takes no subject and checks no permission of its own -- the
// identical choice internal/app/demo/demo_notification.go's own demo patient-message route
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
	mux.HandleFunc(http.MethodPost+" "+ConsultSuggestPath, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NoteID string `json:"note_id"`
		}
		if !httpapi.DecodeJSON(w, r, consultMaxRequestBodyBytes, &body, apperr.Invalid("consult.invalid_request_body")) {
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

// writeConsultError writes err to w as this route's coded error envelope
// (see pkgcore/httpapi): an *apperr.Error keeps its own code and status,
// anything else is folded into consultErrInternal so a caller never sees
// raw Go error text either way.
func writeConsultError(w http.ResponseWriter, err error) {
	httpapi.WriteError(w, err, consultErrInternal)
}
