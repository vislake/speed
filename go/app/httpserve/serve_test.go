package httpserve_test

// This file pins the component's serve and shutdown behaviour, with the
// ordering probes the round's contract requires: the listener opens only
// after every component's Start (in the Serve round), the entry stops
// accepting new requests before any non-entry component is notified, and a
// declaration write outside the Init stage is refused rather than silently
// dropped.

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/app/httpserve"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
)

// freeAddr returns a loopback address that was free a moment ago, so a
// probe can dial the exact port the component will bind.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// dialFails reports whether a TCP connection to addr is refused right now.
func dialFails(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// probeComponent records what its callbacks observe: Start and Stop always
// note whether the listener is reachable at that moment, and Serve does too
// when the component declares it.
func probeComponent(name, addr string, record func(string), serve func(record func(string))) pkgcore.Component {
	c := pkgcore.Component{
		Name: name,
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Start: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			if dialFails(addr) {
				record(name + ":start:closed")
			} else {
				record(name + ":start:listening")
			}
			return nil
		},
		Stop: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			if dialFails(addr) {
				record(name + ":stop:closed")
			} else {
				record(name + ":stop:listening")
			}
			return nil
		},
	}
	if serve != nil {
		c.Serve = func(context.Context, *pkgcore.ComponentRegistry, any) error {
			serve(record)
			return nil
		}
	}
	return c
}

// TestServe_ListenerOrdering probes the listener's lifecycle over a real
// socket, from three sides in one boot: no listener during any Start (the
// Serve round opens it only after every Start), the listener reachable once
// the http component's Serve turn has run (the probe's Serve is ordered
// after it by the probe's route-face requirement), and the listener closed
// by the time an ordinary component -- no Serve callback, so the two-beat
// Stop reaches it in the second beat -- receives its own Stop.
func TestServe_ListenerOrdering(t *testing.T) {
	addr := freeAddr(t)
	var entries []string
	record := func(s string) { entries = append(entries, s) }

	// The Serve-stage probe requires the route face, so its Serve runs after
	// the http component's -- the moment the listener must be up.
	serveProbe := probeComponent("test.serve-probe", addr, record, func(record func(string)) {
		if dialFails(addr) {
			record("test.serve-probe:serve:closed")
			return
		}
		record("test.serve-probe:serve:listening")
	})
	serveProbe.Requires = []pkgcore.Requirement{{Token: (*pkgcore.RouteRegistrar)(nil)}}

	lateStop := probeComponent("test.late-stop", addr, record, nil)

	reg, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("addr", addr)).
		With("test.link-policy", nil).
		With("test.serve-probe", nil).
		With("test.late-stop", nil),
		policyProvider(chainlessPolicy()),
		serveProbe,
		lateStop,
	)
	if err != nil {
		t.Fatalf("Assemble(): %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := app.Shutdown(context.Background(), reg); shutdownErr != nil {
			t.Errorf("Shutdown(): %v", shutdownErr)
		}
	})

	// Every Start ran with no listener up (the Serve round opens it after
	// every Start), and the Serve-stage probe -- ordered after the http
	// component -- found it reachable.
	for _, want := range []string{"test.serve-probe:start:closed", "test.late-stop:start:closed", "test.serve-probe:serve:listening"} {
		if !contains(entries, want) {
			t.Fatalf("the probe records = %v, missing %q", entries, want)
		}
	}
	if contains(entries, "test.late-stop:start:listening") {
		t.Fatalf("the probe records = %v: a component's Start saw the listener already open; the Serve round must open it after every Start", entries)
	}

	// The listener is up and serving: the component mounts the platform
	// liveness routes on the protected mux, so /healthz answers over the
	// socket the component bound.
	resp, getErr := http.Get("http://" + addr + "/healthz")
	if getErr != nil {
		t.Fatalf("GET /healthz after Serve: %v", getErr)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("GET /healthz after Serve = (%d, %q, %v), want the liveness handler's 200 \"ok\"", resp.StatusCode, body, readErr)
	}

	// Shutdown drives the two beats: the entry stops accepting first, and
	// by the time the ordinary component's Stop is notified the listener is
	// closed -- no request can reach a service behind a stopped entry.
	if err := app.Shutdown(context.Background(), reg); err != nil {
		t.Fatalf("Shutdown(): %v", err)
	}
	if !contains(entries, "test.late-stop:stop:closed") {
		t.Fatalf("the probe records = %v, want the second beat's Stop to observe a closed listener", entries)
	}
	if !dialFails(addr) {
		t.Fatal("the listener still accepts connections after Shutdown")
	}
}

func contains(entries []string, want string) bool {
	for _, entry := range entries {
		if entry == want {
			return true
		}
	}
	return false
}

