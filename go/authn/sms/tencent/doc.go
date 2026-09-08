// Package tencent is the Tencent Cloud SMS adapter for go/authn's SMSSender
// seam.
//
// It delivers the seam's messages through Tencent Cloud SMS's SendSms action
// (API version 2021-01-11, gateway https://sms.tencentcloudapi.com/), signed
// with the TC3-HMAC-SHA256 signature Tencent's own API-3.0 documentation
// specifies (cloud.tencent.com, "Tencent Cloud API signature method v3":
// a canonical request over method, "/", the content-type and host headers
// and the SHA-256 of the payload, a string-to-sign binding the timestamp
// and the date/service/tc3_request credential scope, and a key derivation
// chain seeded with "TC3"+SecretKey), implemented directly over the standard
// library (crypto/hmac, crypto/sha256). The signed-header set -- content-type
// and host only, no x-tc-action -- mirrors the current official
// tencentcloud-sdk-go signer byte for byte, since that signing shape is the
// one exercised against Tencent's real gateway by every production SDK
// customer. The no-SDK choice and its measured dependency cost are recorded
// in go/authn/AGENTS.md's "SMS carrier adapters" section.
//
// # The template boundary
//
// Tencent SMS has no free-text send: every message instantiates one approved
// template (TemplateID) whose variables are substituted POSITIONALLY by
// TemplateParamSet. This adapter maps the seam's already-rendered message
// text onto the template's first (and only) positional parameter, always
// sending a single-element TemplateParamSet. A template with zero variables
// would drop the message and one with several cannot be driven by a single
// text, so the registered template must declare exactly one variable.
//
// # Phone numbers
//
// The seam passes SMS.To through unchanged (go/authn's own contract: SMSSender
// implementations never normalize). Tencent's SendSms reference requires the
// E.164 "+[country code][number]" form (domestic numbers additionally accept
// "0086", "86" or a bare 11-digit form), so the E.164 form go/authn's phone
// flows produce is acceptable as-is; numbers in any other form are the
// caller's to reconcile with Tencent's rules. Tencent answers a malformed
// number with a per-number refusal inside the response envelope, which
// surfaces through Send's error.
//
// # Construction and transport
//
// NewSender validates Config eagerly and returns an error naming the missing
// field (never its value -- the secret is a credential and must not echo).
// Config.Region defaults to ap-guangzhou. The default HTTP client is
// authn/internal/safehttp's guarded client, the same SSRF-guarded,
// timeout-bounded client the module's own NewHTTPSMSSender dials through;
// WithClient replaces it (a test pointing a sender at an httptest loopback
// server must inject a plain client, exactly as NewHTTPSMSSender's own tests
// do). The gateway endpoint is a fixed constant of this package -- no
// operator-chosen URL exists anywhere in this adapter, so no scheme check is
// needed before a send.
//
// # Secrets
//
// SecretKey never leaves the Config/NewSender boundary except inside the TC3
// key derivation, and no error or log path in this package echoes a
// credential. The payload rides in the POST body over TLS; only the
// Authorization header (which embeds the non-secret SecretID) is visible in
// the request line's headers.
//
// # Offline proof and the live boundary
//
// The TC3 signature is deterministic given the timestamp, so the unit tier
// pins the canonical request and the final Authorization header against
// values precomputed by an independent implementation. What no offline test
// can prove -- that a real Tencent Cloud account accepts this package's
// signature at the real gateway -- is the env-gated leg in integration_test/,
// which self-skips with a recorded note until TENCENT_SMS_* credentials are
// present (the alipay sandbox-leg precedent).
package tencent
