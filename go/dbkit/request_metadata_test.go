package dbkit

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestRequestMetadata_RoundTrip pins the carrier's basic contract: a
// RequestMetadata set with WithRequestMetadata comes back unchanged from
// RequestMetadataFromContext on a context derived from the one it was set
// on.
func TestRequestMetadata_RoundTrip(t *testing.T) {
	md := RequestMetadata{IP: "203.0.113.7", UserAgent: "speed-client/1.0", TraceID: "trace-abc"}
	ctx := WithRequestMetadata(context.Background(), md)

	got, ok := RequestMetadataFromContext(ctx)
	if !ok {
		t.Fatal("RequestMetadataFromContext() ok = false, want true after WithRequestMetadata")
	}
	if got != md {
		t.Errorf("RequestMetadataFromContext() = %+v, want %+v", got, md)
	}
}

// TestRequestMetadata_Absent_IsReportedAbsent pins the other half of the
// presence contract: a context no WithRequestMetadata call ever touched
// reports absent, whatever other carriers it carries -- a background job's
// context in particular has an actor and a tenant but no request metadata,
// and the audit write paths must be able to tell "no request context" from
// "request context with empty fields".
func TestRequestMetadata_Absent_IsReportedAbsent(t *testing.T) {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-a"))
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})

	if _, ok := RequestMetadataFromContext(ctx); ok {
		t.Error("RequestMetadataFromContext() ok = true on a context carrying only tenant and actor, want false")
	}
}

// TestRequestMetadata_PresentWithEmptyFields_StillReportedPresent mirrors
// pkgcore.ActorFromContext's identical convention: WithRequestMetadata was
// called, so the carrier answers "set", even though every field is empty.
// This is what lets a populating layer deliberately stamp "this action ran
// in a request context that disclosed no metadata" if it ever needs to --
// and it pins that RequestMetadataFromContext answers presence, not
// population (see the function's own doc comment).
func TestRequestMetadata_PresentWithEmptyFields_StillReportedPresent(t *testing.T) {
	ctx := WithRequestMetadata(context.Background(), RequestMetadata{})

	got, ok := RequestMetadataFromContext(ctx)
	if !ok {
		t.Fatal("RequestMetadataFromContext() ok = false, want true (WithRequestMetadata was called)")
	}
	if got != (RequestMetadata{}) {
		t.Errorf("RequestMetadataFromContext() = %+v, want the zero RequestMetadata", got)
	}
}

// TestRequestMetadata_LayersIndependentlyOfOtherCarriers pins that the
// carrier is additive: setting it alongside a tenant and an actor neither
// clears nor is cleared by them, the same layering property pkgcore's
// WithActor/WithOnBehalfOf document for their own pair.
func TestRequestMetadata_LayersIndependentlyOfOtherCarriers(t *testing.T) {
	ctx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-a"))
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})
	ctx = WithRequestMetadata(ctx, RequestMetadata{IP: "203.0.113.7"})

	if _, ok := pkgcore.TenantFromContext(ctx); !ok {
		t.Error("tenant lost from the context after WithRequestMetadata")
	}
	if actor, ok := pkgcore.ActorFromContext(ctx); !ok || actor.ID != "user-1" {
		t.Errorf("actor = (%+v, %v), want user-1 still present after WithRequestMetadata", actor, ok)
	}
	if md, ok := RequestMetadataFromContext(ctx); !ok || md.IP != "203.0.113.7" {
		t.Errorf("RequestMetadata = (%+v, %v), want IP still present after the actor and tenant were set", md, ok)
	}
}
