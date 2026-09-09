// Package twilio is the Twilio SMS adapter for pkgcore's SMSSender seam.
//
// It delivers the seam's messages through Twilio's REST Messages resource
// (API version 2010-04-01,
// https://api.twilio.com/2010-04-01/Accounts/{AccountSid}/Messages.json),
// authenticated with HTTP Basic auth over the AccountSID and AuthToken pair
// and submitting an application/x-www-form-urlencoded body of To, From (or
// MessagingServiceSid) and Body, exactly the request shape Twilio's own
// documentation fixes (twilio.com/docs/messaging/api/message-resource). No
// SDK is used -- the no-SDK choice and its measured dependency cost are
// recorded in go/pkgcore/AGENTS.md's "SMS carrier adapters" section.
//
// # Free text
//
// Twilio is the one carrier among this module's adapters whose Messages API
// accepts free text: Body carries the seam's already-rendered message
// verbatim, no template or signature content registration is involved.
//
// # Phone numbers and senders
//
// The seam passes SMS.To through unchanged (pkgcore's own contract: SMSSender
// implementations never normalize). Twilio requires the recipient's E.164
// form with a leading "+" (e.g. "+8613800000000"); a number in any other
// form is answered with Twilio's own 21211-class error, surfaced through
// Send's error. The sender is exactly one of Config.From (a Twilio-owned
// number, short code or alphanumeric sender ID) and Config.MessagingServiceSID
// (a Messaging Service whose sender pool Twilio picks from -- the shape
// verification-code traffic commonly routes through).
//
// # Construction and transport
//
// NewSender validates Config eagerly and returns an error naming the missing
// field (never its value -- the AuthToken is a credential and must not echo)
// or the From/MessagingServiceSID conflict. The default HTTP client is
// pkgcore/safehttp's guarded client, the same SSRF-guarded, timeout-bounded
// client pkgcore's own NewHTTPSMSSender dials through; WithClient replaces
// it (a test pointing a sender at an httptest loopback
// server must inject a plain client, exactly as NewHTTPSMSSender's own tests
// do). The gateway base URL is a fixed constant of this package -- no
// operator-chosen URL exists anywhere in this adapter, so no scheme check is
// needed before a send.
//
// # Secrets
//
// AuthToken never leaves the Config/NewSender boundary except inside the
// Basic-auth header (which TLS protects), and no error or log path in this
// package echoes a credential.
//
// # Offline proof and the live boundary
//
// The request carries no signature -- Basic auth and a form body are fully
// deterministic -- so the unit tier pins the whole wire shape (URL, auth
// header, body) offline, values precomputed independently. What no offline
// test can prove -- that a real Twilio account accepts this adapter's
// request at the real API -- is the env-gated leg in integration_test/,
// which self-skips with a recorded note until TWILIO_SMS_* credentials are
// present (the alipay sandbox-leg precedent).
package twilio
