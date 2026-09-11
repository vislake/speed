package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/spa"
)

// staticHandler answers every request with body.
func staticHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})
}

// serveTo drives the composed handler with one request.
func serveTo(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestNew_MountsLivenessModuleRoutesAndExtraRoutes pins the mux's three route
// sources composing together: the platform liveness endpoint, a route the
// module set mounted, and a route the host mounted through ExtraRoutes.
func TestNew_MountsLivenessModuleRoutesAndExtraRoutes(t *testing.T) {
	var host testHostConfig

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{&testModule{
				name:      "probe",
				routePath: "/api/v1/probe",
				handler:   staticHandler("probe"),
			}}, nil
		}),
		WithHTTP(HTTPSpec{ExtraRoutes: []pkgcore.MountedRoute{
			{Path: "/api/v1/host", Handler: staticHandler("host")},
		}}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	for _, tc := range []struct{ path, want string }{
		{"/healthz", ""},
		{"/api/v1/probe", "probe"},
		{"/api/v1/host", "host"},
	} {
		rec := serveTo(t, a.Handler(), http.MethodGet, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", tc.path, rec.Code)
		}
		if tc.want != "" && rec.Body.String() != tc.want {
			t.Errorf("GET %s: body %q, want %q", tc.path, rec.Body.String(), tc.want)
		}
	}
}

// TestNew_AppliesHostMiddlewareOutermostFirst pins the middleware order the
// spec documents: the first entry is the one a request reaches first, and the
// chain wraps outside the mux.
func TestNew_AppliesHostMiddlewareOutermostFirst(t *testing.T) {
	var host testHostConfig
	var seen []string
	wrap := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{&testModule{name: "probe", routePath: "/api/v1/probe", handler: staticHandler("probe")}}, nil
		}),
		WithHTTP(HTTPSpec{Middleware: []func(http.Handler) http.Handler{wrap("outer"), wrap("inner")}}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if rec := serveTo(t, a.Handler(), http.MethodGet, "/api/v1/probe"); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/probe through the chain: status %d, want 200", rec.Code)
	}
	want := []string{"outer", "inner"}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("middleware order = %v, want %v", seen, want)
	}
}

// TestNew_ServesTheSPAOutermostOfTheHostComposition pins the frontend's
// position: the SPA answers the paths it owns without running the host's
// middleware chain, while every path it does not own falls through to the
// composed stack unchanged.
func TestNew_ServesTheSPAOutermostOfTheHostComposition(t *testing.T) {
	var host testHostConfig
	spaDir := t.TempDir()
	index := "<!doctype html><title>spa</title>"
	if err := os.WriteFile(filepath.Join(spaDir, "index.html"), []byte(index), 0o600); err != nil {
		t.Fatalf("write the SPA fixture: %v", err)
	}
	var chainRuns int
	counter := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			chainRuns++
			next.ServeHTTP(w, r)
		})
	}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{&testModule{name: "probe", routePath: "/api/v1/probe", handler: staticHandler("probe")}}, nil
		}),
		WithHTTP(HTTPSpec{
			Middleware: []func(http.Handler) http.Handler{counter},
			SPA: &SPASpec{
				Dir: spaDir,
				// The server paths the composed handler owns, declared as
				// every host must: anything left undeclared is answered
				// from the frontend directory.
				Options: []spa.Option{spa.WithServerPrefix("/api"), spa.WithServerPath(obs.HealthzPath)},
			},
		}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	rec := serveTo(t, a.Handler(), http.MethodGet, "/")
	if rec.Code != http.StatusOK || rec.Body.String() != index {
		t.Fatalf("GET /: status %d body %q, want the SPA's index", rec.Code, rec.Body.String())
	}
	if chainRuns != 0 {
		t.Fatalf("the host middleware chain ran %d time(s) for a request the SPA answered, want 0", chainRuns)
	}

	if rec := serveTo(t, a.Handler(), http.MethodGet, "/api/v1/probe"); rec.Code != http.StatusOK || rec.Body.String() != "probe" {
		t.Fatalf("GET /api/v1/probe: status %d body %q, want the module's answer", rec.Code, rec.Body.String())
	}
	if chainRuns != 1 {
		t.Fatalf("the host middleware chain ran %d time(s) for the API request, want 1", chainRuns)
	}
}

// TestNew_PanicsOnAConflictingExtraRoute pins the wiring-error contract the
// mounting rule documents: two handlers under one path panic at assembly
// time, the loudest report available before anything listens.
func TestNew_PanicsOnAConflictingExtraRoute(t *testing.T) {
	var host testHostConfig
	defer func() {
		if recover() == nil {
			t.Fatal("New() with two handlers under one route did not panic")
		}
	}()

	_, _ = New(context.Background(), append(testBaseOptions(t, &host),
		WithHTTP(HTTPSpec{ExtraRoutes: []pkgcore.MountedRoute{
			{Path: "/api/v1/host", Handler: staticHandler("first")},
			{Path: "/api/v1/host", Handler: staticHandler("second")},
		}}),
	)...)
}

