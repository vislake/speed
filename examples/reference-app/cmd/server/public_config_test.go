package main

// The config module's public endpoints, proven through the real composed
// server: tenancy.Middleware with its allowlist, the config module's own
// handler and its DomainResolver, the frozen schema the notes module's
// Register declarations folded into Attach, and the real SQLite database
// the module polls and reads. The notes-side CRUD proof lives in
// server_test.go; these tests cover the other half of buildServer's wiring
// -- the pre-auth display surface, where the tenancy rules are the inverse
// of the CRUD side (unmatched hosts must still render platform defaults,
// never 403), which is exactly what the strictHostResolver fail-closed
// behavior that protects the notes API must NOT do here.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// configSeed is one row to insert into the configs table behind a test
// server. buildServer hands out neither its *gorm.DB nor the config
// Service, so the raw table is the only reach a test has into the module's
// storage -- which is fine, because the tier resolution the endpoints run
// on (tenant row -> system row -> schema default) is exactly what these
// tests mean to exercise, and the schema itself came from the notes
// module's real Register declarations.
type configSeed struct {
	key      string
	scope    config.Scope
	tenantID string
	value    string
}

// buildSeededTestServer builds the real buildServer stack over a fresh
// temp-file database, inserts seeds into the configs table through a
// second connection to the same SQLite file, and returns the running
// server. Every request after seeding is the first this process has made
// for the seeded keys, so the service's cache misses and reads the seeded
// row straight from the database: no warm-up ordering hazard, because the
// seeds land before any request could have cached anything else.
//
// A caveat the tests below rely on in reverse: rows seeded AFTER a request
// has already cached the same (key, scope, tenant) triple would not be
// seen until the 30s anti-loss poller swept them in. Every test here seeds
// before its first request, so the cache starts cold.
func buildSeededTestServer(t *testing.T, seeds ...configSeed) *httptest.Server {
	t.Helper()

	cfg := testConfig(t)
	handler, cleanup, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	if len(seeds) > 0 {
		seedConfigRows(t, cfg, seeds)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// seedConfigRows inserts seeds through a second dbkit.Open connection to
// the same SQLite file the server writes. updated_at is bound as a
// time.Time so the driver serializes it in exactly the layout the server's
// reads parse back -- symmetric with the module's own writes rather than a
// hand-typed text format that could drift from what gorm expects.
func seedConfigRows(t *testing.T, cfg serverConfig, seeds []configSeed) {
	t.Helper()

	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		t.Fatalf("open seed connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			t.Errorf("seed connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close seed connection: %v", closeErr)
		}
	})

	for _, seed := range seeds {
		res := db.Exec(
			"INSERT INTO configs (key, scope, tenant_id, value, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			seed.key, seed.scope, seed.tenantID, seed.value, "e2e-test", time.Now(),
		)
		if res.Error != nil {
			t.Fatalf("seed row key=%q scope=%q tenant=%q: %v", seed.key, seed.scope, seed.tenantID, res.Error)
		}
	}
}

// testPublicConfigResponse is the wire shape of config.PathPublic: every
// Public item's effective value plus the enabled feature flags
// (go/config/http.go's handlePublic doc comment).
type testPublicConfigResponse struct {
	Config   map[string]any `json:"config"`
	Features []string       `json:"features"`
}

// testFeaturesResponse is the wire shape of config.PathSystemFeatures.
type testFeaturesResponse struct {
	Features []string `json:"features"`
}

// doAs runs one request against srv with the given method, path and Host,
// returning the response with its body fully read and closed. Host is set
// the way a real client's would be -- it is the only thing that selects
// the tenant on either side of this app's middleware stack.
func doAs(t *testing.T, srv *httptest.Server, method, path, host string, body io.Reader) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, path, err)
	}
	req.Host = host

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (Host=%s): %v", method, path, host, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s (Host=%s) body: %v", method, path, host, err)
	}
	return resp, respBody
}

