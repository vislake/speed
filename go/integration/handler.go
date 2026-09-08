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

// Handler serves this module's spec-generated HTTP surface by implementing
// the spec-generated api.ServerInterface (see api/integration-server.gen.go,
// regenerated from this module's api/openapi.yaml by task api:gen -- the
// compile-time assertion at the bottom of this file is what makes "spec
// changed, handler not" a compile failure instead of a runtime surprise).
// The fragment covers the API-key surface (Create/List/Rotate/Revoke) and
// the webhook-subscription surface -- subscription CRUD
// (Create/List/Update/Delete), Restore, and the recent-deliveries log.
//
// It must run downstream of tenancy.Middleware on a non-allowlisted path:
// every method reads the tenant tenancy.Middleware already resolved into the
// request context, and never from a request parameter, header or body, per
// the multi-tenant isolation rule -- Service's own methods
// re-derive the tenant themselves through pkgcore.MustTenantFromContext, so
// Handler adds no tenant resolution of its own beyond the observability
// annotation mustTenant below performs.
//
// # Built differently from every other module's Handler
//
// Unlike org's, storage's and notification's Handler -- each built once, at
// NewModule/Register time, directly from concrete services those modules
// build eagerly -- this Handler is built at Register time but reads
// module.service AT CALL TIME, because go/integration's own Service is
// built later, in Attach (see module.go's "Register-time wiring, Attach-time
// Service" doc comment). This is the
// SAME forwarding-wrapper technique module.go's handleDomainEvent and
// webhookDeliveryHandler already use for the identical reason -- Handler is
// simply a third place that reads m.service once Attach has produced one,
// rather than a new pattern.
type Handler struct {
	module  *Module
	subject SubjectResolver
	mux     *http.ServeMux
}

// NewHandler returns a Handler serving this module's spec-generated surface
// (see Handler's own doc comment for what that covers) through module,
// resolving the caller who creates a key or a subscription through subject.
// subject may be nil, in which case integration_createAPIKey and
// integration_createWebhookSubscription fail closed with
// ErrSubjectUnresolved rather than guessing a creator -- see SubjectResolver's
// own doc comment. list, rotate, revoke, update, delete, restore and the
// deliveries log need no caller identity at all (see Handler's own doc
// comment) and are unaffected by a nil subject.
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
// every field of IntegrationCreateAPIKeyRequest is optional, so a caller
// issuing a key with no body at all is making a legal request, not a
// malformed one (this handler's ONE optional body: the webhook request
// bodies declare their fields required, so they decode through
// decodeRequiredJSON below instead). Any other decode failure (malformed
// JSON, a value of the wrong shape) writes ErrInvalidRequestBody and
// reports false.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, ErrInvalidRequestBody.WithCause(err))
		return false
	}
	return true
}

// decodeRequiredJSON decodes r's body into dst, treating a genuinely empty
// body (io.EOF) as a malformed request rather than a legal one: the webhook
// request bodies -- IntegrationCreateWebhookSubscriptionRequest and
// IntegrationUpdateWebhookSubscriptionRequest -- mark every field required
// (api/openapi.yaml declares both requestBody schemas required: true), so a
// caller sending none is making a malformed request, refused with the plain
// ErrInvalidRequestBody the identical strict refusal org's, storage's and
// notification's own handlers apply to their required bodies. Any other
// decode failure writes ErrInvalidRequestBody.WithCause and reports false.
func decodeRequiredJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, ErrInvalidRequestBody)
			return false
		}
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
		if created != nil {
			// Partial failure: the key row committed, but its post-commit
			// audit record failed. Service.Create returns both halves in
			// that case (see its own doc comment). The operation SUCCEEDED
			// as far as the caller is concerned -- the key exists and
			// works, and key material is shown exactly once and never
			// reproduced by List -- so the handler answers the operation's
			// ORDINARY success with the credential in its normal field and
			// the response's auditRecordMissing field true, declaring the
			// gap. The credential must never ride in an error envelope:
			// error responses flow into logs, tickets and bug reports, a
			// distribution channel success bodies do not enter.
			obs.FromContext(ctx).Warn("integration api key created but its audit record failed",
				"id", created.ID, "created_by", created.CreatedBy, "error", err)
			resp := toCreatedAPIKeyResponse(created)
			auditGap := true
			resp.AuditRecordMissing = &auditGap
			writeJSON(w, http.StatusCreated, resp)
			return
		}
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
		if created != nil {
			// Partial failure: the replacement key was created and committed,
			// but a leg after it failed -- the predecessor's revocation, or
			// (when the failure came from Rotate's own internal Create) the
			// replacement's creation audit record (Service.Rotate returns
			// both halves -- see its own doc comment). Either way the
			// operation SUCCEEDED as far as the caller is concerned -- the
			// replacement exists and works, and its material is shown
			// exactly once and never reproduced -- so the handler answers
			// the operation's ORDINARY success with the credential in its
			// normal field and the response's auditRecordMissing field
			// true: a caller seeing it must not assume the predecessor is
			// revoked (the rotation's bookkeeping did not complete), and
			// must persist the replacement from the success response. The
			// credential never rides in an error envelope.
			obs.FromContext(ctx).Warn("integration api key rotated but its bookkeeping did not complete",
				"predecessor_id", keyID, "id", created.ID, "error", err)
			resp := toCreatedAPIKeyResponse(created)
			auditGap := true
			resp.AuditRecordMissing = &auditGap
			writeJSON(w, http.StatusOK, resp)
			return
		}
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

