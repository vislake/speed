package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/config/api"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pkgcore/httpapi"
	"github.com/vislake/speed/go/tenancy"
)

// Tests for http.go's two pre-auth endpoints: the host-resolved tenant
// fallback (custom domain to platform defaults, never an error), the public
// snapshot's wire shape (canonical durations, no Sensitive key, features as
// an array even when empty), the GET/HEAD-only method contract with its
// Allow header, the service-not-attached window's structured error, the
// per-address rate-limit budget (spent by either endpoint, refused with the
// module's own error shape, failing closed on a store that cannot answer),
// the endpoints' wire paths, and the caching contract every response
// declares -- a one-minute public freshness lifetime on a successful answer,
// no-store on every refusal.

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
// the way a host mounts module routes on its own router: the exact path plus
// its subtree variant, since MountedRoute's Handler "serves every request
// below Path" and net/http serves a bare exact-path request directly only
// when the exact pattern is registered (see the reference app's
// mountModuleRoutes).
func mountRoutes(reg *pkgcore.ComponentRegistry) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range reg.Routes.Routes() {
		mux.Handle(route.Path, route.Handler)
		if !strings.HasSuffix(route.Path, "/") {
			mux.Handle(route.Path+"/", route.Handler)
		}
	}
	return mux
}

// newHTTPHarnessWithItems registers (with the given item/flag schema) and
// attaches a config module over an in-memory configs table and the given
// KVStore -- the pre-auth rate-limit check's backend, so a test can hand
// the module a store that misbehaves -- and returns the attached service,
// so tests can write rows the way the platform writes them, and the
// mounted mux the requests hit.
func newHTTPHarnessWithItems(t *testing.T, resolver tenancy.Resolver, items []pkgcore.ConfigItem, flags []pkgcore.FeatureFlag, kv pkgcore.KVStore) (*Service, *http.ServeMux) {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeSystemWrite)
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(kv)
	reg.Put(pkgcore.NewConsoleMailer())
	opts := []Option{WithCipher(buildTestCipher(t)), WithPollInterval(0)}
	if resolver != nil {
		opts = append(opts, WithResolver(resolver))
	}
	module := NewModule(openHTTPTestDB(t), opts...)
	// The whole declaration turn -- the schema, the module's Register, its
	// Attach and the route mounts -- runs inside one Init stage: every one
	// of those is a seat write the window owns.
	var svc *Service
	var mux *http.ServeMux
	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.Config.Add(items...) },
		func(r *pkgcore.ComponentRegistry) error { return r.Features.Add(flags...) },
		module.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := module.Attach(r)
			if attachErr != nil {
				return attachErr
			}
			svc = attached
			mux = mountRoutes(r)
			return nil
		},
	); err != nil {
		t.Fatalf("declare, attach and mount: %v", err)
	}
	return svc, mux
}

// newHTTPHarness is newHTTPHarnessWithItems over the shared item/flag
// schema and a working in-memory KVStore, the common case for the endpoint
// tests.
func newHTTPHarness(t *testing.T, resolver tenancy.Resolver) (*Service, *http.ServeMux) {
	t.Helper()
	return newHTTPHarnessWithItems(t, resolver, serviceTestSchemaItems, serviceTestSchemaFlags, pkgcore.NewMemoryKVStore())
}

// httpResponse pairs a recorder with its body bytes for the assertions
// below.
type httpResponse struct {
	recorder *httptest.ResponseRecorder
	body     []byte
}

func doRequest(t *testing.T, mux *http.ServeMux, method, path, host string) httpResponse {
	t.Helper()
	return doRequestFromIP(t, mux, method, path, host, defaultTestRemoteAddr)
}

// defaultTestRemoteAddr is the source address httptest.NewRequest stamps on
// every request it builds (TEST-NET-1). Tests that need distinct rate-limit
// budgets pass their own.
const defaultTestRemoteAddr = "192.0.2.1:1234"

// doRequestFromIP is doRequest with an explicit direct-connection address,
// which is what the pre-auth rate-limit budget is keyed on: a test spending
// one address's budget passes one value here, and a test proving another
// address is unaffected passes another.
func doRequestFromIP(t *testing.T, mux *http.ServeMux, method, path, host, remoteAddr string) httpResponse {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return httpResponse{recorder: rec, body: rec.Body.Bytes()}
}

// publicSnapshotBody is the wire shape ConfigGetPublicConfig documents: a
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