// requirePublicSnapshot asserts the common shape every successful
// /api/config/public response carries: exactly the one Public item this
// app's notes module registers (brand.site_name), at the given value; the
// sensitive support.reply_email nowhere in the payload, not even as a key;
// and the given feature list.
func requirePublicSnapshot(t *testing.T, body []byte, wantSiteName string, wantFeatures []string) {
	t.Helper()

	if strings.Contains(string(body), notes.ConfigKeySupportReplyEmail) {
		t.Fatalf("public response body mentions the Sensitive key %q; a Sensitive item must never reach the public endpoint", notes.ConfigKeySupportReplyEmail)
	}

	var out testPublicConfigResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode public response %q: %v", body, err)
	}
	if len(out.Config) != 1 {
		t.Fatalf("public config has %d keys, want exactly 1 (%q); got %v", len(out.Config), notes.ConfigKeyBrandSiteName, out.Config)
	}
	siteName, ok := out.Config[notes.ConfigKeyBrandSiteName].(string)
	if !ok {
		t.Fatalf("public config %q = %v (%T), want a string", notes.ConfigKeyBrandSiteName, out.Config[notes.ConfigKeyBrandSiteName], out.Config[notes.ConfigKeyBrandSiteName])
	}
	if siteName != wantSiteName {
		t.Errorf("public config %q = %q, want %q", notes.ConfigKeyBrandSiteName, siteName, wantSiteName)
	}
	if len(out.Features) != len(wantFeatures) {
		t.Fatalf("features = %v, want %v", out.Features, wantFeatures)
	}
	for i, want := range wantFeatures {
		if out.Features[i] != want {
			t.Fatalf("features = %v, want %v", out.Features, wantFeatures)
		}
	}
}

// requireFeatures asserts the shape of a successful /api/system/features
// response.
func requireFeatures(t *testing.T, body []byte, want []string) {
	t.Helper()

	var out testFeaturesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode features response %q: %v", body, err)
	}
	if len(out.Features) != len(want) {
		t.Fatalf("features = %v, want %v", out.Features, want)
	}
	for i, w := range want {
		if out.Features[i] != w {
			t.Fatalf("features = %v, want %v", out.Features, want)
		}
	}
}

// TestPublicConfig_UnmatchedHost_ServesPlatformDefaults pins the heart of
// the config endpoints' tenancy rule: a Host no tenant maps to -- which
// strictHostResolver would 403 for the notes API -- must still get 200 and
// the platform defaults (the schema defaults the notes module declared:
// brand.site_name "Smile Studio"), never an error. It also pins the shape
// of that default snapshot: no Sensitive key anywhere, and no feature
// enabled -- ai.premium_upsell declares a dependency on ai.smile_preview,
// whose platform default is false, so the dependency chain must resolve
// the upsell flag off even though its own default is true.
func TestPublicConfig_UnmatchedHost_ServesPlatformDefaults(t *testing.T) {
	srv := buildSeededTestServer(t)

	const unmatchedHost = "totally-unrecognized-host.example"
	resp, body := doAs(t, srv, http.MethodGet, config.PathPublic, unmatchedHost, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (Host=%s) status = %d, want %d; body = %s",
			config.PathPublic, unmatchedHost, resp.StatusCode, http.StatusOK, body)
	}
	requirePublicSnapshot(t, body, "Smile Studio", []string{})

	// The features endpoint answers the same way for the same host: 200,
	// platform defaults, empty flag list.
	resp2, body2 := doAs(t, srv, http.MethodGet, config.PathSystemFeatures, unmatchedHost, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (Host=%s) status = %d, want %d; body = %s",
			config.PathSystemFeatures, unmatchedHost, resp2.StatusCode, http.StatusOK, body2)
	}
	requireFeatures(t, body2, []string{})
}

// TestPublicConfig_MatchedHost_ServesTenantOverrides proves the tenant
// tier of the config resolution end to end: a seeded override for
// tenant-acme's brand.site_name is served to the acme host, while the
// globex host and an unmatched host both keep falling back to the platform
// default -- the same row the strictHostResolver-side tests in
// server_test.go prove for notes data, here through the config module's
// own read path.
func TestPublicConfig_MatchedHost_ServesTenantOverrides(t *testing.T) {
	const acmeHost = "acme.demo.localhost"
	const globexHost = "globex.demo.localhost"

	srv := buildSeededTestServer(t, configSeed{
		key:      notes.ConfigKeyBrandSiteName,
		scope:    config.ScopeTenant,
		tenantID: "tenant-acme",
		value:    "Acme Studio",
	})

	resp, body := doAs(t, srv, http.MethodGet, config.PathPublic, acmeHost, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (Host=%s) status = %d, want %d; body = %s",
			config.PathPublic, acmeHost, resp.StatusCode, http.StatusOK, body)
	}
	requirePublicSnapshot(t, body, "Acme Studio", []string{})

	// tenant-globex has no override row: it must fall back to the platform
	// default, not to acme's override -- and the unmatched host likewise.
	_, globexBody := doAs(t, srv, http.MethodGet, config.PathPublic, globexHost, nil)
	requirePublicSnapshot(t, globexBody, "Smile Studio", []string{})

	const unmatchedHost = "totally-unrecognized-host.example"
	_, unmatchedBody := doAs(t, srv, http.MethodGet, config.PathPublic, unmatchedHost, nil)
	requirePublicSnapshot(t, unmatchedBody, "Smile Studio", []string{})
}

