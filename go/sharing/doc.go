// Package sharing implements public share links: a controlled entry point
// that lets an unauthenticated external visitor view one internal resource,
// distinct from storage's presigned URLs (which are for already-authenticated
// internal users). See docs/internal/07-platform-services.md's sharing
// section for the design this module implements, and this module's own
// AGENTS.md for what round 1 ships and what it deliberately does not.
//
// # Round 1 scope
//
// This round ships the Share domain model, the Service that creates,
// accesses and revokes share links, the access log a resource owner reads
// back, and the jobs-driven expiry sweep -- the Service/Access API only. It
// does not mount an HTTP surface (no unauthenticated public endpoint exists
// yet -- a later round adds one) and it is not wired into the reference app
// as a live consumer; see AGENTS.md's "No real consumer yet" section for the
// compensating obligations that carries.
//
// # The five mandatory rules
//
// Every rule docs/internal/07-platform-services.md's "mandatory rules" list
// states is enforced here, each with a passing test: tokens are
// cryptographically random and never derived from a predictable value
// (token.go); a share created with no explicit expiry is forced onto the
// tenant's configured default, and a caller that explicitly asks for a
// never-expiring link is refused outright, never silently granted
// (service.go's Create); revocation takes effect on the very next access
// check, with no cache anywhere on this module's own side (service.go's
// Access re-reads the row on every call); every access is logged --
// timestamp, viewer IP, user agent and referrer -- into a tenant-scoped
// access log a resource owner reads back (repository.go's
// AccessLogRepository, service.go's ListAccessLog); and an invalid,
// expired, revoked, view-exhausted or wrong-password token all produce the
// exact same outward "not accessible" answer, so probing a token teaches an
// attacker nothing about which of those it actually is (service.go's
// ErrNotAccessible).
package sharing
