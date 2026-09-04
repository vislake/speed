// Package alipay is billing's Alipay adapter -- one leg of
// docs/internal/06-billing-and-metering.md's domestic payment mode. Alipay
// offers no periodic-charge primitive at this repository's target tier, so
// this package creates one-time orders only (via alipay.trade.precreate,
// the Native/QR-code product), and a caller re-runs
// PaymentGateway.CreateCharge once per internally-tracked billing cycle --
// see billing.PaymentGateway.CreateCharge's own doc comment, and
// go/billing/gateway/AGENTS.md's own pragmatic-trade-off section, for the full
// rationale.
// This package does NOT implement Alipay's periodic-withhold product,
// which is a distinct, higher-compliance-burden API family explicitly out
// of this round's scope.
//
// # SDK choice: no third-party SDK
//
// Alipay publishes no official Go SDK, and no single community package
// dominates the way stripe-go does for Stripe (a survey at the time of this
// round found several partial, inconsistently maintained options, none
// carrying Ant Group's own name or a release cadence this codebase could
// depend on with confidence). Signature verification and the request/
// response shapes this package needs are implemented directly against
// Alipay's own published open-platform documentation (RSA2 signing:
// https://opendocs.alipay.com/common/02kdnc) using only the standard
// library's crypto/rsa, crypto/sha256 and crypto/x509 -- the same choice
// go/pki's own root package makes for LocalSigner, and the identical
// "implement directly against the documented format" instruction this
// round's own task gave for both non-Stripe providers.
//
// # What is, and is not, exercised without live credentials
//
// VerifySignature performs no network call: it recomputes Alipay's own
// RSA2 canonical parameter string and verifies the caller-supplied
// signature against it with a configured Alipay public key, and is fully
// unit-tested offline against a request this package signs itself with a
// locally generated RSA key pair -- there is no published third-party test
// vector for Alipay's signature scheme the way Stripe ships
// webhook.GenerateTestSignedPayload, so a self-constructed fixture is the
// sanctioned alternative (this round's own task explicitly names it: "a
// locally-constructed fixture signed with a test key pair"). CreateCharge
// and QueryStatus build and sign a real alipay.trade.precreate /
// alipay.trade.query request and are unit-tested against a scripted
// httpDoer double that asserts the exact signed form body this package
// sends and returns a canned response -- proof the request/response shapes
// and the outgoing RSA2 signature are correct, never proof against
// Alipay's real, live open platform. No integration tier exists (see
// go/billing/gateway/AGENTS.md's Known limitations): Alipay's sandbox
// requires a registered application and merchant credentials this
// repository does not hold today. Real verification against Alipay's
// sandbox or production environment has not been performed as part of this
// round.
package alipay