// TestSystemFeatures_EnabledFlagChain_ResolvesDependencies proves the
// feature-flag dependency chain end to end: with ai.smile_preview seeded
// on for tenant-acme only, the acme host's flag list must contain BOTH
// ai.smile_preview and its dependent ai.premium_upsell (sorted ascending),
// while globex -- no override row -- stays at the platform defaults where
// the chain resolves everything off.
func TestSystemFeatures_EnabledFlagChain_ResolvesDependencies(t *testing.T) {
	const acmeHost = "acme.demo.localhost"
	const globexHost = "globex.demo.localhost"

	srv := buildSeededTestServer(t, configSeed{
		key:      notes.FeatureFlagSmilePreview,
		scope:    config.ScopeTenant,
		tenantID: "tenant-acme",
		value:    "true",
	})

	resp, body := doAs(t, srv, http.MethodGet, config.PathSystemFeatures, acmeHost, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (Host=%s) status = %d, want %d; body = %s",
			config.PathSystemFeatures, acmeHost, resp.StatusCode, http.StatusOK, body)
	}
	requireFeatures(t, body, []string{notes.FeatureFlagPremiumUpsell, notes.FeatureFlagSmilePreview})

	_, globexBody := doAs(t, srv, http.MethodGet, config.PathSystemFeatures, globexHost, nil)
	requireFeatures(t, globexBody, []string{})
}

// TestPublicConfigEndpoints_Head_Returns200WithoutBody proves the HEAD
// half of the middleware allowlist actually reaches the config handlers:
// net/http serves HEAD off the mux for every path, but tenancy.Middleware
// only lets allowlisted methods through (its own doc comment: allowlist
// http.MethodHead explicitly if HEAD must work) -- buildServer allowlists
// GET and HEAD for both config paths, so both must answer 200 with an
// empty body.
func TestPublicConfigEndpoints_Head_Returns200WithoutBody(t *testing.T) {
	srv := buildSeededTestServer(t)
	const acmeHost = "acme.demo.localhost"

	for _, path := range []string{config.PathPublic, config.PathSystemFeatures} {
		resp, body := doAs(t, srv, http.MethodHead, path, acmeHost, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD %s (Host=%s) status = %d, want %d", path, acmeHost, resp.StatusCode, http.StatusOK)
		}
		if len(body) != 0 {
			t.Fatalf("HEAD %s returned %d body bytes, want 0", path, len(body))
		}
	}
}

// TestPublicConfigEndpoints_NonGetMethod_FailsClosed proves the method
// contract of both config endpoints: only GET and HEAD are allowlisted
// through tenancy.Middleware, so a POST with a resolvable Host reaches the
// config handler and gets its 405 with the Allow header, while a POST with
// an unresolvable Host is stopped by strictHostResolver's 403 before the
// handler is ever consulted. The allowlist exemption is scoped to exactly
// the read methods a pre-auth display surface needs -- everything else
// stays fail-closed behind the same resolver that guards the notes API.
func TestPublicConfigEndpoints_NonGetMethod_FailsClosed(t *testing.T) {
	srv := buildSeededTestServer(t)
	const acmeHost = "acme.demo.localhost"
	const unmatchedHost = "totally-unrecognized-host.example"

	for _, path := range []string{config.PathPublic, config.PathSystemFeatures} {
		resp, body := doAs(t, srv, http.MethodPost, path, acmeHost, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s (Host=%s) status = %d, want %d; body = %s",
				path, acmeHost, resp.StatusCode, http.StatusMethodNotAllowed, body)
		}
		if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("POST %s Allow header = %q, want %q", path, allow, "GET, HEAD")
		}

		resp2, body2 := doAs(t, srv, http.MethodPost, path, unmatchedHost, nil)
		if resp2.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (Host=%s) status = %d, want %d (strictHostResolver must fail closed before the config handler); body = %s",
				path, unmatchedHost, resp2.StatusCode, http.StatusForbidden, body2)
		}
	}
}
