// Package wechat is billing's WeChat Pay adapter -- the second
// domestic leg of docs/internal/06-billing-and-metering.md's payment mode
// split. Like go/billing/gateway/alipay, WeChat Pay offers no
// periodic-charge primitive at this repository's target tier, so this
// package creates one-time Native (QR-code) orders only via the APIv3
// `POST /v3/pay/transactions/native` operation, and a caller re-runs
// PaymentGateway.CreateCharge once per internally-tracked billing cycle --
// see billing.PaymentGateway.CreateCharge's own doc comment, and
// go/billing/gateway/AGENTS.md's own pragmatic-trade-off section, for the
// full
// rationale. This package does NOT implement WeChat Pay's periodic
// withhold agreement product, a distinct, higher-compliance-
// burden API family explicitly out of this round's scope.
//
// # SDK choice: no third-party SDK
//
// WeChat Pay's own officially maintained Go SDK
// (github.com/wechatpay-apiv3/wechatpay-go) exists, but this package
// deliberately does not adopt it: its own certificate-management layer
// (`core/downloader`) is built to periodically DOWNLOAD WeChat Pay's
// current platform certificates over the network and cache them, machinery
// this round has no live account to exercise or verify, and adopting it
// would mean carrying an unverified, untested certificate-rotation
// dependency rather than the explicit, host-supplied platform public key
// this package uses instead (see "Known limitation: static platform
// certificate" below). Signature verification, resource decryption and the
// request/response shapes this package needs are implemented directly
// against WeChat Pay's own published APIv3 documentation (signature scheme:
// https://pay.weixin.qq.com/doc/v3/merchant/4012774368) using only the
// standard library's crypto/rsa, crypto/sha256, crypto/aes and
// crypto/cipher -- the identical "implement directly against the
// documented format" choice go/billing/gateway/alipay makes, and this
// round's own task explicitly sanctioned for both non-Stripe providers.
//
// # Known limitation: static platform certificate
//
// WeChat Pay APIv3 signs both outbound API responses and inbound webhook
// notifications with certificates it itself issues and periodically
// rotates, discoverable through a `GET /v3/certificates` call. This
// package does not implement that discovery/rotation flow: Config takes
// the platform's current RSA public key directly (PEM-encoded), and a host
// is responsible for keeping it current -- an operational gap this
// package's AGENTS.md records explicitly rather than silently accepting an
// expired or rotated certificate would (this package fails closed with a
// verification error in that case, never a silent accept).
//
// # What is, and is not, exercised without live credentials
//
// VerifyWebhook performs no network call: it recomputes WeChat Pay's own
// documented RSA-SHA256 signature over the exact
// "timestamp\nnonce\nbody\n" message and verifies it against the
// configured platform public key, then decrypts the notification's
// AEAD_AES_256_GCM resource ciphertext with the configured APIv3 key --
// both fully unit-tested offline against fixtures this package constructs
// and signs/encrypts itself with a locally generated key pair and a random
// APIv3 key, the same "locally-constructed fixture" strategy
// go/billing/gateway/alipay's own doc.go documents (WeChat Pay ships no
// published third-party Go test vector either). CreateCharge and
// QueryStatus build and sign a real WeChat Pay APIv3 request (the
// documented WECHATPAY2-SHA256-RSA2048 Authorization scheme) and are
// unit-tested against a scripted httpDoer double that asserts the exact
// signed request this package sends and returns a canned response -- proof
// the request/response shapes and the outgoing signature are correct,
// never proof against WeChat Pay's real, live API. No integration tier
// exists (see go/billing/gateway/AGENTS.md's Known limitations): WeChat
// Pay's sandbox requires a registered merchant account this repository
// does not hold today. Real verification against WeChat Pay's sandbox or
// production environment has not been performed as part of this round.
package wechat
