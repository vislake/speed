package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// Tests for http.go's two pre-auth endpoints: the host-resolved tenant
// fallback (custom domain to platform defaults, never an error), the public
// snapshot's wire shape (canonical durations, no Sensitive key, features as
// an array even when empty), the GET/HEAD-only method contract with its
// Allow header, and the service-not-attached window's structured error.

// httpTestDBSeq numbers the in-memory SQLite databases this file's tests
// open, so parallel or repeated runs never share one.
var httpTestDBSeq atomic.Int64

func openHTTPTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:config_http_%d?mode=memory&cache=shared", httpTestDBSeq.Add(1))
	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(NewModule(db)); err != nil {
		t.Fatalf("registering the config migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("applying the config migrations: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// staticHostResolver maps a request host to a tenant, the way a custom
// domain or platform subdomain would. Hosts it does not know fail with an
// error, so the module's resolver-consultation path is exercised: an
// unmatched host must fall back to platform defaults, never error.
type staticHostResolver map[string]pkgcore.TenantID

func (r staticHostResolver) Resolve(req *http.Request) (pkgcore.TenantID, error) {
	if tenant, ok := r[req.Host]; ok {
		return tenant, nil
	}
	return "", fmt.Errorf("no tenant for host %q", req.Host)
}

// mountRoutes mounts every route the registry collected onto a fresh mux,
// the way a host mounts module routes on its own router.
func mountRoutes(reg *pkgcore.Registry) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range reg.Routes.Routes() {
		mux.Handle(route.Path, route.Handler)
	}
	return mux
}

// newHTTPHarness registers (with the shared item/flag schema) and attaches
// a config module over an in-memory configs table, and returns the attached
// service -- so tests can write rows the way the platform writes them -- and
// the mounted mux the requests hit.
func newHTTPHarness(t *testing.T, resolver tenancy.Resolver) (*Service, *http.ServeMux) {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeSystemWrite)
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Config.Add(serviceTestSchemaItems...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}
	if err := reg.Features.Add(serviceTestSchemaFlags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	opts := []Option{WithCipher(buildTestCipher(t)), WithPollInterval(0)}
	if resolver != nil {
		opts = append(opts, WithResolver(resolver))
	}
	module := NewModule(openHTTPTestDB(t), opts...)
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := module.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return svc, mountRoutes(reg)
}

// httpResponse pairs a recorder with its body bytes for the assertions
// below.
type httpResponse struct {
	recorder *httptest.ResponseRecorder
	body     []byte
}

func doRequest(t *testing.T, mux *http.ServeMux, method, path, host string) httpResponse {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return httpResponse{recorder: rec, body: rec.Body.Bytes()}
}

// publicSnapshotBody is the wire shape handlePublic documents: a
// config object plus a features array.
type publicSnapshotBody struct {
	Config   map[string]any `json:"config"`
	Features []string       `json:"features"`
}

func decodeBody(t *testing.T, resp httpResponse, out any) {
	t.Helper()
	if err := json.Unmarshal(resp.body, out); err != nil {
		t.Fatalf("response body is not the documented JSON: %v\nbody: %s", err, resp.body)
	}
}

func decodeErrorCode(t *testing.T, resp httpResponse) string {
	t.Helper()
	var envelope errorEnvelope
	decodeBody(t, resp, &envelope)
	if envelope.Code == nil {
		t.Fatalf("error response carries no code: %s", resp.body)
	}
	return *envelope.Code
}

func TestHTTP_Public_ResolvesTheTenantOverridesByHost(t *testing.T) {
	svc, mux := newHTTPHarness(t, staticHostResolver{
		"studio-a.example.com": "tenant-a",
		"studio-b.example.com": "tenant-b",
	})
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.site_name", Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	if err := svc.Set(tenantA(), ScopeTenant, "brand.site_name", Value{Data: "Studio A"}, "alice"); err != nil {
		t.Fatalf("tenant-a Set: %v", err)
	}
	if err := svc.Set(tenantB(), ScopeTenant, "brand.site_name", Value{Data: "Studio B"}, "bob"); err != nil {
		t.Fatalf("tenant-b Set: %v", err)
	}

	for host, want := range map[string]any{
		"studio-a.example.com": "Studio A",
		"studio-b.example.com": "Studio B",
		// A host no resolver knows must read platform defaults, with a 200:
		// the unauthenticated display rule forbids failing over an
		// unrecognized host.
		"unknown.example.com": "Global Co",
	} {
		resp := doRequest(t, mux, http.MethodGet, PathPublic, host)
		if resp.recorder.Code != http.StatusOK {
			t.Fatalf("GET %s on host %s = %d, want 200", PathPublic, host, resp.recorder.Code)
		}
		var body publicSnapshotBody
		decodeBody(t, resp, &body)
		if body.Config["brand.site_name"] != want {
			t.Fatalf("host %s served brand.site_name = %#v, want %#v", host, body.Config["brand.site_name"], want)
		}
	}
}

