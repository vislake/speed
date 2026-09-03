package pkgcore

import (
	"context"
	"testing"
)

func TestWithActor_ActorFromContext_RoundTrips(t *testing.T) {
	want := Actor{Type: ActorTypeUser, ID: "user-1", DisplayName: "Ada"}
	ctx := WithActor(context.Background(), want)

	got, ok := ActorFromContext(ctx)
	if !ok {
		t.Fatalf("ActorFromContext() ok = false, want true")
	}
	if got != want {
		t.Fatalf("ActorFromContext() = %+v, want %+v", got, want)
	}
}

func TestActorFromContext_Absent_ReturnsZeroValueAndFalse(t *testing.T) {
	got, ok := ActorFromContext(context.Background())
	if ok {
		t.Fatalf("ActorFromContext() ok = true, want false on a context with no actor")
	}
	if got != (Actor{}) {
		t.Fatalf("ActorFromContext() = %+v, want the zero Actor", got)
	}
}

// TestActorFromContext_ZeroValueActor_StillReportedPresent proves the
// documented convention that presence tracks whether WithActor was called
// at all, not whether the Actor "looks populated" -- the same convention
// SystemReasonFromContext already uses.
func TestActorFromContext_ZeroValueActor_StillReportedPresent(t *testing.T) {
	ctx := WithActor(context.Background(), Actor{})

	got, ok := ActorFromContext(ctx)
	if !ok {
		t.Fatalf("ActorFromContext() ok = false, want true for an explicitly-set zero Actor")
	}
	if got != (Actor{}) {
		t.Fatalf("ActorFromContext() = %+v, want the zero Actor", got)
	}
}

func TestWithOnBehalfOf_OnBehalfOfFromContext_RoundTrips(t *testing.T) {
	want := Actor{Type: ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"}
	ctx := WithOnBehalfOf(context.Background(), want)

	got, ok := OnBehalfOfFromContext(ctx)
	if !ok {
		t.Fatalf("OnBehalfOfFromContext() ok = false, want true")
	}
	if got != want {
		t.Fatalf("OnBehalfOfFromContext() = %+v, want %+v", got, want)
	}
}

func TestOnBehalfOfFromContext_Absent_ReturnsZeroValueAndFalse(t *testing.T) {
	got, ok := OnBehalfOfFromContext(context.Background())
	if ok {
		t.Fatalf("OnBehalfOfFromContext() ok = true, want false on a context with no on-behalf-of actor")
	}
	if got != (Actor{}) {
		t.Fatalf("OnBehalfOfFromContext() = %+v, want the zero Actor", got)
	}
}

// TestWithActor_WithOnBehalfOf_LayerIndependently is the core impersonation
// guarantee (docs/internal/10-compliance-and-audit.md): setting Actor must
// never clear OnBehalfOf, and vice versa, in either order, so an
// impersonated request can carry both identities at once.
func TestWithActor_WithOnBehalfOf_LayerIndependently(t *testing.T) {
	impersonated := Actor{Type: ActorTypeUser, ID: "user-1", DisplayName: "Ada"}
	admin := Actor{Type: ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"}

	t.Run("actor then on-behalf-of", func(t *testing.T) {
		ctx := WithActor(context.Background(), impersonated)
		ctx = WithOnBehalfOf(ctx, admin)

		gotActor, ok := ActorFromContext(ctx)
		if !ok || gotActor != impersonated {
			t.Fatalf("ActorFromContext() = %+v, %v, want %+v, true", gotActor, ok, impersonated)
		}
		gotOnBehalfOf, ok := OnBehalfOfFromContext(ctx)
		if !ok || gotOnBehalfOf != admin {
			t.Fatalf("OnBehalfOfFromContext() = %+v, %v, want %+v, true", gotOnBehalfOf, ok, admin)
		}
	})

	t.Run("on-behalf-of then actor", func(t *testing.T) {
		ctx := WithOnBehalfOf(context.Background(), admin)
		ctx = WithActor(ctx, impersonated)

		gotActor, ok := ActorFromContext(ctx)
		if !ok || gotActor != impersonated {
			t.Fatalf("ActorFromContext() = %+v, %v, want %+v, true", gotActor, ok, impersonated)
		}
		gotOnBehalfOf, ok := OnBehalfOfFromContext(ctx)
		if !ok || gotOnBehalfOf != admin {
			t.Fatalf("OnBehalfOfFromContext() = %+v, %v, want %+v, true", gotOnBehalfOf, ok, admin)
		}
	})
}

// TestWithActor_Overwrite_ReplacesPreviousActor proves a second WithActor
// call replaces the first rather than stacking, matching WithTenant's own
// (undocumented-but-relied-on) last-write-wins behavior for context values.
func TestWithActor_Overwrite_ReplacesPreviousActor(t *testing.T) {
	first := Actor{Type: ActorTypeUser, ID: "user-1"}
	second := Actor{Type: ActorTypeAPIKey, ID: "key-1"}

	ctx := WithActor(context.Background(), first)
	ctx = WithActor(ctx, second)

	got, ok := ActorFromContext(ctx)
	if !ok || got != second {
		t.Fatalf("ActorFromContext() = %+v, %v, want %+v, true", got, ok, second)
	}
}