// decodeErrorEnvelope returns the whole structured refusal body -- the
// fragment's api.ConfigError, the shape both endpoint methods write -- so a
// test can assert on the params a denial carries and not only on its code.
func decodeErrorEnvelope(t *testing.T, resp httpResponse) api.ConfigError {
	t.Helper()
	var envelope api.ConfigError
	decodeBody(t, resp, &envelope)
	if envelope.Code == "" {
		t.Fatalf("error response carries no code: %s", resp.body)
	}
	return envelope
}

func decodeErrorCode(t *testing.T, resp httpResponse) string {
	t.Helper()
	return decodeErrorEnvelope(t, resp).Code
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

func TestHTTP_Public_OmitsAPublicItemWithNoValueAnywhere(t *testing.T) {
	// A host declaring a Public item without a Default -- legal, the module
	// serves no value until one is set -- must not take the endpoint down
	// with a 404 for every tenant while ops has not written the row: the
	// pre-auth display rule forbids an error here, and the login page must
	// render regardless of which Public items still lack values. The item
	// stays absent from the snapshot until its row exists.
	items := []pkgcore.ConfigItem{
		{Key: "brand.site_name", Type: "string", Default: "Smile Studio", Public: true, Description: "The tenant's display name", Group: "brand"},
		{Key: "brand.support_phone", Type: "string", Public: true, Description: "The tenant's support phone", Group: "brand"},
	}
	svc, mux := newHTTPHarnessWithItems(t, staticHostResolver{"studio-a.example.com": "tenant-a"}, items, nil, pkgcore.NewMemoryKVStore())

	resp := doRequest(t, mux, http.MethodGet, PathPublic, "studio-a.example.com")
	if resp.recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 while an unset Public item is declared (body: %s)",
			PathPublic, resp.recorder.Code, resp.body)
	}
	var body publicSnapshotBody
	decodeBody(t, resp, &body)
	if body.Config["brand.site_name"] != "Smile Studio" {
		t.Fatalf("served brand.site_name = %#v, want the schema default", body.Config["brand.site_name"])
	}
	if _, present := body.Config["brand.support_phone"]; present {
		t.Fatal("an item with no row and no default leaked into the public snapshot")
	}

	// Once ops writes the row, the item joins the snapshot.
	if err := svc.Set(systemWriteCtx(t), ScopeSystem, "brand.support_phone", Value{Data: "+1-555-0100"}, "ops-1"); err != nil {
		t.Fatalf("system Set: %v", err)
	}
	resp = doRequest(t, mux, http.MethodGet, PathPublic, "studio-a.example.com")
	if resp.recorder.Code != http.StatusOK {
		t.Fatalf("GET %s after the row landed = %d, want 200", PathPublic, resp.recorder.Code)
	}
	decodeBody(t, resp, &body)
	if body.Config["brand.support_phone"] != "+1-555-0100" {
		t.Fatalf("served brand.support_phone = %#v, want the written row", body.Config["brand.support_phone"])
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
	if ct := get.recorder.Header().Get("Content-Type"); ct != httpapi.JSONContentType {
		t.Fatalf("GET %s Content-Type = %q, want %q", PathPublic, ct, httpapi.JSONContentType)
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
	reg := componenttest.NewRegistry()
	module := NewModule(openHTTPTestDB(t), WithPollInterval(0))
	var mux *http.ServeMux
	if err := componenttest.DeclareAll(reg,
		func(r *pkgcore.ComponentRegistry) error { return r.Config.Add(serviceTestSchemaItems...) },
		func(r *pkgcore.ComponentRegistry) error { return r.Features.Add(serviceTestSchemaFlags...) },
		module.Register,
		func(r *pkgcore.ComponentRegistry) error {
			mux = mountRoutes(r)
			return nil
		},
	); err != nil {
		t.Fatalf("declare and mount: %v", err)
	}

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

// TestHTTP_PreAuthEndpoints_ServeUnderTheModuleScopedVersionedPrefix pins
// the endpoints' wire identity. These strings are the actual contract: a
// host's allowlist reaches them through the exported constants, and a
// browser or a frontend path mirror addresses them literally, so the
// versioned, module-scoped prefix is asserted here next to the constants
// that carry it.
func TestHTTP_PreAuthEndpoints_ServeUnderTheModuleScopedVersionedPrefix(t *testing.T) {
	if PathPublic != "/api/v1/config/public" {
		t.Fatalf("PathPublic = %q, want %q", PathPublic, "/api/v1/config/public")
	}
	if PathSystemFeatures != "/api/v1/config/features" {
		t.Fatalf("PathSystemFeatures = %q, want %q", PathSystemFeatures, "/api/v1/config/features")
	}
	for _, path := range []string{PathPublic, PathSystemFeatures} {
		if !strings.HasPrefix(path, "/api/v1/config/") {
			t.Fatalf("path %q is outside the module's versioned prefix %q", path, "/api/v1/config/")
		}
	}
}

// TestHTTP_PreAuthEndpoints_ServeOnlyTheirDeclaredPaths pins the routing the
// fragment's generated wrapper gives the module: the handler answers exactly
// the two literal paths api/openapi.yaml declares. A request below a mounted
// prefix -- the subtree variant mountRoutes registers alongside each route --
// is therefore refused as not found rather than answered with a snapshot,
// so the fragment, not the handler's own dispatch, is what defines the
// endpoints' surface.
func TestHTTP_PreAuthEndpoints_ServeOnlyTheirDeclaredPaths(t *testing.T) {
	_, mux := newHTTPHarness(t, nil)
	for _, path := range []string{PathPublic + "/", PathSystemFeatures + "/", PathPublic + "/extra"} {
		resp := doRequest(t, mux, http.MethodGet, path, "studio-a.example.com")
		if resp.recorder.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404: the fragment declares two paths and the handler serves only those (body: %s)",
				path, resp.recorder.Code, resp.body)
		}
	}
	// The declared paths themselves still answer, which is what makes the
	// refusals above a statement about the surface and not about the mount.
	for _, path := range []string{PathPublic, PathSystemFeatures} {
		resp := doRequest(t, mux, http.MethodGet, path, "studio-a.example.com")
		if resp.recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (body: %s)", path, resp.recorder.Code, resp.body)
		}
	}
}