// TestFace_GateRefusesWritesOutsideInit pins the write gate: after the
// Serve stage the assembled handler exists, so a late mount must be a loud
// wiring error -- Mount panics with ErrStageViolation and Add refuses with
// it -- instead of silently writing into a route table nothing reads.
func TestFace_GateRefusesWritesOutsideInit(t *testing.T) {
	reg, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("listen", false)).
		With("test.link-policy", nil),
		policyProvider(chainlessPolicy()),
	)
	if err != nil {
		t.Fatalf("Assemble(): %v", err)
	}
	face, err := pkgcore.Get[*httpserve.Face](reg)
	if err != nil {
		t.Fatalf("read the face: %v", err)
	}

	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("Mount after Serve did not panic")
			}
			errValue, ok := recovered.(error)
			if !ok || !errors.Is(errValue, pkgcore.ErrStageViolation) {
				t.Fatalf("Mount panicked with %v, want an error wrapping ErrStageViolation", recovered)
			}
			if !strings.Contains(errValue.Error(), "serve") {
				t.Fatalf("the Mount panic = %v, want it to name the current stage", errValue)
			}
		}()
		face.Mount("/late", http.NotFoundHandler())
	}()

	if err := face.Add(func(next http.Handler) http.Handler { return next }); !errors.Is(err, pkgcore.ErrStageViolation) {
		t.Fatalf("Add() after Serve = %v, want ErrStageViolation", err)
	}
	if got := face.Routes(); len(got) != 0 {
		t.Fatalf("Routes() after the refused write = %v, want the refusal to have registered nothing", got)
	}

	if err := app.Shutdown(context.Background(), reg); err != nil {
		t.Fatalf("Shutdown(): %v", err)
	}
	// The refusal is stage-wide, not just mid-assembly: a closed assembly
	// refuses the write too.
	if err := face.Add(func(next http.Handler) http.Handler { return next }); !errors.Is(err, pkgcore.ErrStageViolation) {
		t.Fatalf("Add() after Close = %v, want ErrStageViolation", err)
	}
}

// TestFace_RegistrarsAcceptInitWrites pins the accepting half of the gate
// and the two faces' reads over a real assembly: a probe component's Init
// turn mounts a route and declares middleware through the GetOptional idiom
// every module uses, and both land on the one product -- the route served
// by the composed handler, the middleware applied around the chain.
func TestFace_RegistrarsAcceptInitWrites(t *testing.T) {
	addr := freeAddr(t)
	mounted := make(chan string, 1)
	observed := make(chan string, 1)

	probe := pkgcore.Component{
		Name: "test.mounting-module",
		Requires: []pkgcore.Requirement{
			{Token: (*pkgcore.RouteRegistrar)(nil), Optional: true},
			{Token: (*pkgcore.MiddlewareRegistrar)(nil), Optional: true},
		},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			registrar, ok, err := pkgcore.GetOptional[pkgcore.RouteRegistrar](reg)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			registrar.Mount("/api/v1/mounted", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("mounted"))
			}))
			mounted <- fmt.Sprintf("routes=%d", len(registrar.Routes()))

			mw, ok, err := pkgcore.GetOptional[pkgcore.MiddlewareRegistrar](reg)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			return mw.Add(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					select {
					case observed <- r.URL.Path:
					default:
					}
					next.ServeHTTP(w, r)
				})
			})
		},
	}

	reg, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("addr", addr)).
		With("test.link-policy", nil).
		With("test.mounting-module", nil),
		policyProvider(chainlessPolicy()),
		probe,
	)
	if err != nil {
		t.Fatalf("Assemble(): %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := app.Shutdown(context.Background(), reg); shutdownErr != nil {
			t.Errorf("Shutdown(): %v", shutdownErr)
		}
	})
	if got := <-mounted; got != "routes=1" {
		t.Fatalf("the Init-turn route write recorded %q, want one route on the face", got)
	}

	// The mounted route is served by the composed handler -- the chainless
	// policy serves the protected mux directly -- and the declared
	// middleware wraps the request.
	resp, getErr := http.Get("http://" + addr + "/api/v1/mounted")
	if getErr != nil {
		t.Fatalf("GET the mounted route: %v", getErr)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "mounted" {
		t.Fatalf("GET /api/v1/mounted = (%d, %q), want the mounted handler's 200", resp.StatusCode, body)
	}
	select {
	case path := <-observed:
		if path != "/api/v1/mounted" {
			t.Fatalf("the declared middleware observed %q, want the mounted path", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the declared middleware never observed the request")
	}
}

// TestFace_AddRefusesNilMiddleware pins the declaration face's own
// refusal: a nil middleware could never wrap a handler, so the Init-turn
// declaration is refused with ErrNilMiddleware and the assembly fails
// loudly instead of registering a layer that would panic at serve time.
func TestFace_AddRefusesNilMiddleware(t *testing.T) {
	nilMiddlewares := pkgcore.Component{
		Name: "test.nil-middleware",
		Requires: []pkgcore.Requirement{
			{Token: (*pkgcore.MiddlewareRegistrar)(nil), Optional: true},
		},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			mw, ok, err := pkgcore.GetOptional[pkgcore.MiddlewareRegistrar](reg)
			if err != nil || !ok {
				return err
			}
			return mw.Add(nil)
		},
	}

	_, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("listen", false)).
		With("test.link-policy", nil).
		With("test.nil-middleware", nil),
		policyProvider(chainlessPolicy()),
		nilMiddlewares,
	)
	if !errors.Is(err, pkgcore.ErrNilMiddleware) {
		t.Fatalf("Assemble() with a nil middleware declared = %v, want ErrNilMiddleware", err)
	}
}

