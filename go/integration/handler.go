package integration

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/integration/api"
)

// jsonContentType is the Content-Type every response below writes, matching
// notes', org's, storage's and notification's own handler constant of the
// same name.
const jsonContentType = "application/json; charset=utf-8"

// Handler serves round 1's API-key HTTP surface by implementing the
// spec-generated api.ServerInterface (see api/integration-server.gen.go,
// regenerated from this module's api/openapi.yaml by task api:gen -- the
// compile-time assertion at the bottom of this file is what makes "spec
// changed, handler not" a compile failure instead of a runtime surprise).
//
// It must run downstream of tenancy.Middleware on a non-allowlisted path:
// every method reads the tenant tenancy.Middleware already resolved into the
// request context, and never from a request parameter, header or body, per
// root CLAUDE0's multi-tenant isolation rule -- Service's own methods
// (Create, List) re-derive the tenant themselves through
// pkgcore.MustTenantFromContext, so Handler adds no tenant resolution of its
// own beyond the observability annotation mustTenant below performs.
//
// # Round-1-only, and built differently from every other module's Handler
//
// Unlike org's, storage's and notification's Handler -- each built once, at
// NewModule/Register time, directly from concrete services those modules
// build eagerly -- this Handler is built at Register time but reads
// module.service AT CALL TIME, because go/integration's own Service is
// built later, in Attach (see module.go's "Register-time wiring, Attach-time
// Service" doc comment, and AGENTS.md's identical section). This is the
// SAME forwarding-wrapper technique module.go's handleDomainEvent and
// webhookDeliveryHandler already use for the identical reason -- Handler is
// simply a third place that reads m.service once Attach has produced one,
// rather than a new pattern.
//
// This fragment covers round 1's API-key surface only (Create/List/Rotate/
// Revoke) -- round 2's webhook-subscription CRUD remains unmounted, per
// AGENTS.md's "Deliberately not in scope" table; this Handler does not
// implement anything for that surface and never will unless a later round's
// own fragment grows this module's api.ServerInterface further.
type Handler struct {
	module  *Module
	subject SubjectResolver
	mux     *http.ServeMux
}

// NewHandler returns a Handler serving round 1's API-key surface through
// module, resolving the caller who creates a key through subject. subject
// may be nil, in which case integration_createAPIKey fails closed with
// ErrSubjectUnresolved rather than guessing a creator -- see SubjectResolver's
// own doc comment. list, rotate and revoke need no caller identity at all
// (see Handler's own doc comment) and are unaffected by a nil subject.
//
// The returned Handler's routing is registered by the generated
// api.HandlerFromMux helper: it derives this module's method+path patterns
// from the "paths:" keys of api/openapi.yaml itself, exactly as org's and
// storage's NewHandler do for their own routes.
func NewHandler(module *Module, subject SubjectResolver) *Handler {
	h := &Handler{module: module, subject: subject}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// service returns the Service Module.Attach built, or writes an internal
// error and reports false when Attach has not run yet. This is only ever
// reachable in practice if a request somehow lands before Attach has run --
// Attach happens at startup, before any application traffic -- the same
// "unusual, handled anyway, never assumed away" posture module.go's
// handleDomainEvent documents for the identical race on the event-subscriber
// side.
func (h *Handler) service(w http.ResponseWriter) (*Service, bool) {
	if h.module == nil || h.module.service == nil {
		writeError(w, errors.New("integration: HTTP handler ran before Module.Attach"))
		return nil, false
	}
	return h.module.service, true
}

// mustTenant resolves the caller's tenant, annotating the request's span.
// Unreachable in normal operation -- see org's identical comment on its own
// equivalent call -- because a host must never allowlist this module's
// routes, so tenancy.Middleware has already rejected anything that could
// reach here with no resolved tenant. Handled anyway, never assumed away:
// Service's own MustTenantFromContext call underneath would otherwise fail
// closed with this exact same unwrapped error one layer down, with a less
// specific log line.
func mustTenant(w http.ResponseWriter, r *http.Request) (pkgcore.TenantID, bool) {
	ctx := r.Context()
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		writeError(w, ErrInternal.WithCause(err))
		return "", false
	}
	obs.AnnotateTenant(ctx)
	return tenant, true
}

// resolveSubject returns the authenticated caller's user id, writing
// ErrSubjectUnresolved and reporting false when no SubjectResolver is wired
// or it could not identify the caller. It never invents a default creator.
func (h *Handler) resolveSubject(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.subject == nil {
		writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	userID, ok := h.subject.Subject(r)
	if !ok || userID == "" {
		writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	return userID, true
}

// decodeOptionalJSON decodes r's body into dst, tolerating a genuinely
// empty body (io.EOF) as "use dst's zero value" rather than an error --
// every field of IntegrationCreateAPIKeyRequest, this handler's one request
// body, is optional, so a caller issuing a key with no body at all is
// making a legal request, not a malformed one. Any other decode failure
// (malformed JSON, a value of the wrong shape) writes ErrInvalidRequestBody
// and reports false.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, ErrInvalidRequestBody.WithCause(err))
		return false
	}
	return true
}

