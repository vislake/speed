// Package integration provides a tenant's outward-facing API surface: API
// keys a tenant issues to its own scripts and third-party systems, and the
// three-layer rate limiting that protects the platform, the tenant and the
// individual key from one another.
//
// # Scope of this round
//
// This is round 1 of the module: API key issuance, listing, rotation and
// revocation (model.go, service.go), plus the rate-limiting composition
// built on go/ratelimit (ratelimit.go) and its HTTP 429 translation
// (httpguard.go). Outbound webhooks -- event subscription, the internal-to
// -public event schema mapping, HMAC signing, SSRF-protected delivery,
// retry and dead-lettering -- are docs/internal/07-platform-services.md's
// other half of "integration" and are deliberately not built here; see
// AGENTS.md's "Deferred to a later round" section.
//
// # Design
//
// See docs/internal/07-platform-services.md's "对外集成：API 开放与外发
// Webhook（integration）" section for the target design and
// go/integration/AGENTS.md for what has actually landed, its adjudications,
// and its known limitations.
package integration