// noKeysKeySource is an authn.KeySource with no verification keys: every
// bearer fails verification, which is all the guarded-path tests need from
// the verifier (they assert the chain's routing, not a verified Principal).
type noKeysKeySource struct{}

func (noKeysKeySource) EnsurePurpose(context.Context, string, string, time.Duration) error {
	return nil
}

func (noKeysKeySource) ActiveSigner(context.Context, string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	return "", "", nil, errors.New("noKeysKeySource: no signer")
}

func (noKeysKeySource) VerificationKeys(context.Context, string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	return nil, nil
}

// newTestVerifier returns a real verifier over the empty key source.
func newTestVerifier(t *testing.T) *authn.Verifier {
	t.Helper()
	v, err := authn.NewVerifier(noKeysKeySource{})
	if err != nil {
		t.Fatalf("authn.NewVerifier: %v", err)
	}
	return v
}

// TestServe_GuardedPolicy pins the guarded branch of the Serve-stage
// assembly end to end: with a verifier in the policy, the accumulated
// declarations route through chain.Standard -- authn's subtree is split
// out by structure (served without a Principal) and the protected route
// meets the tenancy chain before any handler.
func TestServe_GuardedPolicy(t *testing.T) {
	authnHits := 0
	protectedHits := 0
	authnHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		authnHits++
		w.WriteHeader(http.StatusOK)
	})
	protectedHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		protectedHits++
		w.WriteHeader(http.StatusOK)
	})

	reg, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("listen", false)).
		With("test.link-policy", nil).
		With("test.module", nil),
		policyProvider(&httpserve.LinkPolicy{Verifier: newTestVerifier(t)}),
		routesModuleComponent("test.module",
			pkgcore.MountedRoute{Path: app.AuthnAPIPath, Handler: authnHandler},
			pkgcore.MountedRoute{Path: "/api/v1/notes", Handler: protectedHandler},
		),
	)
	if err != nil {
		t.Fatalf("Assemble(): %v", err)
	}
	t.Cleanup(func() {
		if shutdownErr := app.Shutdown(context.Background(), reg); shutdownErr != nil {
			t.Errorf("Shutdown(): %v", shutdownErr)
		}
	})
	face, err := pkgcore.Get[*httpserve.Face](reg)
	if err != nil {
		t.Fatalf("read the face: %v", err)
	}
	served := face.Handler()

	// authn's subtree is dispatched ahead of the tenancy chain entirely,
	// so its request reaches the handler with no Principal at all.
	rec := httptest.NewRecorder()
	served.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, app.AuthnAPIPath+"/public", nil))
	if rec.Code != http.StatusOK || authnHits != 1 {
		t.Fatalf("GET %s/public = (%d, hits=%d), want the authn branch to serve it", app.AuthnAPIPath, rec.Code, authnHits)
	}

	// The protected route meets tenancy before the handler: an anonymous
	// request carries no Principal, and the handler must never run.
	rec = httptest.NewRecorder()
	served.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil))
	if rec.Code == http.StatusOK || protectedHits != 0 {
		t.Fatalf("anonymous GET /api/v1/notes = (%d, hits=%d), want the tenancy chain to refuse before the handler", rec.Code, protectedHits)
	}
}

// TestServe_GuardedPolicyWithoutAnAuthnRoute pins the derivation failure:
// a guarded policy whose mounts carry no authn route fails the Serve stage
// naming the failed derivation, instead of serving a chain with no
// verified-caller branch.
func TestServe_GuardedPolicyWithoutAnAuthnRoute(t *testing.T) {
	_, err := assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("listen", false)).
		With("test.link-policy", nil).
		With("test.module", nil),
		policyProvider(&httpserve.LinkPolicy{Verifier: newTestVerifier(t)}),
		routesModuleComponent("test.module",
			pkgcore.MountedRoute{Path: "/api/v1/notes", Handler: http.NotFoundHandler()},
		),
	)
	if err == nil {
		t.Fatal("Assemble() with a guarded policy and no authn route succeeded, want the derivation refusal")
	}
	if !strings.Contains(err.Error(), "authn") {
		t.Fatalf("Assemble() error = %v, want it to name the missing authn subtree", err)
	}
}

// TestServe_FailsWhenTheAddressIsTaken pins the listen failure: an address
// already bound by another process fails the Serve stage naming the
// configured address, rather than starting half a process.
func TestServe_FailsWhenTheAddressIsTaken(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	_, err = assemble(t, pkgcore.ComponentConfig{}.
		With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("addr", occupied.Addr().String())).
		With("test.link-policy", nil),
		policyProvider(chainlessPolicy()),
	)
	if err == nil {
		t.Fatal("Assemble() with an occupied address succeeded, want the listen refusal")
	}
	if !strings.Contains(err.Error(), "listen on "+occupied.Addr().String()) {
		t.Fatalf("Assemble() error = %v, want it to name the occupied address", err)
	}
}