// IntegrationCreateAPIKey implements api.ServerInterface: POST
// /api/v1/integration/apikeys. The creator is the caller's own authenticated
// identity, resolved through SubjectResolver -- never a request field.
func (h *Handler) IntegrationCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}
	createdBy, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	var req api.IntegrationCreateAPIKeyRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	var scopes []string
	if req.Scopes != nil {
		scopes = *req.Scopes
	}

	created, err := svc.Create(ctx, CreateInput{
		CreatedBy: createdBy,
		Scopes:    scopes,
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	// "id", not "key_id" or "credential_id": go/observability's redaction
	// layer masks any attribute whose NAME contains "key" or "credential" as
	// sensitive stems (redact.go's own sensitiveStems list), which would
	// hide this harmless UUID behind [REDACTED] for no security benefit --
	// the log message itself already says what this id identifies.
	obs.FromContext(ctx).Info("integration api key created", "id", created.ID, "created_by", created.CreatedBy)
	writeJSON(w, http.StatusCreated, toCreatedAPIKeyResponse(created))
}

// IntegrationListAPIKeys implements api.ServerInterface: GET
// /api/v1/integration/apikeys. Never exposes the raw key or its hash -- see
// Service.List's own "credential-material-free" contract.
func (h *Handler) IntegrationListAPIKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	summaries, err := svc.List(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]api.IntegrationAPIKeySummary, 0, len(summaries))
	for i := range summaries {
		items = append(items, toAPIKeySummaryResponse(&summaries[i]))
	}
	writeJSON(w, http.StatusOK, api.IntegrationListAPIKeysResponse{APIKeys: &items})
}

// IntegrationRotateAPIKey implements api.ServerInterface: POST
// /api/v1/integration/apikeys/{keyId}/rotate. keyId never needs a caller
// identity of its own: Service.Rotate carries the predecessor's own
// CreatedBy forward and re-validates its Scopes against THAT creator's
// current permissions, never the HTTP caller's -- see Service.Rotate's own
// doc comment.
func (h *Handler) IntegrationRotateAPIKey(w http.ResponseWriter, r *http.Request, keyID api.KeyID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	created, err := svc.Rotate(ctx, keyID)
	if err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("integration api key rotated", "predecessor_id", keyID, "id", created.ID)
	writeJSON(w, http.StatusOK, toCreatedAPIKeyResponse(created))
}

// IntegrationRevokeAPIKey implements api.ServerInterface: DELETE
// /api/v1/integration/apikeys/{keyId}.
func (h *Handler) IntegrationRevokeAPIKey(w http.ResponseWriter, r *http.Request, keyID api.KeyID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	if err := svc.Revoke(ctx, keyID); err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("integration api key revoked", "id", keyID)
	w.WriteHeader(http.StatusNoContent)
}

// toCreatedAPIKeyResponse maps a *CreatedAPIKey onto the spec-generated
// response schema.
func toCreatedAPIKeyResponse(k *CreatedAPIKey) api.IntegrationCreatedAPIKey {
	return api.IntegrationCreatedAPIKey{
		ID:        &k.ID,
		Key:       &k.Key,
		Prefix:    &k.Prefix,
		Scopes:    &k.Scopes,
		CreatedBy: &k.CreatedBy,
		ExpiresAt: &k.ExpiresAt,
	}
}

// toAPIKeySummaryResponse maps one APIKeySummary onto the spec-generated
// response schema. Deliberately has no field reading Hash or the raw key --
// APIKeySummary itself carries neither, per Service.List's own contract.
func toAPIKeySummaryResponse(s *APIKeySummary) api.IntegrationAPIKeySummary {
	return api.IntegrationAPIKeySummary{
		ID:          &s.ID,
		Prefix:      &s.Prefix,
		Scopes:      &s.Scopes,
		CreatedBy:   &s.CreatedBy,
		CreatedAt:   &s.CreatedAt,
		ExpiresAt:   &s.ExpiresAt,
		LastUsedAt:  s.LastUsedAt,
		RevokedAt:   s.RevokedAt,
		Revoked:     &s.Revoked,
		Expired:     &s.Expired,
		CreatorLeft: &s.CreatorLeft,
	}
}

// writeJSON writes v to w as a JSON body with status, matching org's and
// storage's identical helper.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes err to w as a JSON {code, params} body -- the
// spec-generated api.IntegrationError, the same structured-error envelope
// every other module's writeError produces. An err that is not an
// *apperr.Error -- meaning something below this handler did not classify it
// -- is folded into ErrInternal so a caller never sees raw Go error text.
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal
	}
	envelope := api.IntegrationError{Code: &appErr.Code}
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