func TestHTTP_PreAuthEndpoints_RefuseOnceTheAddressBudgetIsSpent(t *testing.T) {
	_, mux := newHTTPHarness(t, nil)
	const spent = "203.0.113.7:5555"

	// Fill the address's budget, alternating endpoints: both draw on one
	// key, so a client cannot split its budget in two by fetching the
	// snapshot and the flag list as separate requests.
	for i := 0; i < preAuthPerIPRate; i++ {
		path := PathPublic
		if i%2 == 1 {
			path = PathSystemFeatures
		}
		resp := doRequestFromIP(t, mux, http.MethodGet, path, "studio-a.example.com", spent)
		if resp.recorder.Code != http.StatusOK {
			t.Fatalf("request %d/%d (GET %s) = %d, want 200 inside the budget (body: %s)",
				i+1, preAuthPerIPRate, path, resp.recorder.Code, resp.body)
		}
	}

	// The request that pushes the address over its budget is the one
	// refused, on either endpoint, with the module's own error shape: the
	// dimension that tripped, the window's recovery time, and a refusal
	// that is not cacheable.
	for _, path := range []string{PathPublic, PathSystemFeatures} {
		resp := doRequestFromIP(t, mux, http.MethodGet, path, "studio-a.example.com", spent)
		if resp.recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("GET %s past the budget = %d, want 429 (body: %s)", path, resp.recorder.Code, resp.body)
		}
		envelope := decodeErrorEnvelope(t, resp)
		if envelope.Code != ErrRateLimited.Code {
			t.Fatalf("GET %s past the budget error code = %q, want %q", path, envelope.Code, ErrRateLimited.Code)
		}
		if envelope.Params == nil {
			t.Fatalf("GET %s past the budget carried no params: %s", path, resp.body)
		}
		params := *envelope.Params
		if dimension := params["dimension"]; dimension != "ip" {
			t.Fatalf("GET %s past the budget reported dimension %v, want %q", path, dimension, "ip")
		}
		if retry, ok := params["retry_after_seconds"].(float64); !ok || retry <= 0 {
			t.Fatalf("GET %s past the budget reported retry_after_seconds %v, want a positive whole-second wait",
				path, params["retry_after_seconds"])
		}
		if cc := resp.recorder.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("GET %s past the budget Cache-Control = %q, want %q", path, cc, "no-store")
		}
	}

	// The budget belongs to the address that spent it: another caller's
	// requests are unaffected, which is the property that keeps one
	// over-budget address from holding anyone else hostage.
	resp := doRequestFromIP(t, mux, http.MethodGet, PathPublic, "studio-a.example.com", "198.51.100.9:5555")
	if resp.recorder.Code != http.StatusOK {
		t.Fatalf("GET %s from a second address = %d, want 200 (body: %s)", PathPublic, resp.recorder.Code, resp.body)
	}
}

