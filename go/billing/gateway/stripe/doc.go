// Package stripe is billing's Stripe adapter -- the international leg of
// docs/internal/06-billing-and-metering.md's domestic-plus-international dual
// payment mode: Stripe's own
// native recurring-subscription primitives, used directly, with no
// internally-managed billing cycle standing in the way (unlike this
// repository's Alipay/WeChat adapters, whose channels have no recurring-
// charge primitive at all -- see go/billing/gateway/AGENTS.md's own
// pragmatic-trade-off write-up).
//
// # SDK choice
//
// This package uses the official github.com/stripe/stripe-go SDK -- the
// only Go client Stripe itself publishes and maintains, and the idiomatic
// choice for any Go integration against Stripe's API. Within that SDK,
// Gateway is built over the PACKAGE-LEVEL per-resource clients
// (github.com/stripe/stripe-go/v82/checkout/session's and .../subscription's
// own Client{B: stripe.Backend, Key: string} types) rather than the newer
// stripe.Client aggregate the SDK's own docs now recommend for new
// integrations -- a deliberate, documented departure, not an oversight:
// stripe.Client's own per-resource fields (Client.V1CheckoutSessions and
// friends) are concrete, unexported struct types with no interface seam,
// so nothing about them can be swapped for a test double, and this
// package's whole testing strategy (VerifyWebhook's real, offline signature
// verification; every other method exercised against a scripted double, per
// go/billing/gateway/AGENTS.md's own testing-strategy write-up) depends on
// stripe.Backend being a real, first-class interface -- its own doc comment
// says so outright: "this interface exists to enable mocking for during
// testing if needed". The per-resource Client type takes exactly that
// Backend, constructed once in NewGateway and reused for every call this
// package makes, so a unit test injects a scripted stripe.Backend and
// asserts the exact request this package builds without a live Stripe
// account. The per-resource Client types are marked Deprecated in the SDK's
// own doc comments ("use stripe.Client instead") but remain fully shipped
// and functional as of the pinned version -- see go/billing/gateway/AGENTS.md
// for the full write-up of this trade-off.
//
// # What is, and is not, exercised without live credentials
//
// VerifyWebhook performs no network call at all -- it is real HMAC-SHA256
// signature verification (Stripe's own webhook.ConstructEvent) against the
// raw delivered bytes, and is fully unit-tested offline using
// webhook.GenerateTestSignedPayload, the SDK's own sanctioned test fixture
// generator. CreateCharge and QueryStatus are real Stripe API calls,
// exercised in this package's own unit tests against a scripted
// stripe.Backend double that asserts the exact HTTP method/path/params this
// package sends and returns a canned response -- proof the request/response
// shapes are correct, never proof against Stripe's real, live API. No
// integration tier exists (see go/billing/gateway/AGENTS.md's Known
// limitations): Stripe's own published Go SDK test suite already needs a
// live test-mode account, and this repository ships no sandbox credentials
// today. Real verification against a live Stripe test-mode account has not
// been performed as part of this round.
package stripe
