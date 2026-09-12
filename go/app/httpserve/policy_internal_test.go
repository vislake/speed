package httpserve

// policy_internal_test.go reaches the component's unexported half: the
// link policy's construction checks and option translation, the
// descriptor's construction refusals, the lifecycle callbacks' type
// guards, and the face's reads. It is the white-box companion to the
// external suite, which pins the same contract end to end through real
// assemblies.

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// stubAuthorizer is a non-nil rbac.Authorizer for the policy checks, which
// only ask whether the field is set.
type stubAuthorizer struct{ rbac.Authorizer }

// stubTenantStatusResolver is a non-nil tenancy.TenantStatusResolver for
// the same purpose.
type stubTenantStatusResolver struct{ tenancy.TenantStatusResolver }

// fullPolicy returns a policy with every slot set, the shape guarded() must
// report completely.
func fullPolicy() *LinkPolicy {
	return &LinkPolicy{
		Verifier:             &authn.Verifier{},
		Authorizer:           stubAuthorizer{},
		RouteRules:           []rbac.RouteRule{{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Public: true}}},
		AdminPrefix:          "/api/v1/admin",
		Impersonation:        func(next http.Handler) http.Handler { return next },
		TenantStatusResolver: stubTenantStatusResolver{},
		ExtraAllowlist:       []tenancy.MiddlewareOption{tenancy.WithAllowlist(http.MethodGet, "/api/v1/notes")},
	}
}

// TestLinkPolicy_GuardedNamesEverySetField pins the construction check's
// field census: every guarded-half slot reports itself by name, in the
// declaration order the refusal message reads in.
func TestLinkPolicy_GuardedNamesEverySetField(t *testing.T) {
	want := []string{"Verifier", "Authorizer", "RouteRules", "AdminPrefix", "Impersonation", "TenantStatusResolver", "ExtraAllowlist"}
	if got := fullPolicy().guarded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("guarded() = %v, want %v", got, want)
	}
	if got := (&LinkPolicy{Chainless: true}).guarded(); len(got) != 0 {
		t.Fatalf("guarded() on a bare chainless policy = %v, want none", got)
	}
}

// TestLinkPolicy_ValidateRefusesContradictions pins both refusal classes and
// both accepting forms directly: a chainless policy may carry none of the
// guarded half, and a guarded policy must carry a verifier -- the chainless
// form is a positive declaration, never a missing-verifier fallback.
func TestLinkPolicy_ValidateRefusesContradictions(t *testing.T) {
	contradictions := []struct {
		name   string
		field  string
		policy *LinkPolicy
	}{
		{"verifier", "Verifier", &LinkPolicy{Chainless: true, Verifier: &authn.Verifier{}}},
		{"authorizer", "Authorizer", &LinkPolicy{Chainless: true, Authorizer: stubAuthorizer{}}},
		{"route rules", "RouteRules", &LinkPolicy{Chainless: true, RouteRules: []rbac.RouteRule{{Path: "/x"}}}},
		{"admin prefix", "AdminPrefix", &LinkPolicy{Chainless: true, AdminPrefix: "/api/v1/admin"}},
		{"impersonation", "Impersonation", &LinkPolicy{Chainless: true, Impersonation: func(next http.Handler) http.Handler { return next }}},
		{"tenant status resolver", "TenantStatusResolver", &LinkPolicy{Chainless: true, TenantStatusResolver: stubTenantStatusResolver{}}},
		{"extra allowlist", "ExtraAllowlist", &LinkPolicy{Chainless: true, ExtraAllowlist: []tenancy.MiddlewareOption{tenancy.WithAllowlist(http.MethodGet, "/x")}}},
	}
	for _, tc := range contradictions {
		t.Run("chainless carrying "+tc.name, func(t *testing.T) {
			err := tc.policy.validate()
			if err == nil {
				t.Fatal("validate() accepted a chainless policy carrying a guarded field")
			}
			if !strings.Contains(err.Error(), "Chainless") || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("validate() error = %v, want it to name Chainless and the %s field", err, tc.field)
			}
		})
	}
	if err := (&LinkPolicy{Chainless: true}).validate(); err != nil {
		t.Fatalf("validate() on a bare chainless policy = %v, want nil", err)
	}
	if err := fullPolicy().validate(); err != nil {
		t.Fatalf("validate() on a fully guarded policy = %v, want nil", err)
	}
	err := (&LinkPolicy{}).validate()
	if err == nil || !strings.Contains(err.Error(), "Verifier") || !strings.Contains(err.Error(), "Chainless") {
		t.Fatalf("validate() on a guarded policy without a verifier = %v, want it to name the missing Verifier and the Chainless alternative", err)
	}
}

// TestLinkPolicy_ChainOptionsTranslatesEveryHalf pins the option
// translation: each declared half contributes exactly one option, and an
// empty policy contributes none.
func TestLinkPolicy_ChainOptionsTranslatesEveryHalf(t *testing.T) {
	if got := (&LinkPolicy{}).chainOptions(); len(got) != 0 {
		t.Fatalf("chainOptions() on an empty policy = %d option(s), want none", len(got))
	}
	if got := fullPolicy().chainOptions(); len(got) != 5 {
		t.Fatalf("chainOptions() on the full policy = %d option(s), want one per declared half (5)", len(got))
	}
}

