package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// This file implements docs/internal/07-platform-services.md's "HMAC
// signature + timestamp" requirement: every delivered payload is signed
// with the subscription's own secret and carries a timestamp, so the
// receiving end
// can verify authenticity (the body was not tampered with, and it really
// came from this deployment) and reject replays outside a tolerance window.
//
// The scheme deliberately follows the shape Stripe's and GitHub's own
// webhook signatures use, which most receiving-end HTTP frameworks already
// have off-the-shelf verification middleware for: a signature computed over
// "<timestamp>.<raw body>", not the body alone. Including the timestamp
// INSIDE the signed content -- not just alongside it as an unsigned header
// -- is what makes replay detection possible at all: an attacker who
// captures one genuine (body, signature) pair cannot re-send it with a
// forged, later timestamp, because the signature would no longer match the
// forged timestamp. A receiver is expected to: (1) recompute the signature
// over "<X-Speed-Webhook-Timestamp>.<raw body>" with its own copy of the
// subscription secret and compare it to X-Speed-Webhook-Signature in
// constant time, and (2) separately reject any request whose timestamp is
// further from the receiver's own clock than
// WebhookReplayTolerance -- both checks are the receiver's own
// responsibility; this module signs and stamps, and has no receiving side
// of its own to enforce either half against (see AGENTS.md's "Deliberately
// not in scope" table).
const (
	// HeaderWebhookID carries the delivering WebhookDelivery.ID, so a
	// receiver can deduplicate on it directly rather than only via the
	// signature/timestamp pair.
	HeaderWebhookID = "X-Speed-Webhook-Id"

	// HeaderWebhookTimestamp carries the Unix time (seconds, base-10) the
	// delivery attempt was made, exactly as signed.
	HeaderWebhookTimestamp = "X-Speed-Webhook-Timestamp"

	// HeaderWebhookSignature carries the scheme-prefixed hex-encoded
	// HMAC-SHA256 signature, "v1=<hex>" -- the "v1=" scheme prefix, not
	// currently meaningful beyond documentation, follows Stripe's own
	// header convention so that a future signing-algorithm change can add a
	// second "v2=..." token to the same header without breaking a receiver
	// still checking for "v1=".
	HeaderWebhookSignature = "X-Speed-Webhook-Signature"
)

// WebhookReplayTolerance is the maximum age docs/internal/07-platform-
// services.md's replay-rejection rule implies a receiver should accept
// between HeaderWebhookTimestamp and its own clock. Exported purely as
// documented guidance for a receiver's own verification code (this module
// signs; it has no receiving side to enforce the tolerance itself -- see
// this file's own header comment) rather than something any code in this
// module reads.
const WebhookReplayTolerance = 5 * time.Minute

// webhookSignatureScheme is HeaderWebhookSignature's prefix.
const webhookSignatureScheme = "v1"

// signWebhookPayload computes the hex-encoded HMAC-SHA256 of
// "<timestamp>.<body>" keyed by secret, and returns it already prefixed
// with webhookSignatureScheme -- the exact value HeaderWebhookSignature
// carries.
func signWebhookPayload(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return webhookSignatureScheme + "=" + hex.EncodeToString(mac.Sum(nil))
}
