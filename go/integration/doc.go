// Package integration provides a tenant's outward-facing API surface: API
// keys a tenant issues to its own scripts and third-party systems, the
// three-layer rate limiting that protects the platform, the tenant and the
// individual key from one another, and outbound webhooks that turn a
// business module's internal domain events into signed HTTP deliveries.
//
// # Scope
//
// API key issuance, listing, rotation and revocation (model.go, service.go,
// authenticate.go), the layered rate limiting built on go/ratelimit
// (ratelimit.go) and its HTTP 429 translation (httpguard.go). Webhook
// subscription management (webhook_model.go, webhook_service.go), the
// internal-to-public event schema mapping mechanism (eventmapping.go's
// EventMapping, module.go's WithEventMapping), event-driven delivery via
// go/jobs with HMAC signing, dead-lettering and retry
// (webhook_delivery.go, webhook_signature.go), and SSRF-protected delivery
// at both subscription-creation and delivery-dial time (webhook_guard.go,
// built on go/pkgcore/safehttp).
//
// The module's HTTP surface (api/openapi.yaml) covers both halves -- the
// API-key operations and the webhook-subscription CRUD under /webhooks --
// mounted through reg.Routes and implemented by handler.go's Handler
// behind a generated api.ServerInterface. The reference app wires the
// module end to end behind its own permission gates. What is deliberately
// not shipped -- no manual-redelivery endpoint, no frontend consumer of
// the fragment -- is recorded as known limitations of the module.
package integration
