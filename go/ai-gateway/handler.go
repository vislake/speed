package aigateway

import (
	"net/http"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/httpapi"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/go/ai-gateway/api"
)

// handlerSystemActor is the pkgcore.SystemReason.Actor
// AiGatewaySetPlatformCredential attributes its audited system context to.
// A fixed string rather than a caller-derived identity: this module
// resolves callers only as far as rbac's router-level permission gate --
// which never imports authn and carries no user id of its own -- so there
// is no richer identity available here to attribute the reason to without
// this module importing authn, which its own dependency boundary does not
// permit. The audit value that matters here is WHICH PATH performed the
// write, not a per-request caller identity a system context was never
// designed to carry (pkgcore.SystemReason.Actor's own doc comment).
const handlerSystemActor = "ai-gateway-http"

// Handler serves ai-gateway's HTTP endpoints by implementing the
// spec-generated api.ServerInterface (api/ai-gateway-server.gen.go,
// regenerated from this module's api/openapi.yaml by task api:gen -- the
// compile-time assertion at the bottom of this file is what makes "spec
// changed, handler not" a compile failure instead of a runtime surprise).
// The three operations it implements are the whole surface the spec
// defines: aiGateway_getCredential (read which scope currently answers for
// a provider), aiGateway_setTenantCredential (write the caller's own
// tenant's BYOK credential) and aiGateway_setPlatformCredential (write the
// platform-wide default).
//
// It performs no business logic of its own beyond decoding the request and
// encoding the response: every operation is answered entirely by the
// CredentialService methods it delegates to (Resolve / SetTenantCredential
// / SetPlatformCredential). The one exception -- and it is translation,
// not a new decision -- is aiGateway_setPlatformCredential building the
// audited system-context reason SetPlatformCredential's own contract
// requires (tenancy.WithSystemContext, the audited wrapper, under
// SystemPurposeCredentialWrite): nothing upstream of this HTTP handler is
// positioned to build it, since the module's own SystemPurpose is what the
// reason must carry and rbac's router-level permission gate (which the
// host wires, never this package) is what actually decides whether the
// caller may reach this operation at all -- the gate and the
// system-context reason answer two different questions.
//
// The two write operations never echo the API key back on any response:
// no operation of this surface returns key material.
//
// It must run downstream of tenancy.Middleware on a non-allowlisted path:
// aiGateway_setTenantCredential and aiGateway_getCredential resolve the
// caller's tenant from whatever context CredentialService's own methods
// read it from (never from a request parameter, header or body) -- there
// is no tenant_id anywhere on this surface.
type Handler struct {
	credentials *CredentialService

	// bus is the pkgcore.EventBus AiGatewaySetPlatformCredential publishes
	// its audited system-context grant on, through tenancy.WithSystemContext
	// (see that method's own doc comment): the platform-wide credential
	// write is a privileged, platform-row write that must never happen
	// unrecorded, and tenancy's wrapper fails the whole call closed if the
	// audit publish fails. Module.Register supplies the registry's resolved
	// bus (reg.EventBus()); a Handler with no bus cannot serve the
	// platform-credential operation at all, since building its system
	// context would then have nowhere to publish the audit event.
	bus pkgcore.EventBus

	mux *http.ServeMux
}

// NewHandler returns a Handler serving reads and writes of ai-gateway's
// platform and tenant BYOK credentials through the given CredentialService
// -- the instance Module.Register mounts it behind, in the same call that
// attaches it at apiPath. bus is the event bus the platform-credential
// operation publishes its audited system-context grant on (see Handler's
// bus field); Module.Register passes the registry's own resolved bus. The
// returned Handler's routing is registered by the generated
// api.HandlerFromMux helper: it derives this module's method+path patterns
// from the "paths:" keys of api/openapi.yaml itself, exactly as storage's
// and org's NewHandler do for their own fragments.
func NewHandler(credentials *CredentialService, bus pkgcore.EventBus) *Handler {
	h := &Handler{credentials: credentials, bus: bus}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// decodeJSON decodes r's body into dst, writing ErrCredentialRequired and
// reporting false on any decode failure (see pkgcore/httpapi's DecodeJSON;
// the body carries no size bound). Both write operations call it.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return httpapi.DecodeJSON(w, r, 0, dst, ErrCredentialRequired)
}

// baseURLOf returns the request's optional baseUrl field, or the empty
// string when it was omitted -- CredentialService's own BaseURL parameter
// is a plain string with "" meaning "none configured here" (credentialRow
// .BaseURL's own doc comment), so a nil pointer and an explicit empty
// string collapse to the identical call either way.
func baseURLOf(req *api.AiGatewaySetCredentialRequest) string {
	if req.BaseURL == nil {
		return ""
	}
	return *req.BaseURL
}

