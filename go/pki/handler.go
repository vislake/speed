package pki

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/pki/api"
)

// jsonContentType is the Content-Type every JSON response below writes,
// matching notes', config', org's and storage's own handler constant of
// the same name.
const jsonContentType = "application/json; charset=utf-8"

// pemContentType is the Content-Type PkiGetAuthorityCrl writes -- the CRL
// operation's response is a PEM document, not JSON.
const pemContentType = "application/x-pem-file; charset=utf-8"

// Handler serves pki's round-3 HTTP surface by implementing the
// spec-generated api.ServerInterface (api/pki-server.gen.go, regenerated
// from this module's api/openapi.yaml by task api:gen -- the compile-time
// assertion at the bottom of this file is what makes "spec changed,
// handler not" a compile failure instead of a runtime surprise). The five
// operations it implements are the whole surface the spec defines: revoke
// a signing key, revoke a certificate, and the three read operations
// (key-lifecycle JWKS, authority-chain JWKS, authority CRL).
//
// # Tenant context: read only where the underlying data needs it
//
// Unlike every other module's HTTP surface, pki's own tables span two data
// domains: pki_certificates is tenant data, but pki_signing_keys and
// pki_authorities are platform data with no tenant at all
// (docs/internal/04-data-and-tenancy.md). Only PkiRevokeCertificate reads
// the caller's tenant from the request context (via
// pkgcore.MustTenantFromContext, never from a request parameter, header or
// body, per root CLAUDE.md's multi-tenant isolation rule) -- the other four
// operations never do, because the rows they touch have no tenant column
// to scope by.
//
// # Audit trail
//
// PkiRevokeSigningKey and PkiRevokeCertificate each record an AuditEvent
// through audit.Emit after their underlying Service/CAService call has
// already committed -- the same "record at the HTTP boundary, after the
// write" placement examples/reference-app/internal/notes/handler.go's
// recordNoteCreatedAudit documents, chosen for the identical reason: it
// keeps this module's own Go API (revocation.go) free of an audit.Emit
// dependency it does not otherwise need, and it sidesteps the same-SQLite-
// connection deadlock hazard go/dbkit/audit's own known-limitations entry
// records for a write-capture plugin sharing a transaction with the
// business write. bus/auditActions are nil-safe: a nil bus makes the
// audit-record calls no-ops (recordAudit checks bus first, mirroring
// Service.publish's own nil-bus tolerance), so a host that boots pki
// without ever wiring an EventBus (not a real deployment shape, but not
// something this Handler crashes on either) still serves every operation
// correctly, just without an audit trail.
type Handler struct {
	service      *Service
	ca           *CAService
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	mux          *http.ServeMux
}

// NewHandler returns a Handler serving service's and ca's operations,
// recording audit events on bus (validated against auditActions). The
// returned Handler's routing is registered by the generated
// api.HandlerFromMux helper, exactly as every other module fragment's
// NewHandler does.
func NewHandler(service *Service, ca *CAService, bus pkgcore.EventBus, auditActions pkgcore.AuditActionRegistrar) *Handler {
	h := &Handler{service: service, ca: ca, bus: bus, auditActions: auditActions}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// mustTenant resolves the caller's tenant, annotating the request's span --
// the identical helper storage's and org's own handler.go carry, needed
// here only by PkiRevokeCertificate.
func mustTenant(w http.ResponseWriter, r *http.Request) (pkgcore.TenantID, bool) {
	ctx := r.Context()
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		writeError(w, ErrInternal.WithCause(err))
		return "", false
	}
	observability.AnnotateTenant(ctx)
	return tenant, true
}

// decodeJSON decodes r's body into dst, writing a 400 apperr.Invalid and
// reporting false on any decode failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, apperr.Invalid("pki.invalid_request_body").WithCause(err))
		return false
	}
	return true
}

// PkiRevokeSigningKey implements api.ServerInterface: POST
// /api/v1/pki/signing-keys/{kid}/revoke.
func (h *Handler) PkiRevokeSigningKey(w http.ResponseWriter, r *http.Request, kid api.KID) {
	ctx := r.Context()
	var req api.PkiRevokeSigningKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, apperr.Invalid("pki.revocation_reason_required"))
		return
	}

	if _, err := h.service.RevokeSigningKey(ctx, kid, req.Reason); err != nil {
		writeError(w, err)
		return
	}
	key, err := h.service.signingKeys.FindByID(ctx, kid)
	if err != nil {
		writeError(w, err)
		return
	}

	h.recordAudit(ctx, AuditActionKeyRevoke, "pki_signing_key", key.ID)
	observability.FromContext(ctx).Info("pki signing key revoked via HTTP", "kid", kid)
	writeJSON(w, http.StatusOK, toSigningKeyResponse(key))
}

// PkiRevokeCertificate implements api.ServerInterface: POST
// /api/v1/pki/certificates/{certificateId}/revoke.
func (h *Handler) PkiRevokeCertificate(w http.ResponseWriter, r *http.Request, certificateID api.CertificateID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	var req api.PkiRevokeCertificateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, apperr.Invalid("pki.revocation_reason_required"))
		return
	}

	if _, err := h.ca.RevokeCertificate(ctx, certificateID, req.Reason); err != nil {
		writeError(w, err)
		return
	}
	cert, err := h.ca.certificates.FindByID(ctx, certificateID)
	if err != nil {
		writeError(w, err)
		return
	}

	h.recordAudit(ctx, AuditActionCertificateRevoke, "pki_certificate", cert.ID)
	observability.FromContext(ctx).Info("pki certificate revoked via HTTP", "certificate_id", certificateID)
	writeJSON(w, http.StatusOK, toCertificateResponse(cert))
}

