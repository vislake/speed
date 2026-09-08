// Package sharing implements public share links: a controlled entry point
// that lets an unauthenticated external visitor view one internal resource,
// distinct from storage's presigned URLs (which are for already-authenticated
// internal users).
//
// # Scope
//
// The Share domain model, the Service that creates, accesses and revokes
// share links, and the access log a resource owner reads back. The Service
// surface spans the in-process methods (Create, Access, Revoke, Get,
// ListAccessLog, List), the genuinely unauthenticated entry point
// Service.AccessPublic (tenant resolution from the bearer token alone), and
// the jobs-driven expiry sweep. Two HTTP surfaces implement it: the public
// access route (api/openapi.yaml's sharing_accessShare, handler.go) and the
// owner-facing operations under PathShares. The ResourceResolver seam turns
// a Share's ResourceRef into actual bytes, go/ratelimit gates Create and
// every AccessPublic entry, and the reference app wires the module end to
// end as a real HTTP consumer. What is deliberately not shipped -- no live
// consumer of the sharing.share.accessed event, no owner-facing frontend
// surface -- is recorded as known limitations of the module; the serving
// protocol and its guarantees are documented on the methods themselves
// (handler.go's SharingAccessShare, service.go's reserve/confirm/refund
// trio).
//
// # The five mandatory rules
//
// The design's five mandatory rules for sharing are enforced here, each
// with a passing test: tokens are cryptographically random and never
// derived from a predictable value (token.go); a share created with no
// explicit expiry is forced onto the tenant's configured default, and a
// caller that explicitly asks for a never-expiring link is refused
// outright, never silently granted (service.go's Create); revocation takes
// effect on the very next access check, with no cache anywhere on this
// module's own side (service.go's Access re-reads the row on every call);
// every access is logged -- timestamp, viewer IP, user agent and referrer
// -- into a tenant-scoped access log a resource owner reads back
// (repository.go's AccessLogRepository, service.go's ListAccessLog); and an
// invalid, expired, revoked, view-exhausted or wrong-password token all
// produce the exact same outward "not accessible" answer, so probing a
// token teaches an attacker nothing about which of those it actually is
// (service.go's ErrNotAccessible).
package sharing