// IntegrationListWebhookSubscriptions implements api.ServerInterface: GET
// /api/v1/integration/webhooks. Never exposes a subscription's signing
// secret -- WebhookSubscriptionSummary carries none, per Service's own
// contract. Needs no caller identity of its own: List reads only the tenant,
// and CreatedBy comes from the stored row.
func (h *Handler) IntegrationListWebhookSubscriptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	summaries, err := svc.ListWebhookSubscriptions(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]api.IntegrationWebhookSubscriptionSummary, 0, len(summaries))
	for i := range summaries {
		items = append(items, toWebhookSubscriptionSummaryResponse(&summaries[i]))
	}
	writeJSON(w, http.StatusOK, api.IntegrationListWebhookSubscriptionsResponse{WebhookSubscriptions: &items})
}

// IntegrationCreateWebhookSubscription implements api.ServerInterface: POST
// /api/v1/integration/webhooks. The creator is the caller's own
// authenticated identity, resolved through SubjectResolver -- never a
// request field -- the identical rule integration_createAPIKey applies: the
// new subscription's CreatedBy is the audit trail's responsible party. The
// response is the one and only place the raw signing secret is ever
// available, mirroring IntegrationCreatedAPIKey's own "shown once"
// contract.
func (h *Handler) IntegrationCreateWebhookSubscription(w http.ResponseWriter, r *http.Request) {
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

	var req api.IntegrationCreateWebhookSubscriptionRequest
	if !decodeRequiredJSON(w, r, &req) {
		return
	}

	created, err := svc.CreateWebhookSubscription(ctx, CreateWebhookSubscriptionInput{
		URL:        req.URL,
		EventTypes: req.EventTypes,
		CreatedBy:  createdBy,
	})
	if err != nil {
		if created != nil {
			// Partial failure: the subscription row committed, but its
			// post-commit audit record failed. Service.
			// CreateWebhookSubscription returns both halves in that case
			// (see its own doc comment). The operation SUCCEEDED as far as
			// the caller is concerned -- the subscription exists and is
			// already delivering events signed with a secret that is shown
			// exactly once and never reproduced -- so the handler answers
			// the operation's ORDINARY success with the secret in its
			// normal field and the response's auditRecordMissing field
			// true, declaring the gap. The secret must never ride in an
			// error envelope: error responses flow into logs, tickets and
			// bug reports, a distribution channel success bodies do not
			// enter.
			obs.FromContext(ctx).Warn("integration webhook subscription created but its audit record failed",
				"id", created.ID, "created_by", created.CreatedBy, "error", err)
			resp := toCreatedWebhookSubscriptionResponse(created)
			auditGap := true
			resp.AuditRecordMissing = &auditGap
			writeJSON(w, http.StatusCreated, resp)
			return
		}
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("integration webhook subscription created", "id", created.ID, "created_by", created.CreatedBy)
	writeJSON(w, http.StatusCreated, toCreatedWebhookSubscriptionResponse(created))
}

// IntegrationUpdateWebhookSubscription implements api.ServerInterface: PATCH
// /api/v1/integration/webhooks/{subscriptionId}. A field absent from the
// request leaves the corresponding stored value unchanged -- the request
// schema's pointer fields map one-to-one onto Service's own "nil means no
// change" UpdateWebhookSubscriptionInput. subscriptionId never needs a
// caller identity of its own: the row's CreatedBy is never rewritten by an
// update (Service.UpdateWebhookSubscription does not touch it), so no
// SubjectResolver is consulted here.
func (h *Handler) IntegrationUpdateWebhookSubscription(w http.ResponseWriter, r *http.Request, subscriptionID api.SubscriptionID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	var req api.IntegrationUpdateWebhookSubscriptionRequest
	if !decodeRequiredJSON(w, r, &req) {
		return
	}
	in := UpdateWebhookSubscriptionInput{
		ID:     subscriptionID,
		URL:    req.URL,
		Active: req.Active,
	}
	if req.EventTypes != nil {
		in.EventTypes = *req.EventTypes
	}

	updated, err := svc.UpdateWebhookSubscription(ctx, in)
	if err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("integration webhook subscription updated", "id", updated.ID)
	writeJSON(w, http.StatusOK, toWebhookSubscriptionSummaryResponse(updated))
}

// IntegrationDeleteWebhookSubscription implements api.ServerInterface:
// DELETE /api/v1/integration/webhooks/{subscriptionId}. The deletion is a
// mark-delete, undoable through integration_restoreWebhookSubscription.
func (h *Handler) IntegrationDeleteWebhookSubscription(w http.ResponseWriter, r *http.Request, subscriptionID api.SubscriptionID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	if err := svc.DeleteWebhookSubscription(ctx, subscriptionID); err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("integration webhook subscription deleted", "id", subscriptionID)
	w.WriteHeader(http.StatusNoContent)
}

// IntegrationRestoreWebhookSubscription implements api.ServerInterface: POST
// /api/v1/integration/webhooks/{subscriptionId}/restore. The restored
// subscription always comes back paused (Active = false) -- Service's own
// deliberate rule -- so resuming delivery is the caller's explicit next
// PATCH setting active=true, never an implicit side effect of this call.
func (h *Handler) IntegrationRestoreWebhookSubscription(w http.ResponseWriter, r *http.Request, subscriptionID api.SubscriptionID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	if err := svc.RestoreWebhookSubscription(ctx, subscriptionID); err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("integration webhook subscription restored", "id", subscriptionID)
	w.WriteHeader(http.StatusNoContent)
}

// IntegrationListWebhookDeliveries implements api.ServerInterface: GET
// /api/v1/integration/webhooks/{subscriptionId}/deliveries. params.Limit is
// passed straight through -- nil (or any non-positive value) becomes
// Service's default of 50 -- and a malformed query value never reaches this
// handler at all: the generated wrapper's own parameter binding refuses it
// with the plain-text 400 of HandlerFromMux's default ErrorHandlerFunc
// (integration-server.gen.go) before this method runs.
func (h *Handler) IntegrationListWebhookDeliveries(w http.ResponseWriter, r *http.Request, subscriptionID api.SubscriptionID, params api.IntegrationListWebhookDeliveriesParams) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	svc, ok := h.service(w)
	if !ok {
		return
	}

	var limit int
	if params.Limit != nil {
		limit = *params.Limit
	}
	deliveries, err := svc.ListRecentWebhookDeliveries(ctx, subscriptionID, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]api.IntegrationWebhookDeliverySummary, 0, len(deliveries))
	for i := range deliveries {
		items = append(items, toWebhookDeliverySummaryResponse(&deliveries[i]))
	}
	writeJSON(w, http.StatusOK, api.IntegrationListWebhookDeliveriesResponse{Deliveries: &items})
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

