package aigateway

import (
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/ai-gateway/api"
)

// jsonContentType is the Content-Type every response below writes, matching
// storage's, notes' and org's own handler constant of the same name.
const jsonContentType = "application/json; charset=utf-8"

// handlerSystemActor is the pkgcore.SystemReason.Actor
// AiGatewaySetPlatformCredential attributes its audited system context to.
// A fixed string rather than a caller-derived identity: this module
// resolves callers only as far as rbac's router-level permission gate --
// which never imports authn and carries no user id of its own -- so there
// is no richer identity available here to attribute the reason to without
// this module importing authn, which its own dependency-boundary
// discipline (go/ai-gateway/AGENTS.md; root CLAUDE.md's module-boundary
// rule) does not permit. It mirrors the identical fixed-Actor shape the
// reference app's own boot-time platform-credential write already uses
// ("reference-app-boot" in cmd/server/server.go) for the same reason: the
// audit value that matters here is WHICH PATH performed the write, not a
// per-request caller identity a system context was never designed to
// carry (pkgcore.SystemReason.Actor's own doc comment).
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
// EXISTING CredentialService methods (Resolve / SetTenantCredential /
// SetPlatformCredential) this module's earlier rounds already shipped and
// tested. The one exception -- and it is translation, not a new decision
// -- is aiGateway_setPlatformCredential building the audited system-context
// reason SetPlatformCredential's own contract requires
// (pkgcore.WithSystemContext under SystemPurposeCredentialWrite): nothing
// upstream of this HTTP handler is positioned to build it, since the
// module's own SystemPurpose is what the reason must carry and rbac's
// router-level permission gate (which the host wires, never this package)
// is what actually decides whether the caller may reach this operation at
// all -- the gate and the system-context reason answer two different
// questions, exactly as go/config's identical ScopeSystem write rule and
// this module's own SetPlatformCredential doc comment already document.
//
// The two write operations never echo the API key back on any response --
// see the spec fragment's own header for the write-only rule this
// module's AGENTS.md and root CLAUDE.md's Security rules require.
//
// It must run downstream of tenancy.Middleware on a non-allowlisted path,
// exactly like storage's identical handler: aiGateway_setTenantCredential
// and aiGateway_getCredential resolve the caller's tenant from whatever
// context CredentialService's own methods read it from (never from a
// request parameter, header or body, per root CLAUDE.md's multi-tenant
// isolation rule) -- there is no tenant_id anywhere on this surface,
// exactly as the spec's own header records.
type Handler struct {
	credentials *CredentialService
	mux         *http.ServeMux
}

// NewHandler returns a Handler serving reads and writes of ai-gateway's
// platform and tenant BYOK credentials through the given CredentialService
// -- the instance Module.Register mounts it behind, in the same call that
// attaches it at apiPath. The returned Handler's routing is registered by
// the generated api.HandlerFromMux helper: it derives this module's
// method+path patterns from the "paths:" keys of api/openapi.yaml itself,
// exactly as storage's and org's NewHandler do for their own fragments.
func NewHandler(credentials *CredentialService) *Handler {
	h := &Handler{credentials: credentials}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// decodeJSON decodes r's body into dst, writing ErrInvalidRequestBody-
// equivalent feedback and reporting false on any decode failure. Both
// write operations call it.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, ErrCredentialRequired.WithCause(err))
		return false
	}
	return true
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
	writeJSON(w, http.StatusOK, toCredentialResponse(cred.Provider, cred.Scope, cred.BaseURL))
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
	writeJSON(w, http.StatusOK, toCredentialResponse(provider, CredentialScopeTenant, baseURL))
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
// this handler itself performs no permission check, exactly like every
// other module's handler in this codebase (storage's and org's own
// doc comments make the identical point: enforcement is the host's router
// gate, never the handler).
func (h *Handler) AiGatewaySetPlatformCredential(w http.ResponseWriter, r *http.Request, provider api.Provider) {
	ctx := r.Context()
	observability.AnnotateTenant(ctx)

	var req api.AiGatewaySetCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   handlerSystemActor,
		Purpose: SystemPurposeCredentialWrite,
	})
	if err != nil {
		// Unreachable in practice -- Module.Register always registers
		// SystemPurposeCredentialWrite before any request can reach this
		// handler -- but handled anyway rather than assumed away, per
		// this file's own ErrInternal doc comment.
		writeError(w, ErrInternal.WithCause(err))
		return
	}
	baseURL := baseURLOf(&req)
	if err := h.credentials.SetPlatformCredential(sysCtx, provider, req.APIKey, baseURL); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCredentialResponse(provider, CredentialScopeSystem, baseURL))
}

// writeJSON writes v to w as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes err to w as a JSON {code, params} body -- the
// spec-generated api.AiGatewayError, the same structured-error envelope
// storage's, notes' and org's own writeError produce. An err that is not
// an *apperr.Error -- meaning something below this handler did not
// classify it -- is folded into ErrInternal so a caller never sees raw Go
// error text.
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal
	}
	envelope := api.AiGatewayError{Code: &appErr.Code}
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow (docs/internal/21-api-contract.md): add an operation
// to the fragment, regenerate, and this assertion stops compiling until
// Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