// errKVStore is a KVStore whose write path always fails: the rate-limit
// check's store round trip is the first thing the limiter performs, so this
// is exactly a limiter that cannot answer. Every other method delegates to
// a working in-memory store, keeping the failure the limiter's own.
type errKVStore struct {
	pkgcore.KVStore
	err error
}

func (s errKVStore) IncrByFloatWithTTL(context.Context, string, float64, time.Duration) (float64, error) {
	return 0, s.err
}

func TestHTTP_PreAuthEndpoints_FailClosedWhenTheStoreCannotAnswer(t *testing.T) {
	// A limiter that cannot answer must never read as "allow". The check is
	// the only throttle these endpoints have, so a store outage that opened
	// the gate would leave exactly the request volume it bounds unguarded
	// for the outage's duration: the caller gets the module's internal
	// error and no answer.
	_, mux := newHTTPHarnessWithItems(t, nil, serviceTestSchemaItems, serviceTestSchemaFlags,
		errKVStore{KVStore: pkgcore.NewMemoryKVStore(), err: errors.New("kvstore is down")})

	for _, path := range []string{PathPublic, PathSystemFeatures} {
		resp := doRequest(t, mux, http.MethodGet, path, "anything.example.com")
		if resp.recorder.Code != http.StatusInternalServerError {
			t.Fatalf("GET %s with a failing limiter store = %d, want 500 (body: %s)", path, resp.recorder.Code, resp.body)
		}
		if code := decodeErrorCode(t, resp); code != ErrStorage.Code {
			t.Fatalf("GET %s with a failing limiter store error code = %q, want %q", path, code, ErrStorage.Code)
		}
		if cc := resp.recorder.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("GET %s with a failing limiter store Cache-Control = %q, want %q", path, cc, "no-store")
		}
	}
}

// TestHTTP_PreAuthEndpoints_DeclareTheCachingContract asserts the header
// half of the module's caching decision: a successful answer on either
// endpoint, for either admitted method, declares exactly the pinned
// freshness lifetime and the host dimension the answer varies on, while a
// refusal says the opposite -- a cached 429 would keep refusing past the
// window it announced, and a cached 405 or 500 would hide recovery.
func TestHTTP_PreAuthEndpoints_DeclareTheCachingContract(t *testing.T) {
	_, mux := newHTTPHarness(t, nil)
	if publicCacheMaxAge <= 0 || publicCacheMaxAgeSeconds <= 0 {
		t.Fatalf("the pinned freshness lifetime must be positive, got %v (%d whole seconds)",
			publicCacheMaxAge, publicCacheMaxAgeSeconds)
	}
	wantCacheControl := fmt.Sprintf("public, max-age=%d", publicCacheMaxAgeSeconds)

	for _, path := range []string{PathPublic, PathSystemFeatures} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			resp := doRequest(t, mux, method, path, "studio-a.example.com")
			if resp.recorder.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200", method, path, resp.recorder.Code)
			}
			if cc := resp.recorder.Header().Get("Cache-Control"); cc != wantCacheControl {
				t.Fatalf("%s %s Cache-Control = %q, want %q", method, path, cc, wantCacheControl)
			}
			if vary := resp.recorder.Header().Get("Vary"); vary != "Host" {
				t.Fatalf("%s %s Vary = %q, want %q (the answer is resolved for the request's host)",
					method, path, vary, "Host")
			}
		}
	}

	resp := doRequest(t, mux, http.MethodPost, PathPublic, "studio-a.example.com")
	if resp.recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST %s = %d, want 405", PathPublic, resp.recorder.Code)
	}
	if cc := resp.recorder.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("POST %s Cache-Control = %q, want %q for a method refusal", PathPublic, cc, "no-store")
	}
}

// TestClientIP_KeepsAnAddressItCannotSplit pins the fallback of the
// rate-limit key's extraction: a RemoteAddr a real server always writes as
// "IP:port", but one it cannot split is still key material -- the check
// must count such requests under the address as given rather than collapse
// every one of them into a single empty key, which is also what the
// documented empty-address case (ratelimit.go) describes at the Service
// seam.
func TestClientIP_KeepsAnAddressItCannotSplit(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, PathPublic, nil)
	req.RemoteAddr = "203.0.113.7"
	if got := clientIP(req); got != "203.0.113.7" {
		t.Fatalf("clientIP(%q) = %q, want the address unchanged", req.RemoteAddr, got)
	}

	req.RemoteAddr = "[2001:db8::1]:443"
	if got := clientIP(req); got != "2001:db8::1" {
		t.Fatalf("clientIP(%q) = %q, want the host half with the port stripped", req.RemoteAddr, got)
	}
}
