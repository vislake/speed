package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

func newTestGuard(t *testing.T, limits LayeredLimits, extract Extractor) *HTTPGuard {
	t.Helper()
	limiter := newTestLayeredLimiter(limits)
	return NewHTTPGuard(limiter, "global", extract)
}

func fixedExtractor(tenantKey, apiKeyID string) Extractor {
	return func(r *http.Request) (string, string) { return tenantKey, apiKeyID }
}

func TestHTTPGuard_Middleware_Allowed_CallsNext(t *testing.T) {
	g := newTestGuard(t, LayeredLimits{Key: ratelimit.Limit{Rate: 10, Per: minute}}, fixedExtractor("t1", "k1"))

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	g.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Error("next was never called for an allowed request")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestHTTPGuard_Middleware_Denied_Returns429WithHeaders(t *testing.T) {
	g := newTestGuard(t, LayeredLimits{Key: ratelimit.Limit{Rate: 1, Per: minute}}, fixedExtractor("t1", "k1"))

	firstCalled := false
	secondCalled := false
	called := &firstCalled
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := g.Middleware(next)

	// First request consumes the single-hit budget and must reach next.
	firstRec := httptest.NewRecorder()
	mw.ServeHTTP(firstRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !firstCalled || firstRec.Code != http.StatusOK {
		t.Fatalf("first request: called=%v status=%d, want called=true status=200", firstCalled, firstRec.Code)
	}

	// Second request must be denied without reaching next.
	called = &secondCalled
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if secondCalled {
		t.Error("next was called on the request that should have been denied")
	}

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header is empty, want a positive value")
	}
	if rec.Header().Get(headerRateLimitLayer) != LayerKey {
		t.Errorf("%s = %q, want %q", headerRateLimitLayer, rec.Header().Get(headerRateLimitLayer), LayerKey)
	}
	if rec.Header().Get(headerRateLimitReset) == "" {
		t.Error("X-RateLimit-Reset header is empty")
	}
	if rec.Header().Get("Content-Type") != errorContentType {
		t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), errorContentType)
	}

	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != ErrRateLimited.Code {
		t.Errorf("body.Code = %q, want %q", body.Code, ErrRateLimited.Code)
	}
}

// TestHTTPGuard_Middleware_LimiterError_Returns500 proves an underlying
// Limiter failure never falls through to next -- go/ratelimit.Limiter's own
// doc comment is explicit that it decides no fail-open policy on the
// caller's behalf, and HTTPGuard.Middleware must honor that rather than
// treating "the limiter itself errored" as an implicit allow.
func TestHTTPGuard_Middleware_LimiterError_Returns500(t *testing.T) {
	// An invalid Limit (negative Per) makes the underlying Allow call fail
	// deterministically, with no store or network involved.
	g := newTestGuard(t, LayeredLimits{Global: ratelimit.Limit{Rate: 10, Per: -1}}, fixedExtractor("t1", "k1"))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next must not be called when the limiter itself errors")
	})

	rec := httptest.NewRecorder()
	g.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestWriteAppError_NonAppError_WritesInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAppError(rec, context.DeadlineExceeded)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	var body errorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != ErrInternal.Code {
		t.Errorf("body.Code = %q, want %q", body.Code, ErrInternal.Code)
	}
}

func TestWithRateLimitParams_CarriesLayerAndRetryAfter(t *testing.T) {
	decision := LayeredDecision{Layer: LayerTenant, Decision: ratelimit.Decision{Remaining: 0, ResetAfter: 0}}
	err := WithRateLimitParams(decision)

	found, ok := apperr.As(err)
	if !ok {
		t.Fatal("WithRateLimitParams did not return an *apperr.Error")
	}
	if found.Params["layer"] != LayerTenant {
		t.Errorf(`Params["layer"] = %v, want %q`, found.Params["layer"], LayerTenant)
	}
	if _, ok := found.Params["retry_after_seconds"].(int); !ok {
		t.Errorf(`Params["retry_after_seconds"] = %v, want an int`, found.Params["retry_after_seconds"])
	}
}
