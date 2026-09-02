// Package ratelimit provides a KVStore-backed rate limiter shared by modules
// that need to limit how often something happens. It is deliberately narrow:
// one dimension per call, no HTTP or protocol awareness, and no opinion on
// what a caller does with a denial. See AGENTS.md for the full design intent
// and docs/internal/11-cross-cutting.md's rate-limiting section for the discussion
// this module implements.
package ratelimit
