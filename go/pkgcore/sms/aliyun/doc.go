// Package aliyun is the Aliyun SMS (Dysmsapi) adapter for pkgcore's SMSSender
// seam.
//
// It delivers the seam's messages through Aliyun's SendSms action of the
// Dysmsapi product (API version 2017-05-25, gateway
// https://dysmsapi.aliyuncs.com/), signed with the RPC HMAC-SHA1 mechanism
// Aliyun's own signature documentation specifies (help.aliyun.com, "RPC
// mechanism": canonicalized query string sorted by key, percent-encoded per
// RFC 3986, then
//
//	stringToSign = HTTPMethod + "&" + "%2F" + "&" + percentEncode(canonicalQueryString)
//	signature   = base64(HMAC-SHA1(AccessKeySecret + "&", stringToSign))
//
// implemented directly over the standard library (crypto/hmac, crypto/sha1).
// The no-SDK choice and its measured dependency cost are recorded in
// go/pkgcore/AGENTS.md's "SMS carrier adapters" section.
//
// # The template boundary
//
// Dysmsapi has no free-text send: every message is one approved template
// (TemplateCode) substituted with a JSON TemplateParam object. This adapter
// maps the seam's already-rendered message text onto the template's SINGLE
// variable: TemplateParam is always {"<TemplateParamName>": "<message text>"},
// where Config.TemplateParamName names that variable and defaults to
// "content". A template with zero variables would drop the message and one
// with several cannot be driven by a single text, so the registered template
// must declare exactly one variable under this name. Config.TemplateParamName
// exists because the variable name is data of the operator's Aliyun account,
// not data this codebase could know.
//
// # Phone numbers
//
// The seam passes SMS.To through unchanged (pkgcore's own contract: SMSSender
// implementations never normalize). Aliyun's published SendSms documentation
// accepts a domestic number with "+", "+86", "0086", "86" or no prefix, and
// an international number as country code plus number, so an E.164 number
// the sending module's flows produce is acceptable as-is; numbers in any
// other form are the caller's to reconcile with Aliyun's rules. Aliyun
// answers a malformed number with a business error envelope, never an HTTP
// error, which surfaces through Send's error.
//
// # Construction and transport
//
// NewSender validates Config eagerly and returns an error naming the missing
// field (never its value -- the secret is a credential and must not echo).
// The default HTTP client is pkgcore/safehttp's guarded client, the same
// SSRF-guarded, timeout-bounded client pkgcore's own NewHTTPSMSSender dials
// through; WithClient replaces it (a test pointing a sender at an httptest
// loopback server must inject a plain client, exactly as NewHTTPSMSSender's
// own tests do). The gateway endpoint is a fixed constant of this package --
// no operator-chosen URL exists anywhere in this adapter, so no scheme check
// is needed before a send.
//
// # Secrets
//
// AccessKeySecret never leaves the Config/NewSender boundary except inside
// the HMAC key derivation, and no error or log path in this package echoes a
// credential. The whole request -- TemplateParam JSON included -- rides over
// TLS; its parameters travel percent-encoded in the request line's query
// string with an empty body, exactly the placement both official dysmsapi
// SDK generations use, so the message text reaches Aliyun's gateway as it
// reaches this package's own seam contract: content to deliver, over an
// encrypted channel.
//
// # Offline proof and the live boundary
//
// The signing is deterministic given Timestamp and SignatureNonce, so the
// unit tier pins both the RPC mechanism against Aliyun's own documented
// worked example and a Send-shaped request (fixed clock and nonce injected
// through unexported fields) against a signature precomputed by an
// independent implementation. What no offline test can prove -- that a real
// Aliyun account accepts this package's signature at the real gateway -- is
// the env-gated leg in integration_test/, which self-skips with a recorded
// note until ALIYUN_SMS_* credentials are present (the alipay sandbox-leg
// precedent).
package aliyun
