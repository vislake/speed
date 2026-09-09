package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// newClinicNameTree returns an org TreeService over a freshly migrated
// SQLite database -- the real org module's tree, exactly the service
// buildServer wires into wireClinicName (server.go) -- so the route
// handler tests below exercise the real Root lookup against the real
// tree, not a stand-in.
func newClinicNameTree(t *testing.T) *org.TreeService {
	t.Helper()
	db := dbtest.NewSQLite(t)

	orgModule := org.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(orgModule); err != nil {
		t.Fatalf("register org module for migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply org migrations: %v", err)
	}
	return orgModule.Tree()
}

// clinicNameGET issues a GET against the wired route under tenant and
// returns the recorded response.
func clinicNameGET(t *testing.T, mux *http.ServeMux, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, clinicNamePath, nil)
	if tenant != "" {
		req = req.WithContext(pkgcore.WithTenant(req.Context(), pkgcore.TenantID(tenant)))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// decodeClinicNameResponse decodes rec's body as the name answer.
func decodeClinicNameResponse(t *testing.T, rec *httptest.ResponseRecorder) clinicNameResponse {
	t.Helper()
	var resp clinicNameResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode clinic-name response: %v; body = %s", err, rec.Body.String())
	}
	return resp
}

// TestClinicNameRoute_TenantWithATreeAnswersItsRootName drives the route's
// name-answering half: a tenant whose org tree has a root node reads its
// name back -- the answer this app's own web chrome shows for a tenant no
// static demo copy describes (clinic_name.go's own doc comment).
func TestClinicNameRoute_TenantWithATreeAnswersItsRootName(t *testing.T) {
	tree := newClinicNameTree(t)
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-clinic-a"))
	if _, err := tree.CreateRoot(ctx, "Smile Studio Downtown", "clinic"); err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	mux := http.NewServeMux()
	wireClinicName(mux, tree)
	rec := clinicNameGET(t, mux, "tenant-clinic-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if resp := decodeClinicNameResponse(t, rec); resp.Name != "Smile Studio Downtown" {
		t.Fatalf("name = %q, want the tenant's org root name", resp.Name)
	}
}

// TestClinicNameRoute_TenantWithNoTreeAnswersAnEmptyName pins the
// no-tree half of the answer: a tenant context with a real tenant but no
// org tree yet -- a tenant between provisioning steps, or not a customer
// tenant at all -- answers 200 with the empty name, never an identifier
// invented to fill the slot and never a fabricated error.
func TestClinicNameRoute_TenantWithNoTreeAnswersAnEmptyName(t *testing.T) {
	tree := newClinicNameTree(t)
	mux := http.NewServeMux()
	wireClinicName(mux, tree)

	rec := clinicNameGET(t, mux, "tenant-with-no-tree")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if resp := decodeClinicNameResponse(t, rec); resp.Name != "" {
		t.Fatalf("name = %q, want the empty name for a tenant with no tree", resp.Name)
	}
}

// TestClinicNameRoute_NonGetMethod_IsRefused pins the route's method
// gate: only GET may read a tenant's name, and a non-GET method is
// refused with the Allow header naming the one legal method plus the
// envelope's method-not-allowed code -- never answered with a name.
func TestClinicNameRoute_NonGetMethod_IsRefused(t *testing.T) {
	tree := newClinicNameTree(t)
	mux := http.NewServeMux()
	wireClinicName(mux, tree)

	req := httptest.NewRequest(http.MethodPost, clinicNamePath, nil)
	req = req.WithContext(pkgcore.WithTenant(req.Context(), pkgcore.TenantID("tenant-clinic-a")))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow header = %q, want %q", allow, http.MethodGet)
	}
	var envelope clinicNameError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if envelope.Code != "reference_app.method_not_allowed" {
		t.Fatalf("error code = %q, want reference_app.method_not_allowed", envelope.Code)
	}
}

// TestClinicNameRoute_NoTenantInContext_AnswersACodedError pins the
// failure shape: a request whose context carries no resolved tenant -- an
// answer no org row could ever name -- is a coded 500 (the org tree's
// own internal-error answer for the missing tenant context, written
// through the route's coded-error path), never an empty name the caller
// could mistake for "unnamed": the empty name is reserved for the
// real-tenant-but-no-tree case above.
func TestClinicNameRoute_NoTenantInContext_AnswersACodedError(t *testing.T) {
	tree := newClinicNameTree(t)
	mux := http.NewServeMux()
	wireClinicName(mux, tree)

	rec := clinicNameGET(t, mux, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	var envelope clinicNameError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if envelope.Code == "" {
		t.Fatal("error code is empty -- a genuine failure must be a coded error, never a silent empty name")
	}
}

// TestWriteClinicNameError_PreservesACodedErrorAndFoldsARawOne pins
// writeClinicNameError's classification directly: an *apperr.Error keeps
// its own code and status on the wire, and a raw error is folded into
// the internal fallback so a caller never sees raw Go error text.
func TestWriteClinicNameError_PreservesACodedErrorAndFoldsARawOne(t *testing.T) {
	coded := apperr.Invalid("test.clinic_name_error")
	rec := httptest.NewRecorder()
	writeClinicNameError(rec, coded)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("coded error status = %d, want 400", rec.Code)
	}
	var envelope clinicNameError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if envelope.Code != "test.clinic_name_error" {
		t.Fatalf("coded error code = %q, want the error's own code", envelope.Code)
	}

	rec = httptest.NewRecorder()
	writeClinicNameError(rec, context.DeadlineExceeded)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("raw error status = %d, want 500", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if envelope.Code != "reference_app.internal_error" {
		t.Fatalf("raw error code = %q, want the internal fallback", envelope.Code)
	}
}
