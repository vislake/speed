package spa_test

// Runnable documentation for the spa public API. Every example here is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/vislake/speed/go/pkgcore/spa"
)

// ExampleNew builds a handler around a built frontend directory and shows the
// two answers that matter: a page request the frontend serves itself (the
// entry document, here by the client-route fallback), and a request for a
// declared server path that keeps passing through to the wrapped handler.
func ExampleNew() {
	dir, err := os.MkdirTemp("", "spa-example")
	if err != nil {
		fmt.Println("temp dir:", err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<main>app</main>"), 0o644); err != nil {
		fmt.Println("write index:", err)
		return
	}

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "api answered")
	})
	frontend := spa.New(dir, api,
		spa.WithServerPrefix("/api"),
		spa.WithServerPath("/healthz"),
	)

	rec := httptest.NewRecorder()
	frontend.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/deep/link", nil))
	fmt.Println("page:", rec.Code, strings.Contains(rec.Body.String(), "app"))

	rec = httptest.NewRecorder()
	frontend.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil))
	fmt.Println("api:", rec.Code, rec.Body.String())

	// Output:
	// page: 200 true
	// api: 200 api answered
}

// ExampleNew_assetPrefix shows the asset directory declaration: files under
// the declared prefix carry the immutable cache policy their content-addressed
// names make safe.
func ExampleNew_assetPrefix() {
	dir, err := os.MkdirTemp("", "spa-example-assets")
	if err != nil {
		fmt.Println("temp dir:", err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.MkdirAll(filepath.Join(dir, "bundles"), 0o755); err != nil {
		fmt.Println("mkdir bundles:", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "bundles", "app-1234.js"), []byte("console.log('app');"), 0o644); err != nil {
		fmt.Println("write bundle:", err)
		return
	}

	frontend := spa.New(dir, http.NotFoundHandler(), spa.WithAssetPrefix("bundles"))

	rec := httptest.NewRecorder()
	frontend.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bundles/app-1234.js", nil))
	fmt.Println(rec.Code, rec.Header().Get("Cache-Control"))

	// Output:
	// 200 public, max-age=31536000, immutable
}
