// Package ratelimit provides a KVStore-backed rate limiter shared by modules
// that need to limit how often something happens. It is deliberately narrow:
// one dimension per call, and no protocol-shaped behavior or opinion on what
// a caller does with a denial -- a Decision is plain data, and translating it
// into a response is the caller's job (the one protocol-vocabulary helper the
// package ships, RetryAfterSeconds, is a pure duration-to-whole-seconds
// conversion, not a translation). It is a pure library: it implements no
// pkgcore.Module and registers nothing, so a consumer just calls ratelimit.New.
package ratelimit
