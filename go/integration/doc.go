// Package integration provides a tenant's outward-facing API surface: API
// keys a tenant issues to its own scripts and third-party systems, the
// three-layer rate limiting that protects the platform, the tenant and the
// individual key from one another, and outbound webhooks that turn a
// business module's internal domain events into signed HTTP deliveries.
//
// # Scope so far
//
// Round 1: API key issuance, listing, rotation and revocation (model.go,
// service.go), plus the rate-limiting composition built on go/ratelimit
// (ratelimit.go) and its HTTP 429 translation (httpguard.go).
//
// Round 2: webhook subscription management (webhook_model.go,
// webhook_service.go), the internal-to-public event schema mapping
// mechanism (eventmapping.go's EventMapping, module.go's WithEventMapping),
// event-driven delivery via go/jobs with HMAC signing and dead-lettering
// (webhook_delivery.go, webhook_signature.go), and SSRF-protected delivery
// at both subscription-creation and delivery-dial time (ssrf.go).
//
// Neither round mounts an HTTP surface (Module.OpenAPISpec returns nil) or
// wires the reference app as a consumer; see AGENTS.md's "Deliberately not
// in scope" table and "No reference-app consumer yet" section for both
// rounds' exact boundaries and the compensating obligations that carries.
//
// # Design
//
// See docs/internal/07-platform-services.md's integration section (the
// module's target design, covering both API key management and outbound
// webhooks) and go/integration/AGENTS.md for what has actually landed, its
// adjudications, and its known limitations.
package integration
