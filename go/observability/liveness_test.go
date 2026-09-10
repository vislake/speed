package observability_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	obs "github.com/vislake/speed/go/observability"
)

func TestHealthzHandler_AlwaysAnswers200Ok(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		rec := httptest.NewRecorder()
		obs.HealthzHandler().ServeHTTP(rec, httptest.NewRequest(method, obs.HealthzPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200 regardless of method", method, obs.HealthzPath, rec.Code)
		}
		// A HEAD response's body is stripped by a real server, not by the
		// handler, so the body is only asserted for the methods that carry
		// one.
		if method == http.MethodGet && rec.Body.String() != "ok" {
			t.Fatalf("GET %s body = %q, want %q", obs.HealthzPath, rec.Body.String(), "ok")
		}
	}
}

func TestMountLiveness_ServesTheLivenessRouteSetAsGetPatterns(t *testing.T) {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, obs.HealthzPath, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET %s = (%d, %q), want (200, %q)", obs.HealthzPath, rec.Code, rec.Body.String(), "ok")
	}

	// ServeMux serves HEAD from a registered GET pattern automatically.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, obs.HealthzPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD %s = %d, want 200: ServeMux serves HEAD from the GET pattern", obs.HealthzPath, rec.Code)
	}

	// Any other method is refused by the method-scoped pattern itself.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, obs.HealthzPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST %s = %d, want 405 from the GET-scoped pattern", obs.HealthzPath, rec.Code)
	}

	// The metrics route delegates to MetricsHandler() at REQUEST time, so
	// it serves whatever the handler currently is -- whatever that is in
	// this test binary, it must be the same thing a direct call produces.
	want := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(want, httptest.NewRequest(http.MethodGet, obs.MetricsPath, nil))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, obs.MetricsPath, nil))
	if rec.Code != want.Code || rec.Body.String() != want.Body.String() {
		t.Fatalf("GET %s = (%d, %q), want the current MetricsHandler's answer (%d, %q)",
			obs.MetricsPath, rec.Code, rec.Body.String(), want.Code, want.Body.String())
	}
}
