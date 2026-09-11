// Package testkit holds the small primitives every suite that observes
// asynchronous behavior or speaks the platform's wire and context
// conventions reaches for: a bounded convergence poll (Eventually), a
// tenant context constructor (TenantCtx), the {code, params} error-envelope
// decoder (DecodeErrorBody), a concurrency-safe event recorder
// (EventRecorder) and a run-channel receive (WaitForRun). One home for each
// keeps the semantics uniform across every tier that waits on asynchronous
// work, instead of each suite's private copy drifting on its own.
//
// The package sits in pkgcore -- underneath every module -- so any module's
// test tiers, and a consuming application's own tests, can import it: a
// module-local testutil could not serve pkgcore's own tiers, and none of
// these primitives is specific to one module. It deliberately holds nothing
// but plumbing: what a suite asserts about the behavior under test stays in
// that suite.
package testkit