// PkiGetKeyJwks implements api.ServerInterface: GET /api/v1/pki/jwks.
func (h *Handler) PkiGetKeyJwks(w http.ResponseWriter, r *http.Request, params api.PkiGetKeyJwksParams) {
	ctx := r.Context()
	jwks, err := h.service.ExportJWKS(ctx, params.Purpose)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jwks)
}

// PkiGetAuthorityJwks implements api.ServerInterface: GET
// /api/v1/pki/authorities/{authorityId}/jwks.
func (h *Handler) PkiGetAuthorityJwks(w http.ResponseWriter, r *http.Request, authorityID api.AuthorityID) {
	ctx := r.Context()
	jwks, err := h.ca.ExportAuthorityChainJWKS(ctx, authorityID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jwks)
}

// PkiGetAuthorityCrl implements api.ServerInterface: GET
// /api/v1/pki/authorities/{authorityId}/crl. Serves the stored CRLPEM
// exactly as GenerateCRL (or the periodic pki.crl_regenerate job) last
// wrote it -- this operation never generates one itself.
func (h *Handler) PkiGetAuthorityCrl(w http.ResponseWriter, r *http.Request, authorityID api.AuthorityID) {
	ctx := r.Context()
	authority, err := h.ca.authorities.FindByID(ctx, authorityID)
	if err != nil {
		writeError(w, err)
		return
	}
	if authority.CRLPEM == "" {
		writeError(w, ErrCRLNotGenerated.WithParam("authority_id", authorityID))
		return
	}
	w.Header().Set("Content-Type", pemContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(authority.CRLPEM))
}

// recordAudit records an AuditEvent for action against a resource of type
// resourceType and id resourceID, through audit.Emit -- see this file's own
// doc comment for why revocation.go itself never calls audit.Emit directly.
// A failure is logged, not returned, matching notes.recordNoteCreatedAudit's
// identical choice: the underlying revocation already committed by the
// time this runs, so an audit-write failure must not turn an otherwise
// successful revoke into a 500 for the caller.
func (h *Handler) recordAudit(ctx context.Context, action, resourceType, resourceID string) {
	if h.bus == nil {
		return
	}
	err := audit.Emit(ctx, h.bus, h.auditActions, audit.Input{
		Action:   action,
		Resource: audit.Resource{Type: resourceType, ID: resourceID},
		Result:   audit.Result{Success: true},
	})
	if err != nil {
		observability.FromContext(ctx).Error("pki audit event emit failed",
			"action", action, "resource_id", resourceID, "error", err)
	}
}

// toSigningKeyResponse converts key to its spec-generated JSON response
// type. Every field is optional (hence pointer-typed) in the generated
// model; the nullable timestamps stay absent on the wire exactly when the
// row's own pointer is nil, never rendered as a zero time.
func toSigningKeyResponse(key *SigningKey) api.PkiSigningKey {
	resp := api.PkiSigningKey{
		Kid:         &key.ID,
		Purpose:     &key.Purpose,
		Algorithm:   &key.Algorithm,
		Status:      &key.Status,
		NotBefore:   &key.NotBefore,
		NotAfter:    &key.NotAfter,
		ActivatedAt: key.ActivatedAt,
		RetiringAt:  key.RetiringAt,
		RetiredAt:   key.RetiredAt,
		RevokedAt:   key.RevokedAt,
		CreatedAt:   &key.CreatedAt,
	}
	if key.RevocationReason != "" {
		resp.RevocationReason = &key.RevocationReason
	}
	return resp
}

// toCertificateResponse converts cert to its spec-generated JSON response
// type, the identical shape toSigningKeyResponse documents above.
func toCertificateResponse(cert *Certificate) api.PkiCertificate {
	var sans []string
	if len(cert.SANs) > 0 {
		_ = json.Unmarshal(cert.SANs, &sans)
	}
	resp := api.PkiCertificate{
		ID:           &cert.ID,
		AuthorityID:  &cert.AuthorityID,
		Purpose:      &cert.Purpose,
		Subject:      &cert.Subject,
		Serial:       &cert.Serial,
		Status:       &cert.Status,
		KeyDelivered: &cert.KeyDelivered,
		NotBefore:    &cert.NotBefore,
		NotAfter:     &cert.NotAfter,
		RevokedAt:    cert.RevokedAt,
		CreatedAt:    &cert.CreatedAt,
	}
	if sans != nil {
		resp.Sans = &sans
	}
	if cert.RevocationReason != "" {
		resp.RevocationReason = &cert.RevocationReason
	}
	return resp
}

// writeJSON writes v to w as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes err to w as a JSON {code, params} body -- the
// spec-generated api.PkiError, the same structured-error envelope every
// other module's own writeError produces. An err that is not an
// *apperr.Error is folded into ErrInternal so a caller never sees raw Go
// error text.
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal
	}
	envelope := api.PkiError{Code: &appErr.Code}
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