// TestNew_SkipsANilMiddlewareEntry pins the defensive half of the spec's
// middleware list: a nil entry is skipped rather than panicking the request
// path.
func TestNew_SkipsANilMiddlewareEntry(t *testing.T) {
	var host testHostConfig

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithHTTP(HTTPSpec{Middleware: []func(http.Handler) http.Handler{nil}}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if rec := serveTo(t, a.Handler(), http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: status %d, want 200", rec.Code)
	}
}

// TestNew_ComposeReceivesThePreparedMux pins the protected-face hook's
// contract: the callback receives the mux already carrying the platform
// liveness route and the host's ExtraRoutes, the engine does not mount the
// registry's routes when the callback composes the face, and the returned
// handler is what the host middleware entries wrap.
func TestNew_ComposeReceivesThePreparedMux(t *testing.T) {
	var host testHostConfig
	var composedMux *http.ServeMux
	wrapped := 0
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped++
			next.ServeHTTP(w, r)
		})
	}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
			return []pkgcore.Module{&testModule{name: "probe", routePath: "/api/v1/probe", handler: staticHandler("probe")}}, nil
		}),
		WithHTTP(HTTPSpec{
			ExtraRoutes: []pkgcore.MountedRoute{{Path: "/api/v1/host", Handler: staticHandler("host")}},
			Compose: func(mux *http.ServeMux) (http.Handler, error) {
				composedMux = mux
				return mux, nil
			},
			Middleware: []func(http.Handler) http.Handler{wrap},
		}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if composedMux == nil {
		t.Fatal("Compose was never called")
	}

	for _, tc := range []struct{ path, want string }{
		{"/healthz", ""},
		{"/api/v1/host", "host"},
	} {
		rec := serveTo(t, a.Handler(), http.MethodGet, tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s through the composed face: status %d, want 200", tc.path, rec.Code)
		}
		if tc.want != "" && rec.Body.String() != tc.want {
			t.Fatalf("GET %s: body %q, want %q", tc.path, rec.Body.String(), tc.want)
		}
	}
	if wrapped != 2 {
		t.Fatalf("the host middleware ran %d time(s), want 2; it must wrap the composed handler", wrapped)
	}

	// The engine must not mount the registry's routes itself when Compose
	// composes the face: this test's callback returns the bare mux without
	// mounting anything, so the module route answers the mux's 404.
	if rec := serveTo(t, a.Handler(), http.MethodGet, "/api/v1/probe"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/probe: status %d, want 404; a Compose host owns the face, so the engine must not mount the registry's routes", rec.Code)
	}
}

// TestNew_ComposeErrorsFailTheAssembly pins the failure half of the hook:
// a composition that cannot be built refuses the assembly instead of
// serving a face the host never composed.
func TestNew_ComposeErrorsFailTheAssembly(t *testing.T) {
	var host testHostConfig

	_, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithHTTP(HTTPSpec{Compose: func(*http.ServeMux) (http.Handler, error) {
			return nil, errors.New("unauthorized route table")
		}}),
	)...)
	if err == nil {
		t.Fatal("New() with a failing Compose returned no error")
	}

	_, err = New(context.Background(), append(testBaseOptions(t, &host),
		WithHTTP(HTTPSpec{Compose: func(*http.ServeMux) (http.Handler, error) { return nil, nil }}),
	)...)
	if err == nil {
		t.Fatal("New() with a nil-handler Compose returned no error")
	}
}

// TestNew_ReachesTheComposedChainForUnroutedPaths pins the fall-through the
// host chain sees: a path no route answers still travels the whole
// composition and the mux's own 404 is the answer, which is also what puts
// every rejected request in front of the observability middleware wrapping
// outside it.
func TestNew_ReachesTheComposedChainForUnroutedPaths(t *testing.T) {
	var host testHostConfig
	var chainPaths []string
	record := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			chainPaths = append(chainPaths, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithHTTP(HTTPSpec{Middleware: []func(http.Handler) http.Handler{record}}),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	rec := serveTo(t, a.Handler(), http.MethodGet, "/no/such/route")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /no/such/route: status %d, want the mux's 404", rec.Code)
	}
	if len(chainPaths) != 1 || chainPaths[0] != "/no/such/route" {
		t.Fatalf("host chain saw %v, want the unrouted request to pass through it", chainPaths)
	}
}
