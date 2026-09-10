package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/config"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// failingResolver fails every resolution, the tenant-less state the
// pre-auth allowlist exists for.
type failingResolver struct{}

func (failingResolver) Resolve(*http.Request) (pkgcore.TenantID, error) {
	return "", errors.New("test: no tenant resolvable")
}

// TestPreAuthAllowlist_ExemptsBothMethodsOnEveryPlatformPath pins the
// returned option set directly: each platform pre-auth path passes the
// tenancy chain under GET and HEAD alike, and the same path under another
// method stays refused.
func TestPreAuthAllowlist_ExemptsBothMethodsOnEveryPlatformPath(t *testing.T) {
	protected := tenancy.Middleware(failingResolver{}, PreAuthAllowlist()...)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{obs.HealthzPath, obs.MetricsPath, config.PathPublic, config.PathSystemFeatures} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNoContent {
				t.Errorf("%s %s: status %d, want %d; the path is on the pre-auth allowlist under both GET and HEAD", method, path, rec.Code, http.StatusNoContent)
			}
		}
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s: status %d, want %d; an unlisted method on an allowlisted path stays refused", path, rec.Code, http.StatusForbidden)
		}
	}
}

// staticRegistrar is a pkgcore.RouteRegistrar holding a fixed route set --
// enough of one to drive RegisterMountedRoutes.
type staticRegistrar struct{ routes []pkgcore.MountedRoute }

func (s staticRegistrar) Mount(path string, handler http.Handler) {
	_ = path
	_ = handler
}

func (s staticRegistrar) Routes() []pkgcore.MountedRoute { return s.routes }

// TestRegisterMountedRoutes_AcceptsARegistryWithMountedRoutes is a smoke
// test of the seeding call's entry surface: it accepts a registry carrying
// module routes without panicking. What the seed DOES -- reserving a
// route-label slot consumed at obs.Middleware construction -- is pinned by
// go/observability's own suite (TestMiddleware_RealRoutesSurviveGarbage_WhenSeeded),
// which is the package that also owns the mechanism.
func TestRegisterMountedRoutes_AcceptsARegistryWithMountedRoutes(t *testing.T) {
	reg := &pkgcore.Registry{Routes: staticRegistrar{routes: []pkgcore.MountedRoute{
		{Path: "/api/v1/notes", Handler: http.NewServeMux()},
	}}}
	RegisterMountedRoutes(reg)
}

// TestServeUntilShutdown_ServesThenDrainsCleanly drives the lifecycle end
// to end: the server answers a real request, then a cancelled context
// returns a nil error once the drain completes.
func TestServeUntilShutdown_ServesThenDrainsCleanly(t *testing.T) {
	// Reserve a port and release it, so the test knows where to dial: the
	// window between release and ServeUntilShutdown's own bind is the
	// usual test-time race, and a rebind failure would surface loudly
	// below as the server never answering.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if closeErr := l.Close(); closeErr != nil {
		t.Fatalf("release the port: %v", closeErr)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- ServeUntilShutdown(ctx, context.Background(), handler, addr, "app-test", "standalone")
	}()

	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err = http.Get("http://" + addr + "/") //nolint:gosec,noctx // test-local loopback URL a test built itself
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never answered at %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read the response body: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("GET /: status %d body %q, want 200 %q", resp.StatusCode, body, "ok")
	}

	cancel()
	select {
	case serveErr := <-errCh:
		if serveErr != nil {
			t.Fatalf("ServeUntilShutdown returned %v, want nil after a clean drain", serveErr)
		}
	case <-time.After(ShutdownTimeout + 5*time.Second):
		t.Fatal("ServeUntilShutdown did not return after its context was cancelled")
	}
}

// TestServeUntilShutdown_ServeFailureIsAttributed pins the failure path:
// an unlistenable address returns an error prefixed with the host's own
// app name, never a bare listener error.
func TestServeUntilShutdown_ServeFailureIsAttributed(t *testing.T) {
	err := ServeUntilShutdown(context.Background(), context.Background(), http.NewServeMux(), "not-an-addr", "app-test", "standalone")
	if err == nil {
		t.Fatal("ServeUntilShutdown returned nil for an unlistenable address")
	}
	if !strings.HasPrefix(err.Error(), "app-test: serve: ") {
		t.Fatalf("ServeUntilShutdown error %q does not carry the app-name attribution prefix", err)
	}
}
