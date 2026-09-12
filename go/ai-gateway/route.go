package aigateway

// ModelRoute is what a logical model key resolves to: a named provider
// implementation (a component name in the "chat" directory, for example
// "chat.openai-compatible") plus the concrete vendor model id that provider
// should call (for example "gpt-4o-mini").
type ModelRoute struct {
	// Provider is the provider's component name to resolve.
	Provider string
	// VendorModel is the vendor-specific model id, opaque to this package,
	// passed through to the provider verbatim.
	VendorModel string
}

// WithModelRoute declares that logicalKey (a caller-facing key such as
// "chat:default" or "chat:fast") resolves to provider/vendorModel. It is a
// construction-time GatewayOption, not a dynamic go/config item: model
// routing is an infrastructure-composition decision the host assembler
// makes once, the same tier of decision WithEventBus/WithMailer and friends
// are in pkgcore -- not a value a tenant or an operator tunes at runtime
// through the admin console, and there is no runtime-configurable routing
// layer. Keeping it a Go-level option means the whole pipeline is provably
// wired at process start, with no possibility of a route silently changing
// mid-request.
//
// Calling WithModelRoute twice for the same logicalKey replaces the
// earlier route -- there is no duplicate-registration error, because
// GatewayOptions apply in the order given and "last wins" is the ordinary
// functional-option convention.
//
// Business code that calls Gateway.Chat/ChatStream with an unrouted
// logicalKey gets ErrUnroutedModel -- never a silent fallback to some
// default provider or vendor model.
func WithModelRoute(logicalKey, provider, vendorModel string) GatewayOption {
	return func(g *Gateway) {
		g.routes[logicalKey] = ModelRoute{Provider: provider, VendorModel: vendorModel}
	}
}
