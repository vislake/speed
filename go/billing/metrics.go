package billing

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// InstrumentationName identifies this package's own meter, mirroring
// go/jobs's (whose root-package constant its queue/asynq subpackage
// reuses) and go/metering/metrics.go's: the gateway provider
// subpackages register their webhook instrument under this same name.
const InstrumentationName = "github.com/vislake/speed/go/billing"

// Metric instrument names registerBillingMetrics wires under
// InstrumentationName -- the "payment" row of
// docs/internal/09-observability.md's must-instrument table: callback
// handling success rate and intermediate-state order count and dwell.
// The row is covered as follows, all low-cardinality labels, never
// tenant_id:
//
//   - billing.invoice.transition -- one count per applied invoice
//     status transition, labeled from and to. The intermediate-state
//     ("open") invoice count is derivable as the running delta of
//     transitions INTO open minus transitions OUT of it -- invoices
//     are the ledger's orders, and their transitions are the only
//     status mutations the row can hang an honest count on. The
//     module's Subscription lifecycle has the identical state machine
//     (SubscriptionService.transition) and is deliberately not
//     double-instrumented: the row names orders, invoices
//     are its ledger face, and subscriptions carry their own
//     intermediate-state vocabulary this row does not distinguish.
//   - billing.invoice.open_dwell -- how long an invoice stayed open
//     before leaving the intermediate state, recorded at the
//     transition out (open -> paid or open -> void) as the time since
//     the invoice's own creation. The row's dwell half:
//     an invoice stuck open is money movement stuck mid-flight.
//   - billing.webhook.verify (in the gateway provider subpackages,
//     same meter, channel-labeled) -- one count per inbound webhook
//     verification, outcome-labeled succeeded or failed. The row's
//     callback-handling-success-rate half: a channel's failure-rate alert is
//     verify{channel=X, outcome=failed} over the same channel's
//     succeeded count. Providers register and record it themselves
//     (gateway/stripe, gateway/alipay, gateway/wechat -- each
//     subpackage owns its VerifyWebhook implementation); the finer
//     classification (signature-invalid vs payload-unrecognized) is a
//     diagnostic refinement deliberately deferred, the outcome binary
//     being the alert the 09-table's first-alert list actually names.
const (
	invoiceTransitionMetricName = "billing.invoice.transition"
	invoiceOpenDwellMetricName  = "billing.invoice.open_dwell"

	fromAttr    = "from"
	toAttr      = "to"
	channelAttr = "channel"

	outcomeAttr      = "outcome"
	outcomeFailed    = "failed"
	outcomeSucceeded = "succeeded"
)

// WebhookVerifyMetricName is the metric name the gateway provider
// subpackages (gateway/stripe, gateway/alipay, gateway/wechat) record
// their inbound-webhook verification outcomes onto, each under its own
// channel attribute value -- the (callback handling
// success rate) half of the 09-table payment row.
const WebhookVerifyMetricName = "billing.webhook.verify"

// RegisterWebhookVerifyMetric wires the billing.webhook.verify counter
// a provider records onto. Exported because the gateway provider
// subpackages own their VerifyWebhook implementations and register
// their own instrument at construction; every provider registers under
// the same meter (InstrumentationName) and metric name -- the same
// name/options triple makes OTel return the one shared counter -- and
// each provider's recording call labels its points with its own
// channel value, so the shared counter still carries three
// distinguishable series. channel is validated here only by
// convention; it is recorded per point, never stored.
func RegisterWebhookVerifyMetric(channel string) metric.Int64Counter {
	meter := otel.Meter(InstrumentationName)
	counter, _ := meter.Int64Counter(
		WebhookVerifyMetricName,
		metric.WithDescription("Inbound payment-webhook verifications, labeled by channel and by outcome (succeeded or failed); a channel's callback-failure rate is its failed count over the same channel's succeeded count."),
		metric.WithUnit("{verification}"),
	)
	return counter
}

// RecordWebhookVerify counts one webhook verification onto a counter
// RegisterWebhookVerifyMetric returned, under the caller's channel and
// an outcome classified from err's nilness alone: every VerifyWebhook
// implementation refuses with a non-nil error on any failure (signature
// or payload), so the succeeded/failed binary is derived from the
// return, never threaded through every branch of three provider
// implementations. The counter may be nil (a gateway built without the
// registration, tests constructing bare structs), which the record
// sites guard.
func RecordWebhookVerify(
	ctx context.Context, counter metric.Int64Counter, channel string, err error,
) {
	if counter == nil {
		return
	}
	outcome := outcomeSucceeded
	if err != nil {
		outcome = outcomeFailed
	}
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(channelAttr, channel),
		attribute.String(outcomeAttr, outcome),
	))
}

// registerBillingMetrics wires the invoice instruments, owned by the
// InvoiceRepository (setStatus is the only transition site).
func registerBillingMetrics() (metric.Int64Counter, metric.Float64Histogram) {
	meter := otel.Meter(InstrumentationName)
	transition, _ := meter.Int64Counter(
		invoiceTransitionMetricName,
		metric.WithDescription("Applied invoice status transitions, labeled from and to; the intermediate-state (open) count is the running delta of transitions in minus transitions out."),
		metric.WithUnit("{transition}"),
	)
	openDwell, _ := meter.Float64Histogram(
		invoiceOpenDwellMetricName,
		metric.WithDescription("How long an invoice stayed open before leaving the intermediate state, in seconds, recorded at the transition out."),
		metric.WithUnit("s"),
	)
	return transition, openDwell
}

// recordInvoiceTransition counts one applied transition and, when it
// leaves the open intermediate state, its dwell. The instruments are
// nil-guarded for an InvoiceRepository built as a bare struct literal.
func recordInvoiceTransition(
	ctx context.Context,
	transition metric.Int64Counter,
	openDwell metric.Float64Histogram,
	from, to InvoiceStatus,
	openedAt time.Time,
) {
	if transition != nil {
		transition.Add(ctx, 1, metric.WithAttributes(
			attribute.String(fromAttr, string(from)),
			attribute.String(toAttr, string(to)),
		))
	}
	if openDwell != nil && from == InvoiceStatusOpen && to != InvoiceStatusOpen {
		openDwell.Record(ctx, time.Since(openedAt).Seconds(),
			metric.WithAttributes(attribute.String(toAttr, string(to))))
	}
}
