package aigateway

import "testing"

func TestWithModelRoute_RegistersTheRoute(t *testing.T) {
	g := NewGateway(nil, WithModelRoute("chat:default", ProviderOpenAICompatible, "gpt-4o-mini"))
	got, ok := g.routes["chat:default"]
	if !ok {
		t.Fatal("chat:default was not registered")
	}
	want := ModelRoute{Provider: ProviderOpenAICompatible, VendorModel: "gpt-4o-mini"}
	if got != want {
		t.Fatalf("routes[\"chat:default\"] = %+v, want %+v", got, want)
	}
}

func TestWithModelRoute_LastCallForOneKeyWins(t *testing.T) {
	g := NewGateway(nil,
		WithModelRoute("chat:default", ProviderOpenAICompatible, "gpt-4o-mini"),
		WithModelRoute("chat:default", ProviderOpenAICompatible, "gpt-4.1"),
	)
	got := g.routes["chat:default"]
	if got.VendorModel != "gpt-4.1" {
		t.Fatalf("routes[\"chat:default\"].VendorModel = %q, want the last call to win (%q)", got.VendorModel, "gpt-4.1")
	}
}

func TestWithModelRoute_UnrelatedKeysCoexist(t *testing.T) {
	g := NewGateway(nil,
		WithModelRoute("chat:default", ProviderOpenAICompatible, "gpt-4o-mini"),
		WithModelRoute("chat:fast", ProviderOpenAICompatible, "gpt-4o-mini-fast"),
	)
	if len(g.routes) != 2 {
		t.Fatalf("got %d routes, want 2: %+v", len(g.routes), g.routes)
	}
}
