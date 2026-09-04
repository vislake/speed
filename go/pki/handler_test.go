package pki

import (
	"bytes"
	"context"
	"crypto/x509/pkix"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/pki/api"
)

// This file tests Handler, pki's round-3 HTTP surface: the five operations
// api/openapi.yaml defines, served behind the spec-generated
// api.ServerInterface (api/pki-server.gen.go) -- the enforcement half of
// the spec-first flow. Every test drives whole requests through Handler's
// own mux (the same routing Module.Register mounts at apiPath) over real
// Service/CAService instances sharing one migrated database, with a real
// in-memory EventBus so audit recording is exercised for real rather than
// stubbed out.

// newHandlerHarness returns a Handler over the module's real composition,
// mirroring storage's own newHandlerHarness in shape: one migrated
// database, real Service and CAService instances, and a real
// pkgcore.MemoryEventBus with the module's own audit action names
// registered on a minimal AuditActionRegistrar.
func newHandlerHarness(t *testing.T) (*Handler, *Service, *CAService) {
	t.Helper()
	db := newTestDB(t)
	svc := NewService(NewLocalSigner(db), "local", NewSigningKeyRepository(db), DefaultCacheTTL, DefaultPropagationWindow, DefaultRenewalLeadTime)
	t.Cleanup(func() { _ = svc.Close() })
	ca := NewCAService(NewLocalSigner(db), "local", NewAuthorityRepository(db), NewCertificateRepository(db), NewCertificateRevocationRepository(db))

	bus := pkgcore.NewMemoryEventBus()
	svc.bus = bus
	bus.Subscribe(EventSigningKeyStaged, svc.onSigningKeyLifecycleEvent)
	bus.Subscribe(EventSigningKeyActivated, svc.onSigningKeyLifecycleEvent)
	bus.Subscribe(EventSigningKeyRetired, svc.onSigningKeyLifecycleEvent)
	bus.Subscribe(EventSigningKeyRevoked, svc.onSigningKeyLifecycleEvent)
	ca.bus = bus

	actions := &fakeAuditActionRegistrar{}
	if err := actions.Add(AuditActionKeyRevoke, AuditActionCertificateRevoke); err != nil {
		t.Fatalf("register audit actions: %v", err)
	}

	return NewHandler(svc, ca, bus, actions), svc, ca
}