func TestHTTP_Public_ServesPlatformDefaultsWithoutAResolver(t *testing.T) {
	svc, mux := newHTTPHarness(t, nil)
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.site_name", Value{Data: "Global Co"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}

	// With no resolver wired, every request reads the platform tier; none
	// of them may fail.
	resp := doRequest(t, mux, http.MethodGet, PathPublic, "anything.example.com")
	if resp.recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", PathPublic, resp.recorder.Code)
	}
	var body publicSnapshotBody
	decodeBody(t, resp, &body)
	if body.Config["brand.site_name"] != "Global Co" {
		t.Fatalf("served brand.site_name = %#v, want the platform row", body.Config["brand.site_name"])
	}
}

func TestHTTP_Public_ServesCanonicalWireValuesOnly(t *testing.T) {
	svc, mux := newHTTPHarness(t, staticHostResolver{"studio-a.example.com": "tenant-a"})
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.welcome_interval", Value{Data: 2 * time.Minute}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	if err := svc.Set(tenantA(), ScopeTenant, "support.reply_email", Value{Data: "ops@example.com"}, "alice"); err != nil {
		t.Fatalf("sensitive Set: %v", err)
	}

	resp := doRequest(t, mux, http.MethodGet, PathPublic, "studio-a.example.com")
	if resp.recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", PathPublic, resp.recorder.Code)
	}
	var body publicSnapshotBody
	decodeBody(t, resp, &body)
	// Durations render as canonical "2m0s" text, never a nanosecond int.
	if body.Config["brand.welcome_interval"] != "2m0s" {
		t.Fatalf("served brand.welcome_interval = %#v (%T), want %q", body.Config["brand.welcome_interval"], body.Config["brand.welcome_interval"], "2m0s")
	}
	// A Sensitive value never reaches the wire: neither its key nor its
	// plaintext appears anywhere in the raw body.
	for _, needle := range []string{"support.reply_email", "ops@example.com"} {
		if bytes.Contains(resp.body, []byte(needle)) {
			t.Fatalf("the raw response body leaks %q: %s", needle, resp.body)
		}
	}
}

