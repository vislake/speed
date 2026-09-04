package aigateway

// ModelRoute is what a logical model key resolves to: a named ChatProvider
// implementation (a ChatProviderRegistry key, for example
// "chat.openai-compatible") plus the concrete vendor model id that provider
// should call (for example "gpt-4o-mini").
type ModelRoute struct {
	// Provider is the ChatProviderRegistry name to resolve.
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
// already are in pkgcore -- not a value a tenant or an operator tunes at
// runtime through the admin console. A future round MAY choose to make
// routing config-driven instead (this package's own doc comment records
// that as an open, deliberate choice, not an oversight); this round keeps
// it a Go-level option so the whole pipeline is provably wired at process
// start, with no possibility of a route silently changing mid-request.
//
// Calling WithModelRoute twice for the same logicalKey replaces the
// earlier route -- there is no duplicate-registration error, because
// GatewayOptions apply in the order given and "last wins" is the ordinary
// functional-option convention every other Gateway/Module option in this
// codebase already follows.
//
// Business code that calls Gateway.Chat/ChatStream with an unrouted
// logicalKey gets ErrUnroutedModel -- never a silent fallback to some
// default provider or vendor model.
func WithModelRoute(logicalKey, provider, vendorModel string) GatewayOption {
	return func(g *Gateway) {
		g.routes[logicalKey] = ModelRoute{Provider: provider, VendorModel: vendorModel}
	}
}
