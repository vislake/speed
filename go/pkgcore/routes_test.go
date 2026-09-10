package pkgcore

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMountRoutes_ServesTheExactPathWithoutARedirect(t *testing.T) {
	mux := http.NewServeMux()
	var gotMethod string
	MountRoutes(mux, MountedRoute{Path: "/api/v1/notes", Handler: http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			w.WriteHeader(http.StatusNoContent)
		},
	)})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/notes", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /api/v1/notes answered %d, want %d served directly: a 301/308 here would mean only the subtree pattern is registered, silently breaking every POST",
			rec.Code, http.StatusNoContent)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("the handler saw method %q, want POST", gotMethod)
	}

	// Nested requests reach the same handler through the subtree pattern.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/notes/42", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("GET /api/v1/notes/42 answered %d, want the same handler", rec.Code)
	}
}

func TestMountRoutes_TrailingSlashPathIsRegisteredOnce(t *testing.T) {
	mux := http.NewServeMux()
	// A Path already ending in "/" IS the subtree pattern; mounting it must
	// not register a second, unmatchable path+"/" pattern.
	MountRoutes(mux, MountedRoute{Path: "/api/v1/tree/", Handler: http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "tree") },
	)})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tree/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/tree/ answered %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tree/branch", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/tree/branch answered %d, want 200 from the same subtree handler", rec.Code)
	}
}

func TestMountRoutes_MountsEveryRouteInOneCall(t *testing.T) {
	mux := http.NewServeMux()
	newMarker := func(mark string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, mark) })
	}
	MountRoutes(mux,
		MountedRoute{Path: "/admin", Handler: newMarker("admin")},
		MountedRoute{Path: "/api/v1/authn", Handler: newMarker("authn")},
	)

	for path, want := range map[string]string{"/admin": "admin", "/admin/grants": "admin", "/api/v1/authn": "authn", "/api/v1/authn/login": "authn"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Fatalf("GET %s = (%d, %q), want (200, %q)", path, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestMountRoutes_EmptyPathPanics(t *testing.T) {
	// ServeMux refuses an empty pattern outright (it would otherwise mean
	// "the whole tree"); mounting is not the place to soften a wiring error
	// into a silently over-broad route.
	defer func() {
		if recover() == nil {
			t.Fatal("MountRoutes with an empty Path did not panic")
		}
	}()
	MountRoutes(http.NewServeMux(), MountedRoute{Path: "", Handler: http.NotFoundHandler()})
}
