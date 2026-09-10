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
// Dysmsapi has no free-text send: every message instantiates one approved
// template (TemplateCode) substituted with a JSON TemplateParam object.
// Aliyun has one approved template per kind of message -- there is no
// all-variable template, and a single variable is limited to 35 characters
// by the template review rules -- so Config.Templates declares, per message
// identity, which approved template serves it: the map is keyed
// "<locale>/<message-id>" (the locale the message was rendered in and the
// identity it was rendered from, both carried by pkgcore.SMS) and each entry
// names the template code plus the variables that template declares. Send
// selects the entry by (Locale, MessageID), forwards exactly the declared
// variables from SMS.Params, and refuses -- before any request, so nothing
// reaches Aliyun and nothing is billed -- a message with no mapped template
// or a declared variable the message carries no value for. There is no
// fallback template and no free-text path: which templates exist, and which
// variables each declares, is data of the operator's Aliyun account that
// this codebase cannot know, and guessing would be a send the operator
// never approved. A message parameter the mapped template does not declare
// is never sent; the operator may have baked that value into the template's
// fixed text.
//
// The template's declared variables are forwarded as given, so choosing
// values that satisfy Aliyun's own review rules -- the 35-character cap on a
// single variable is the notable one -- is the operator's to observe on the
// templates the account registers; this adapter deliberately checks no
// length, because the cap differs by message class and a hard-coded check
// would refuse sends Aliyun itself accepts.
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
// credential. The TemplateParam JSON carries the mapped template's variable
// values, which may be a credential (a login verification code); an error
// this package returns names a missing variable, never its value. The whole
// request rides over TLS, its parameters percent-encoded in the request
// line's query string with an empty body, exactly the placement both
// official dysmsapi SDK generations use.
//
// # Offline proof and the live boundary
//
// The signing is deterministic given Timestamp and SignatureNonce, so the
// unit tier pins both the RPC mechanism against Aliyun's own documented
// worked example and a Send-shaped request (fixed clock and nonce injected
// through unexported fields, a mapped template selected by (locale,
// message-id)) against a signature precomputed by an independent
// implementation; the same tier pins that an unmapped message and a missing
// declared variable are refused before any request. What no offline test can
// prove -- that a real Aliyun account accepts this package's signature at
// the real gateway -- is the env-gated leg in integration_test/, which
// self-skips with a recorded note until ALIYUN_SMS_* credentials are present
// (the alipay sandbox-leg precedent).
package aliyun