// toCredentialResponse renders provider/scope/baseURL as the spec's
// AiGatewayCredential -- never the api key, on any of the three
// operations.
func toCredentialResponse(provider string, scope CredentialScope, baseURL string) api.AiGatewayCredential {
	resp := api.AiGatewayCredential{
		Provider: provider,
		Scope:    string(scope),
	}
	if baseURL != "" {
		resp.BaseURL = &baseURL
	}
	return resp
}

// AiGatewayGetCredential implements api.ServerInterface: GET
// /api/v1/ai-gateway/credentials/{provider}. Reports whichever scope
// CredentialService.Resolve actually answers with for provider under the
// caller's own request context -- the tenant's own BYOK row when one
// exists, the platform-wide row otherwise -- and never the api key.
func (h *Handler) AiGatewayGetCredential(w http.ResponseWriter, r *http.Request, provider api.Provider) {
	ctx := r.Context()
	observability.AnnotateTenant(ctx)

	cred, err := h.credentials.Resolve(ctx, provider)
	if err != nil {
		writeError(w, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, toCredentialResponse(cred.Provider, cred.Scope, cred.BaseURL))
}

// AiGatewaySetTenantCredential implements api.ServerInterface: PUT
// /api/v1/ai-gateway/credentials/{provider}/tenant. Writes the caller's
// own tenant's BYOK credential for provider through
// CredentialService.SetTenantCredential verbatim -- the owning tenant
// comes from the request's own context, never from a request parameter,
// header or body.
func (h *Handler) AiGatewaySetTenantCredential(w http.ResponseWriter, r *http.Request, provider api.Provider) {
	ctx := r.Context()
	observability.AnnotateTenant(ctx)

	var req api.AiGatewaySetCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	baseURL := baseURLOf(&req)
	if err := h.credentials.SetTenantCredential(ctx, provider, req.APIKey, baseURL); err != nil {
		writeError(w, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, toCredentialResponse(provider, CredentialScopeTenant, baseURL))
}

// AiGatewaySetPlatformCredential implements api.ServerInterface: PUT
// /api/v1/ai-gateway/credentials/{provider}/platform. Writes the
// platform-wide default credential for provider through
// CredentialService.SetPlatformCredential verbatim, after building the
// audited system-context reason that method's own contract requires --
// see this type's own doc comment for why building that reason lives here
// rather than upstream. This operation is gated by the host's router-level
// permission check on PermissionManagePlatform, a distinct and more
// restrictive permission than aiGateway_setTenantCredential's
// PermissionWrite (see module.go's own doc comment on both constants) --
// this handler itself performs no permission check: enforcement is the
// host's router gate, never the handler.
func (h *Handler) AiGatewaySetPlatformCredential(w http.ResponseWriter, r *http.Request, provider api.Provider) {
	ctx := r.Context()
	observability.AnnotateTenant(ctx)

	var req api.AiGatewaySetCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if h.bus == nil {
		// Fail closed before granting anything rather than panic mid-publish
		// on a nil bus: NewHandler's contract says bus is never nil
		// (Module.Register passes the registry's resolved bus), but a host
		// wiring the handler by hand could get it wrong, and an escape
		// hatch granted with its audit publish about to panic is exactly
		// the unrecorded-grant gap this path exists to close.
		writeError(w, ErrInternal.WithParam("reason", "handler has no event bus for its audited system-context grant"))
		return
	}
	sysCtx, err := tenancy.WithSystemContext(ctx, h.bus, pkgcore.SystemReason{
		Actor:   handlerSystemActor,
		Purpose: SystemPurposeCredentialWrite,
	})
	if err != nil {
		// Both failure modes -- pkgcore refusing the reason (unreachable in
		// practice: Module.Register always registers
		// SystemPurposeCredentialWrite before any request can reach this
		// handler) and the audit publish itself failing, which
		// tenancy.WithSystemContext reports as ErrAuditPublishFailed and
		// fails closed so no unrecorded grant ever proceeds -- are handled
		// the same way rather than assumed away, per this file's own
		// ErrInternal doc comment.
		writeError(w, ErrInternal.WithCause(err))
		return
	}
	baseURL := baseURLOf(&req)
	if err := h.credentials.SetPlatformCredential(sysCtx, provider, req.APIKey, baseURL); err != nil {
		writeError(w, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, toCredentialResponse(provider, CredentialScopeSystem, baseURL))
}

// writeError writes err to w as the coded error envelope (see
// pkgcore/httpapi): an *apperr.Error keeps its own code and status,
// anything else -- something below this handler did not classify it -- is
// folded into ErrInternal so a caller never sees raw Go error text either
// way.
func writeError(w http.ResponseWriter, err error) {
	httpapi.WriteError(w, err, ErrInternal)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow: add an operation to the fragment, regenerate, and
// this assertion stops compiling until Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
