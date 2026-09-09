package main

import (
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// clinic_name.go mounts the reference-app's own tenant-identity answer:
// the NAME of the tenant the signed-in caller's token is scoped to,
// served as the org root name of that tenant (org's row for "what is
// this clinic called" -- the self-service provisioner names a clinic's
// root after the name its registrant gave at registration,
// self_service.go's clinicRootNameFor, and a tenant renames the node the
// moment the name means something else to them).
//
// Why this route exists at all, and why it lives HERE: the demo-host
// web's tenant switcher and clinic-context line name the current tenant
// from the app's own static copy for the two boot-configured demo
// tenants (src/demo-tenants.ts), and a tenant that did not exist at boot
// -- the clinic a self-service registration created -- has no such copy.
// The host answers that tenant's name from its own org rows through this
// route, its own composition surface, mounted beside the other
// hand-written app routes (cases, smilesim): it is not part of any
// module's OpenAPI fragment, nor of the app's own three fragments --
// platform module fragments generate into @speed/api-sdk and the app's
// own fragments into the app-owned SDK (src/app-api), while this
// app-owned answer is in neither: it simply belongs to the host,
// exactly like /api/config/public's host-side resolution. The web
// reaches it through the app's own api-client RequestFn
// (src/tenant-name.ts), the same transport every other app request
// rides.
//
// The route sits behind authn.Middleware and tenancy.Middleware like
// every non-allowlisted route: an anonymous caller is refused before
// this handler runs (tenancy.tenant_unresolved), and the tenant comes
// from the access token's claims in the request context, never from a
// parameter, header or body. The answer is deliberately small -- one
// name -- because that is the one question the frame chrome and the
// work-area clinic line ask about a tenant they have no copy for.
const clinicNamePath = "/api/reference-app/clinic-name"

// clinicNameResponse is the answer's wire shape: name is the current
// tenant's org root name, and the empty string when the tenant has no
// org tree to name it with (a context no org row can answer -- the web
// renders nothing rather than inventing an identifier).
type clinicNameResponse struct {
	Name string `json:"name"`
}

// clinicNameError is the answer's failure shape, the same code-plus-
// params envelope every module's generated handlers write.
type clinicNameError struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// writeClinicNameError writes err as the envelope answer.
func writeClinicNameError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = apperr.Internal("reference_app.internal_error")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(clinicNameError{Code: appErr.Code, Params: appErr.Params})
}

// wireClinicName mounts the clinic-name answer on mux. tree is the org
// TreeService whose Root lookup answers the name -- the same service
// every other org-backed surface in this app drives, never a raw read.
func wireClinicName(mux *http.ServeMux, tree *org.TreeService) {
	mux.HandleFunc(clinicNamePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeClinicNameError(w, apperr.Invalid("reference_app.method_not_allowed"))
			return
		}
		root, err := tree.Root(r.Context())
		switch {
		case err == nil:
			// A tenant with a tree answers its root's name; the empty
			// name is reserved for the no-tree case below.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(clinicNameResponse{Name: root.Name})
		case orgCodeIs(err, org.ErrNodeNotFound.Code):
			// The tenant context is real but no org tree exists in it
			// yet (a tenant between provisioning steps, or a context
			// that is not a customer tenant at all). There is no name to
			// serve -- never an identifier invented to fill the slot.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(clinicNameResponse{Name: ""})
		default:
			// A genuine failure (a missing tenant context, a storage
			// error) is a coded error, never a fabricated empty name:
			// the caller must not read "unnamed" for "unanswerable".
			writeClinicNameError(w, err)
		}
	})
}
