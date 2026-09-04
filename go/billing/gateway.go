package billing

import (
	"context"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// This file is billing's half of the one-way dependency rule
// docs/internal/06-billing-and-metering.md's dependency-direction-must-be-one-way paragraph
// states: go/billing/gateway (and its stripe/alipay/wechat sub-subpackages)
// may import go/billing, to turn a channel's own event shape into
// PaymentGateway's provider-agnostic types below -- but billing's own root
// package must never import go/billing/gateway or anything under it. billing
// consumes a channel through PaymentGateway, an interface declared here;
// PaymentGatewayRegistry is the database/sql-style seam a provider
// subpackage's own init() registers a concrete implementation into (mirroring
// go/pki's SignerRegistry -- see that file's own doc comment for the identical
// shape, including the WithSigner-vs-SignerRegistry duality this seam
// repeats: a host that already holds a constructed PaymentGateway wires it
// directly wherever one is needed -- PollingService's gateways map this
// round, or a later round's webhook handler -- while PaymentGatewayRegistry
// is for a host that wants to build one by name plus a flat pkgcore.Config,
// the shape a Preset or an environment-driven bootstrap naturally produces).
//
// Getting this backwards is exactly what
// docs/internal/06-billing-and-metering.md warns against by name: a
// provider-specific type (a stripe.Subscription-shaped field, an Alipay
// notify param) leaking into billing's own domain model, coupling
// Subscription/Invoice's channel-agnostic design (subscription.go's own doc
// comment) to whichever channel happened to be imported first. The
// depguard rules this round adds to .golangci.yml (stripe-sdk-only-in-
// billing-gateway-stripe and its two siblings) are the CI-enforced half of
// this rule; this file, and the fact that no provider SDK's import ever
// appears in this package's own go.mod requirement list as anything other
// than what a subpackage needs (go/billing/gateway/AGENTS.md's Compile-time
// / packaging section has the precise claim and the pruned-module-graph
// mechanism behind it), are the other half.

// ChannelReference identifies one channel-side collection object: a Stripe
// Checkout Session id, an Alipay/WeChat out_trade_no, or whatever else a
// future provider names its own equivalent. Opaque to billing -- nothing in
// this package parses or compares it beyond equality, exactly like Plan.ID
// or Subscription.ID are opaque to their own callers.
type ChannelReference string

// ChannelStatus is a channel-agnostic classification of one charge or
// order's state, shared by NormalizedEvent.Status (what a webhook reported)
// and PaymentGateway.QueryStatus's own return (what the channel's API
// reports right now) so a caller compares the two without a second
// vocabulary.
type ChannelStatus string

const (
	// ChannelStatusPending is a charge or order still awaiting the buyer's
	// action (an unpaid Checkout Session, an Alipay/WeChat order not yet
	// scanned or confirmed).
	ChannelStatusPending ChannelStatus = "pending"
	// ChannelStatusSucceeded is a charge or order the channel has settled.
	ChannelStatusSucceeded ChannelStatus = "succeeded"
	// ChannelStatusFailed is a charge or order the channel declined or that
	// expired before completion.
	ChannelStatusFailed ChannelStatus = "failed"
	// ChannelStatusCanceled is a channel-side subscription (Stripe) that
	// was canceled, distinct from ChannelStatusFailed -- a cancellation is
	// a deliberate stop, not a declined attempt.
	ChannelStatusCanceled ChannelStatus = "canceled"
	// ChannelStatusRefunded is a charge the channel has since refunded,
	// full or partial -- QueryStatus and NormalizedEvent both collapse
	// partial and full refunds to this one value; a caller needing the
	// exact refunded amount reads NormalizedEvent.Amount or re-queries the
	// channel directly, this package's own scope stopping at "was this
	// charge refunded at all".
	ChannelStatusRefunded ChannelStatus = "refunded"
)

// NormalizedEventType is a channel-agnostic classification of what kind of
// thing happened, independent of ChannelStatus (which reports the state a
// charge or subscription is now IN). Two events can carry the same Status
// with different Types -- a fresh charge succeeding and a later refund
// event both ultimately concern a charge that is no longer "pending", but
// only one of them means new money arrived.
type NormalizedEventType string

const (
	// NormalizedEventChargeSucceeded reports that one charge or order (a
	// Stripe Checkout Session's payment, an Alipay/WeChat order) was
	// collected successfully. ChannelStatus is ChannelStatusSucceeded.
	NormalizedEventChargeSucceeded NormalizedEventType = "charge_succeeded"
	// NormalizedEventChargeFailed reports that a charge or order failed or
	// expired without ever collecting. ChannelStatus is
	// ChannelStatusFailed.
	NormalizedEventChargeFailed NormalizedEventType = "charge_failed"
	// NormalizedEventSubscriptionCanceled reports that a channel-native
	// subscription (Stripe only -- Alipay/WeChat have no such object, per
	// this round's own explicit non-scope for their native recurring
	// billing) was canceled on the channel's side. ChannelStatus is
	// ChannelStatusCanceled.
	NormalizedEventSubscriptionCanceled NormalizedEventType = "subscription_canceled"
	// NormalizedEventRefunded reports that a previously succeeded charge
	// was refunded, full or partial. ChannelStatus is
	// ChannelStatusRefunded.
	NormalizedEventRefunded NormalizedEventType = "refunded"
)

// NormalizedEvent is what every provider's VerifyWebhook normalizes an
// inbound, signature-verified webhook delivery into -- the one shape
// billing's own code (and, in a later round, a caller driving Subscription/
// Invoice transitions from it) ever has to understand, per
// docs/internal/06-billing-and-metering.md's normalize-upward-into-NormalizedEvent
// rule. It never carries a provider-specific field: no stripe.Event, no
// Alipay notify param map, nothing that would leak a channel's own shape
// into billing's domain model -- see this file's own header comment for why
// that leak is the exact failure mode this design guards against.
//
// TenantID/SubscriptionID/InvoiceID are populated by decoding whatever
// caller-supplied identifiers ChargeRequest asked CreateCharge to attach to
// the channel-side object (Stripe's own Metadata map; WeChat Pay's attach
// field; Alipay's passback_params) -- never guessed or looked up by this
// package, which holds no table of its own mapping a ChannelReference back
// to a tenant. A webhook delivery whose payload cannot be decoded into a
// known event shape -- including one missing this identifying information
// entirely -- is ErrWebhookPayloadUnrecognized, never a NormalizedEvent with
// blank identifiers silently passed upstream.
type NormalizedEvent struct {
	// EventID is the channel's own event id -- together with Channel, the
	// natural key docs/internal/06-billing-and-metering.md's
	// insert-first-dedup rule dedups payment_events on (see
	// PaymentEventRepository.InsertIfNew's own doc comment).
	EventID string
	// Channel names which provider produced this event, matching
	// PaymentEvent.Channel and the registry name's own suffix (e.g.
	// "stripe" for the implementation registered as "gateway.stripe") --
	// never the registry name itself, since a caller comparing
	// NormalizedEvent.Channel across gateways should not have to know or
	// care what name each one happened to self-register under.
	Channel string
	// ChannelReference is the channel-side object this event concerns.
	ChannelReference ChannelReference

	TenantID       string
	SubscriptionID string
	InvoiceID      string

	Type   NormalizedEventType
	Status ChannelStatus
	// Amount is the event's own settled amount -- zero-valued
	// (Money{}) for an event type this does not apply to (a
	// NormalizedEventSubscriptionCanceled carries no amount). Per
	// docs/internal/06-billing-and-metering.md's callbacks-cannot-be-trusted rule, this
	// value is a signal for a caller to decide whether to re-query
	// QueryStatus, never trusted on its own to update a ledger or an
	// invoice -- see QueryStatus's own doc comment.
	Amount Money
	// OccurredAt is when the channel says this event happened, decoded from
	// the payload -- not when VerifyWebhook ran.
	OccurredAt time.Time

	// RawPayload is the exact, already signature-verified webhook body,
	// kept for PaymentEvent.RawPayload's own audit trail -- never
	// re-parsed by anything outside the provider package that produced it.
	RawPayload []byte
}

// ChargeRequest is what a caller asks a channel to create a collection for:
// one Invoice's amount, for one Subscription. See PaymentGateway.CreateCharge's
// own doc comment for what "create a collection" means per channel --
// Stripe's own recurring subscription for the international leg, a single
// Alipay/WeChat order for one internally-tracked billing cycle for the
// domestic leg.
type ChargeRequest struct {
	TenantID       string
	SubscriptionID string
	InvoiceID      string
	Amount         Money
	// Description is a short, human-readable line item shown to the payer
	// on the channel's own checkout/order page (a Stripe Checkout line
	// item's product name, an Alipay/WeChat order's subject).
	Description string
	// IdempotencyKey lets a retried CreateCharge call (a caller that timed
	// out not knowing whether its first attempt reached the channel) be
	// recognized as the same request rather than creating a second,
	// duplicate charge -- the identical idempotency discipline
	// CreditService.PreDeduct's own IdempotencyKey documents for the
	// identical reason. Every provider in this round derives its own
	// channel-native idempotency mechanism from this value (Stripe's
	// Idempotency-Key request header; Alipay/WeChat's own out_trade_no,
	// which each channel itself deduplicates on).
	IdempotencyKey string
}

// ChargeHandle is what CreateCharge returns: enough for the caller to
// direct the payer to complete the collection. Persisting the mapping from
// an Invoice or Subscription to this ChannelReference is the caller's own
// responsibility -- billing's Subscription and Invoice models stay
// channel-agnostic by design (invoice.go's and subscription.go's own doc
// comments), so no table in this round's own scope stores it; a later
// round's live webhook endpoint is where that mapping decision gets made.
type ChargeHandle struct {
	// ChannelReference is the channel's own identifier for the created
	// object -- a Stripe Checkout Session id, an Alipay/WeChat
	// out_trade_no.
	ChannelReference ChannelReference
	// RedirectURL is a checkout/redirect URL for a channel that expects the
	// payer's browser to be sent there (Stripe Checkout's hosted page,
	// Alipay's own page-based flow) -- empty for a channel with no redirect
	// step.
	RedirectURL string
	// QRCodeContent is the raw content -- ordinarily a URL -- a domestic
	// channel expects the caller to render as a QR code for the payer's
	// wallet app to scan (WeChat Pay Native's code_url, Alipay's own
	// QR-code trade flow) -- empty for a channel with no QR step.
	QRCodeContent string
}

// PaymentGateway is the seam one payment channel implementation satisfies,
// declared here in billing's own root package so business code -- and a
// later round's live webhook endpoint -- depends on this interface alone,
// never on go/billing/gateway or any provider SDK. See this file's own
// header comment for the full dependency-direction rationale.
type PaymentGateway interface {
	// CreateCharge asks the channel to create one collection. What that
	// means differs by channel, and is a deliberate, documented split
	// (docs/internal/06-billing-and-metering.md's own pragmatic-trade-off write-up), not an
	// abstraction leak:
	//
	//   - Stripe (international): creates a genuine recurring Stripe
	//     subscription -- called once per Subscription, at its first
	//     activation. Stripe's own billing engine generates every later
	//     cycle's invoice and charge on its own, announced through its own
	//     webhook events; a caller does not call CreateCharge again for
	//     later cycles of the same Subscription.
	//   - Alipay / WeChat Pay (domestic): creates one one-time order for
	//     one Invoice. Neither channel offers a periodic-charge primitive
	//     at this repository's target tier (docs/internal/06's own
	//     rationale), so a caller re-runs CreateCharge once per
	//     internally-tracked billing cycle instead -- this round
	//     deliberately does not implement Alipay/WeChat native recurring
	//     billing (docs/internal/06's own explicit non-scope).
	//
	// req.TenantID/SubscriptionID/InvoiceID are attached to the created
	// channel-side object using whichever metadata mechanism the channel
	// offers (Stripe's Metadata map; WeChat Pay's attach field; Alipay's
	// passback_params), so VerifyWebhook can decode them back out of a
	// later event concerning the same object without this package holding
	// any table of its own.
	CreateCharge(ctx context.Context, req ChargeRequest) (ChargeHandle, error)

	// VerifyWebhook authenticates one inbound webhook delivery -- the raw
	// headers and body exactly as the channel sent them -- against this
	// channel's own signature scheme, and on success normalizes it into a
	// NormalizedEvent. It performs no network call of its own: signature
	// verification and payload parsing only, which is what makes it fully
	// unit-testable offline against a known-good fixture (every provider
	// subpackage in this round does exactly that -- see stripe's own
	// gateway_test.go, alipay's notify_test.go, and wechat's notify_test.go
	// plus decrypt_test.go for the payload-decryption leg).
	//
	// A signature that does not verify is ErrWebhookSignatureInvalid. A
	// signature that verifies but whose payload this implementation cannot
	// parse into a known, identifiable event -- including one missing the
	// tenant/subscription/invoice metadata CreateCharge attached -- is
	// ErrWebhookPayloadUnrecognized. Both are refused before any
	// NormalizedEvent is returned; there is no partial or best-effort
	// result.
	VerifyWebhook(ctx context.Context, headers map[string][]string, body []byte) (NormalizedEvent, error)

	// QueryStatus re-queries the channel's own API for the authoritative,
	// current status and amount of one ChannelReference --
	// docs/internal/06-billing-and-metering.md's callbacks-cannot-be-trusted rule that a
	// webhook body's own numbers are never trusted directly, only used as
	// the signal to make this call. It is also the call the active-polling
	// fallback (job.go's taskTypePoll) makes for a ChannelReference whose
	// webhook never arrived, or arrived before the record it should update
	// existed.
	//
	// A ref this channel has no record of at all is
	// ErrChannelReferenceNotFound. Every other outcome is a ChannelStatus
	// plus the amount the channel actually holds for it -- never the
	// caller's own expected amount echoed back.
	QueryStatus(ctx context.Context, ref ChannelReference) (ChannelStatus, Money, error)
}

// PaymentGatewayRegistry is the package-level pkgcore.SeamRegistry[PaymentGateway]
// every host resolves a named channel implementation through, mirroring
// go/pki's SignerRegistry (see this file's own header comment) and, through
// it, the database/sql driver-registration pattern pkgcore's own
// EventBusRegistry/KVStoreRegistry/MailerRegistry/ObjectStoreRegistry
// already follow. go/billing/gateway/stripe, .../alipay and .../wechat each
// register themselves here from their own init() under "gateway.stripe",
// "gateway.alipay" and "gateway.wechat" -- a host that never imports a
// provider subpackage never resolves that name, and PaymentGatewayRegistry
// itself carries none of the three SDKs: go.mod's require list does name
// github.com/stripe/stripe-go/v82 directly (it is a non-indirect
// requirement, since go/billing/gateway/stripe is a subpackage of this same
// module), but never as an import of this package's own code -- only as
// what that subpackage needs, the pruned-module-graph distinction
// go/billing/gateway/AGENTS.md's Compile-time / packaging section states
// precisely.
//
// No built-in implementation is pre-registered here the way pkgcore's own
// EventBusRegistry ships "eventbus.memory" -- unlike an infrastructure seam,
// every PaymentGateway implementation genuinely needs a provider SDK or a
// hand-rolled channel client, so there is no zero-dependency default the way
// LocalSigner is for SignerRegistry (see that file's own doc comment on why
// "signer.local" registers in go/pki's own root package instead of a
// subpackage).
var PaymentGatewayRegistry = pkgcore.NewSeamRegistry[PaymentGateway]()