// TestFace_ReadsBeforeAndAfterCompose pins the product's reads around a
// compose-only Serve turn: no listen address and no listener before the
// turn, the configured address after it (nothing bound), and an empty
// declaration set throughout.
func TestFace_ReadsBeforeAndAfterCompose(t *testing.T) {
	face := &Face{
		reg:    pkgcore.NewComponentRegistry(),
		cfg:    httpConfig{Addr: "127.0.0.1:9090", Listen: false},
		policy: &LinkPolicy{Chainless: true},
	}
	if got := face.Addr(); got != "" {
		t.Fatalf("Addr() = %q before the Serve stage, want the empty address", got)
	}
	if got := face.MountedRoutes(); len(got) != 0 {
		t.Fatalf("MountedRoutes() = %v, want nothing before any mount", got)
	}
	if err := face.serve(context.Background()); err != nil {
		t.Fatalf("serve() in compose-only mode = %v", err)
	}
	if got := face.Addr(); got != "127.0.0.1:9090" {
		t.Fatalf("Addr() after a compose-only Serve = %q, want the configured address", got)
	}
	if face.Listening() {
		t.Fatal("Listening() = true for a compose-only face, want false")
	}
}

// TestNewFace_Refusals pins the construction contract: the host link policy
// must be resolvable, the configuration block must decode strictly, and the
// resolved defaults come through on the happy path.
func TestNewFace_Refusals(t *testing.T) {
	ctx := context.Background()

	t.Run("missing policy", func(t *testing.T) {
		_, err := newFace(ctx, pkgcore.NewComponentRegistry(), pkgcore.ComponentConfig{})
		if err == nil || !strings.Contains(err.Error(), "link policy") {
			t.Fatalf("newFace() without a policy = %v, want the missing-policy refusal", err)
		}
	})

	t.Run("unknown configuration key", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(&LinkPolicy{Chainless: true})
		_, err := newFace(ctx, reg, pkgcore.ComponentConfig{}.With("unknown_key", "x"))
		if err == nil || !strings.Contains(err.Error(), "unknown_key") {
			t.Fatalf("newFace() with an unknown key = %v, want the strict-decode refusal naming it", err)
		}
	})

	t.Run("empty addr", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(&LinkPolicy{Chainless: true})
		_, err := newFace(ctx, reg, pkgcore.ComponentConfig{}.With("addr", ""))
		if err == nil || !strings.Contains(err.Error(), "addr") {
			t.Fatalf("newFace() with an emptied addr = %v, want the empty-address refusal", err)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		policy := &LinkPolicy{Chainless: true}
		reg.Put(policy)
		instance, err := newFace(ctx, reg, pkgcore.ComponentConfig{})
		if err != nil {
			t.Fatalf("newFace() = %v, want the constructed face", err)
		}
		face, ok := instance.(*Face)
		if !ok {
			t.Fatalf("newFace() returned %T, want *Face", instance)
		}
		if face.policy != policy {
			t.Fatal("the constructed face does not carry the registry's policy value")
		}
		if face.cfg.Addr != ":8080" || !face.cfg.Listen {
			t.Fatalf("the constructed face's configuration = %+v, want the schema defaults (addr :8080, listen true)", face.cfg)
		}
	})
}

// TestLifecycleCallbacks_RefuseAForeignInstance pins the three callbacks'
// type guards: an instance that is not this component's own face is refused,
// never served or stopped blind.
func TestLifecycleCallbacks_RefuseAForeignInstance(t *testing.T) {
	ctx := context.Background()
	if err := serveFace(ctx, nil, struct{}{}); err == nil || !strings.Contains(err.Error(), "the component holds an instance") {
		t.Fatalf("serveFace() with a foreign instance = %v, want the type refusal", err)
	}
	if err := stopFace(ctx, nil, struct{}{}); err == nil || !strings.Contains(err.Error(), "the component holds an instance") {
		t.Fatalf("stopFace() with a foreign instance = %v, want the type refusal", err)
	}
	if err := closeFace(ctx, nil, struct{}{}); err == nil || !strings.Contains(err.Error(), "the component holds an instance") {
		t.Fatalf("closeFace() with a foreign instance = %v, want the type refusal", err)
	}
}

// TestFace_CloseCoversBothDrainShapes pins close's two contract shapes
// deterministically, so neither depends on where the first beat's drain
// goroutine happens to be: a close that arrives before any Stop drove the
// drain stops and drains synchronously itself, and a close arriving after
// the drain finished just reports its outcome.
func TestFace_CloseCoversBothDrainShapes(t *testing.T) {
	ctx := context.Background()

	t.Run("close before stop drains synchronously", func(t *testing.T) {
		face := &Face{
			reg:    pkgcore.NewComponentRegistry(),
			cfg:    httpConfig{Addr: "127.0.0.1:0", Listen: true, ShutdownTimeout: time.Second},
			policy: &LinkPolicy{Chainless: true},
		}
		if err := face.serve(ctx); err != nil {
			t.Fatalf("serve() = %v", err)
		}
		if err := face.close(ctx); err != nil {
			t.Fatalf("close() without a preceding stop = %v, want the synchronous drain to succeed", err)
		}
		if conn, dialErr := net.DialTimeout("tcp", face.Addr(), 500*time.Millisecond); dialErr == nil {
			_ = conn.Close()
			t.Fatal("the listener still accepts connections after the synchronous close")
		}
	})

	t.Run("close after the stopped drain reports it", func(t *testing.T) {
		face := &Face{
			reg:    pkgcore.NewComponentRegistry(),
			cfg:    httpConfig{Addr: "127.0.0.1:0", Listen: true, ShutdownTimeout: time.Second},
			policy: &LinkPolicy{Chainless: true},
		}
		if err := face.serve(ctx); err != nil {
			t.Fatalf("serve() = %v", err)
		}
		face.stop(ctx)
		face.mu.Lock()
		drained := face.drained
		face.mu.Unlock()
		<-drained
		if err := face.close(ctx); err != nil {
			t.Fatalf("close() after the drain finished = %v, want the drained outcome", err)
		}
	})
}