func TestHTTP_Public_ReportsEnabledFlagsAndEmptyAsArray(t *testing.T) {
	svc, mux := newHTTPHarness(t, staticHostResolver{
		"studio-a.example.com": "tenant-a",
		"studio-b.example.com": "tenant-b",
	})

	// No flag enabled: the features member must marshal as JSON's [] --
	// the documented array shape -- never as null (regression test for the
	// nil-slice encoding).
	resp := doRequest(t, mux, http.MethodGet, PathPublic, "studio-a.example.com")
	if !bytes.Contains(resp.body, []byte(`"features":[]`)) {
		t.Fatalf("empty features did not marshal as []: %s", resp.body)
	}
	var body publicSnapshotBody
	decodeBody(t, resp, &body)
	if len(body.Features) != 0 {
		t.Fatalf("features = %v, want none while the chain is off", body.Features)
	}

	if err := svc.Set(tenantA(), ScopeTenant, "ai.smile_preview", Value{Data: true}, "alice"); err != nil {
		t.Fatalf("tenant-a Set: %v", err)
	}

	// Tenant a sees the enabled chain; tenant b's endpoint still reports
	// nothing; the features-only endpoint serves the same list for tenant a.
	want := []string{"ai.premium_upsell", "ai.smile_preview"}
	resp = doRequest(t, mux, http.MethodGet, PathPublic, "studio-a.example.com")
	decodeBody(t, resp, &body)
	if !equalStrings(body.Features, want) {
		t.Fatalf("public features for tenant a = %v, want %v", body.Features, want)
	}

	resp = doRequest(t, mux, http.MethodGet, PathSystemFeatures, "studio-a.example.com")
	if resp.recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", PathSystemFeatures, resp.recorder.Code)
	}
	var featuresOnly map[string][]string
	decodeBody(t, resp, &featuresOnly)
	if !equalStrings(featuresOnly["features"], want) {
		t.Fatalf("features endpoint for tenant a = %v, want %v", featuresOnly["features"], want)
	}

	resp = doRequest(t, mux, http.MethodGet, PathSystemFeatures, "studio-b.example.com")
	decodeBody(t, resp, &featuresOnly)
	if len(featuresOnly["features"]) != 0 {
		t.Fatalf("features endpoint for tenant b = %v, want none", featuresOnly["features"])
	}
}

// equalStrings compares two slices order-insensitively of contents -- here
// both are sorted, so an ordered comparison would do -- but a copy-paste of
// one slice into the other must not be masked by aliasing, hence the
// elementwise check.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestHTTP_MethodGating_AdmitsGetAndHeadOnly(t *testing.T) {
	_, mux := newHTTPHarness(t, nil)

	for _, path := range []string{PathPublic, PathSystemFeatures} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			resp := doRequest(t, mux, method, path, "anything.example.com")
			if resp.recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d, want 405", method, path, resp.recorder.Code)
			}
			if allow := resp.recorder.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Fatalf("%s %s Allow = %q, want %q", method, path, allow, "GET, HEAD")
			}
			if code := decodeErrorCode(t, resp); code != "config.method_not_allowed" {
				t.Fatalf("%s %s error code = %q, want %q", method, path, code, "config.method_not_allowed")
			}
		}
	}

	get := doRequest(t, mux, http.MethodGet, PathPublic, "anything.example.com")
	if get.recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", PathPublic, get.recorder.Code)
	}
	if ct := get.recorder.Header().Get("Content-Type"); ct != jsonContentType {
		t.Fatalf("GET %s Content-Type = %q, want %q", PathPublic, ct, jsonContentType)
	}
	head := doRequest(t, mux, http.MethodHead, PathPublic, "anything.example.com")
	if head.recorder.Code != http.StatusOK {
		t.Fatalf("HEAD %s = %d, want 200", PathPublic, head.recorder.Code)
	}
}

func TestHTTP_Endpoints_ReportTheServiceNotAttachedWindow(t *testing.T) {
	// A module that registered but never attached -- the wiring gap between
	// the two Bootstrap/Attach steps -- must answer with the structured
	// internal error, not a nil-pointer crash.
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Config.Add(serviceTestSchemaItems...); err != nil {
		t.Fatalf("reg.Config.Add: %v", err)
	}
	if err := reg.Features.Add(serviceTestSchemaFlags...); err != nil {
		t.Fatalf("reg.Features.Add: %v", err)
	}
	if err := NewModule(openHTTPTestDB(t), WithPollInterval(0)).Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mux := mountRoutes(reg)

	for _, path := range []string{PathPublic, PathSystemFeatures} {
		resp := doRequest(t, mux, http.MethodGet, path, "anything.example.com")
		if resp.recorder.Code != http.StatusInternalServerError {
			t.Fatalf("GET %s before Attach = %d, want 500", path, resp.recorder.Code)
		}
		if code := decodeErrorCode(t, resp); code != ErrServiceNotAttached.Code {
			t.Fatalf("GET %s before Attach error code = %q, want %q", path, code, ErrServiceNotAttached.Code)
		}
	}
}