// toWebhookSubscriptionSummaryResponse maps one WebhookSubscriptionSummary
// onto the spec-generated response schema. Deliberately has no field reading
// Secret -- WebhookSubscriptionSummary carries none, per Service's own
// contract.
func toWebhookSubscriptionSummaryResponse(s *WebhookSubscriptionSummary) api.IntegrationWebhookSubscriptionSummary {
	return api.IntegrationWebhookSubscriptionSummary{
		ID:         &s.ID,
		URL:        &s.URL,
		EventTypes: &s.EventTypes,
		Active:     &s.Active,
		CreatedBy:  &s.CreatedBy,
		CreatedAt:  &s.CreatedAt,
		UpdatedAt:  &s.UpdatedAt,
	}
}

// toCreatedWebhookSubscriptionResponse maps a *CreatedWebhookSubscription
// onto the spec-generated response schema -- the one response carrying the
// raw signing secret, and therefore the only mapper below with a Secret
// field at all.
func toCreatedWebhookSubscriptionResponse(s *CreatedWebhookSubscription) api.IntegrationCreatedWebhookSubscription {
	return api.IntegrationCreatedWebhookSubscription{
		ID:         &s.ID,
		URL:        &s.URL,
		EventTypes: &s.EventTypes,
		Secret:     &s.Secret,
		Active:     &s.Active,
		CreatedBy:  &s.CreatedBy,
		CreatedAt:  &s.CreatedAt,
	}
}

// toWebhookDeliverySummaryResponse maps one WebhookDeliverySummary onto the
// spec-generated response schema.
func toWebhookDeliverySummaryResponse(d *WebhookDeliverySummary) api.IntegrationWebhookDeliverySummary {
	return api.IntegrationWebhookDeliverySummary{
		ID:             &d.ID,
		SubscriptionID: &d.SubscriptionID,
		EventType:      &d.EventType,
		EventVersion:   &d.EventVersion,
		Status:         &d.Status,
		Attempts:       &d.Attempts,
		LastStatusCode: d.LastStatusCode,
		LastError:      &d.LastError,
		LastAttemptAt:  d.LastAttemptAt,
		DeliveredAt:    d.DeliveredAt,
		CreatedAt:      &d.CreatedAt,
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
// the spec-first flow: add an operation to the fragment, regenerate, and
// this assertion stops compiling until Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
