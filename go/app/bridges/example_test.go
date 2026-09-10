package bridges_test

// Runnable documentation for the bridges package's public API, mirroring
// go/tenancy/example_test.go's convention: every example here is compiled
// and executed by `go test`, so a change to the package's public API that
// breaks the documented usage fails the build instead of only rotting in
// prose.

import (
	"context"
	"fmt"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/app/bridges"
	"github.com/vislake/speed/go/billing"
)

// exampleEntitlements is a billing.Entitlements stand-in answering one
// fixed plan-permits decision; a real host hands the bridge its billing
// module's *billing.EntitlementsService instead.
type exampleEntitlements struct{}

func (exampleEntitlements) Check(context.Context, string, int64) (billing.Decision, error) {
	return billing.Decision{Allowed: true, Reason: billing.DecisionReasonOK}, nil
}

// ExampleEntitlements shows the AI-model gating bridge: one call turns
// billing's entitlement check into the seam ai-gateway gates every
// Chat/ChatStream call on -- key "model:<logical-key>", requested 1,
// answered before any provider is reached. A host wires it with
//
//	aigateway.WithEntitlements(bridges.Entitlements(billingModule.Entitlements()))
//
// and a host that also pre-flights the gate outside the gateway (before
// opening a credit reservation, say) hands that pre-flight the same
// returned func, so the two can never disagree about one state.
func ExampleEntitlements() {
	gate := aigateway.Entitlements(bridges.Entitlements(exampleEntitlements{}))

	decision, err := gate.Check(context.Background(), "model:gpt-4o-mini", 1)
	if err != nil {
		panic(err)
	}
	fmt.Printf("allowed=%t reason=%s\n", decision.Allowed, decision.Reason)

	// Output:
	// allowed=true reason=ok
}