// request drives one request against h and returns the recorder. tenant
// non-empty puts a resolved tenant on the request context, mirroring where
// tenancy.Middleware would have left it for a non-allowlisted route.
func requestPKI(t *testing.T, h *Handler, tenant pkgcore.TenantID, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	ctx := context.Background()
	if tenant != "" {
		ctx = pkgcore.WithTenant(ctx, tenant)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_PkiRevokeSigningKey_Success(t *testing.T) {
	h, svc, _ := newHandlerHarness(t)
	ctx := context.Background()
	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	kid, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}

	rec := requestPKI(t, h, "", http.MethodPost, "/api/v1/pki/signing-keys/"+kid+"/revoke", api.PkiRevokeSigningKeyRequest{Reason: "compromised"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp api.PkiSigningKey
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status == nil || *resp.Status != SigningKeyStatusRevoked {
		t.Errorf("response Status = %v, want %q", resp.Status, SigningKeyStatusRevoked)
	}

	if _, _, _, err := svc.ActiveSigner(ctx, "authn.access_token"); !apperrIs(err, ErrNoActiveKey) {
		t.Errorf("ActiveSigner(after HTTP revoke) error = %v, want ErrNoActiveKey", err)
	}
}

func TestHandler_PkiRevokeSigningKey_MissingReason(t *testing.T) {
	h, svc, _ := newHandlerHarness(t)
	ctx := context.Background()
	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}
	kid, _, _, err := svc.ActiveSigner(ctx, "authn.access_token")
	if err != nil {
		t.Fatalf("ActiveSigner: %v", err)
	}

	rec := requestPKI(t, h, "", http.MethodPost, "/api/v1/pki/signing-keys/"+kid+"/revoke", api.PkiRevokeSigningKeyRequest{Reason: ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandler_PkiRevokeSigningKey_UnknownKID(t *testing.T) {
	h, _, _ := newHandlerHarness(t)
	rec := requestPKI(t, h, "", http.MethodPost, "/api/v1/pki/signing-keys/does-not-exist/revoke", api.PkiRevokeSigningKeyRequest{Reason: "reason"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandler_PkiRevokeCertificate_Success(t *testing.T) {
	h, _, ca := newHandlerHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	_, cert := issueTestCertificate(t, ca, ctx)

	rec := requestPKI(t, h, "tenant-acme", http.MethodPost, "/api/v1/pki/certificates/"+cert.ID+"/revoke", api.PkiRevokeCertificateRequest{Reason: "compromised"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp api.PkiCertificate
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status == nil || *resp.Status != CertificateStatusRevoked {
		t.Errorf("response Status = %v, want %q", resp.Status, CertificateStatusRevoked)
	}
}

func TestHandler_PkiRevokeCertificate_WrongTenant_NotFound(t *testing.T) {
	h, _, ca := newHandlerHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-acme"))
	_, cert := issueTestCertificate(t, ca, ctx)

	rec := requestPKI(t, h, "tenant-other", http.MethodPost, "/api/v1/pki/certificates/"+cert.ID+"/revoke", api.PkiRevokeCertificateRequest{Reason: "reason"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (another tenant's certificate must read as not-found); body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandler_PkiGetKeyJwks(t *testing.T) {
	h, svc, _ := newHandlerHarness(t)
	ctx := context.Background()
	if err := svc.EnsurePurpose(ctx, "authn.access_token", AlgorithmEd25519, 15*time.Minute); err != nil {
		t.Fatalf("EnsurePurpose: %v", err)
	}

	rec := requestPKI(t, h, "", http.MethodGet, "/api/v1/pki/jwks?purpose=authn.access_token", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp api.PkiJwks
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Keys == nil || len(*resp.Keys) != 1 {
		t.Errorf("response Keys = %v, want exactly 1", resp.Keys)
	}
}

func TestHandler_PkiGetAuthorityJwks(t *testing.T) {
	h, _, ca := newHandlerHarness(t)
	ctx := context.Background()
	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	rec := requestPKI(t, h, "", http.MethodGet, "/api/v1/pki/authorities/"+root.ID+"/jwks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp api.PkiJwks
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Keys == nil || len(*resp.Keys) != 1 {
		t.Errorf("response Keys = %v, want exactly 1", resp.Keys)
	}
}

func TestHandler_PkiGetAuthorityJwks_NotFound(t *testing.T) {
	h, _, _ := newHandlerHarness(t)
	rec := requestPKI(t, h, "", http.MethodGet, "/api/v1/pki/authorities/does-not-exist/jwks", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandler_PkiGetAuthorityCrl_NotGeneratedYet(t *testing.T) {
	h, _, ca := newHandlerHarness(t)
	ctx := context.Background()
	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}

	rec := requestPKI(t, h, "", http.MethodGet, "/api/v1/pki/authorities/"+root.ID+"/crl", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandler_PkiGetAuthorityCrl_ServesTheStoredDocument(t *testing.T) {
	h, _, ca := newHandlerHarness(t)
	ctx := context.Background()
	root, err := ca.CreateRootCA(ctx, RootCAParams{
		Subject:  pkix.Name{CommonName: "speed Root CA"},
		NotAfter: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRootCA: %v", err)
	}
	generated, err := ca.GenerateCRL(ctx, root.ID, 0)
	if err != nil {
		t.Fatalf("GenerateCRL: %v", err)
	}

	rec := requestPKI(t, h, "", http.MethodGet, "/api/v1/pki/authorities/"+root.ID+"/crl", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != generated.CRLPEM {
		t.Errorf("response body does not match the stored CRLPEM exactly")
	}
	if got := rec.Header().Get("Content-Type"); got != pemContentType {
		t.Errorf("Content-Type = %q, want %q", got, pemContentType)
	}
}

// fakeAuditActionRegistrar is a minimal pkgcore.AuditActionRegistrar,
// mirroring the memory-backed one pkgcore.NewRegistry builds internally --
// this file needs its own because it wires Handler directly rather than
// through Module.Register/Bootstrap.
type fakeAuditActionRegistrar struct {
	actions []string
}

func (r *fakeAuditActionRegistrar) Add(actions ...string) error {
	r.actions = append(r.actions, actions...)
	return nil
}

func (r *fakeAuditActionRegistrar) Actions() []string { return r.actions }

var _ pkgcore.AuditActionRegistrar = (*fakeAuditActionRegistrar)(nil)
