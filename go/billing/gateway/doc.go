// Package gateway provides payment gateway adapters for Stripe, Alipay, and
// WeChat Pay.
//
// It is a subpackage of go/billing rather than a module of its own: a
// subpackage already isolates its dependencies completely -- the effect
// reaches go.mod, go.sum and minimal version selection, so a project that
// takes no payments and never imports this package carries none of the three
// payment SDKs. Modules are release units divided by domain cohesion, and
// under lockstep versioning each one costs a go.work entry, a CI matrix row,
// an AGENTS.md and a version tag that a subpackage does not.
//
// The module-boundary rules this packaging choice rests on are stated in
// the deployment-modes design doc; this package's own design rationale
// lives in the billing design doc.
package gateway
