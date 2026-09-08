package alipay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/vislake/speed/go/billing"
)

// productCodeFaceToFace is Alipay's product_code for the Native (QR-code)
// payment product -- the domestic-leg one-time-order shape.
const productCodeFaceToFace = "FACE_TO_FACE_PAYMENT"

const alipayTimeFormat = "2006-01-02 15:04:05"

// httpDoer is the subset of *http.Client this package calls, declared as
// its own interface so unit tests can inject a scripted double without a
// real Alipay account -- the identical stub-the-transport approach
// go/pki/signer/kmsaws's kmsClient interface uses for the AWS SDK, applied
// here at the HTTP layer since Alipay has no Go SDK to stub (doc.go's own
// SDK-choice section). *http.Client already has this exact method
// signature, so it satisfies httpDoer structurally -- no adapter type is
// needed anywhere in this package.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Gateway is this package's billing.PaymentGateway implementation over one
// Alipay open-platform application.
type Gateway struct {
	cfg    Config
	keys   resolvedKeys
	client httpDoer

	// webhookVerify carries billing.webhook.verify for this channel
	// (metrics.go in the billing root), registered by NewGateway; nil
	// for a gateway built as a bare struct literal, which the record
	// site guards.
	webhookVerify metric.Int64Counter
}

// NewGateway returns a Gateway over cfg, parsing both configured RSA keys
// immediately (a malformed key is a configuration error that should fail
// fast at construction, never surface as a mysterious signature failure on
// the first real request). Nothing is dialed: the default *http.Client
// issues no request until the first CreateCharge/QueryStatus call.
func NewGateway(cfg Config) (*Gateway, error) {
	if cfg.AppID == "" {
		return nil, fmt.Errorf("billing/gateway/alipay: NewGateway requires a non-empty Config.AppID")
	}
	if cfg.NotifyURL == "" {
		return nil, fmt.Errorf("billing/gateway/alipay: NewGateway requires a non-empty Config.NotifyURL")
	}
	keys, err := cfg.parseKeys()
	if err != nil {
		return nil, err
	}
	return &Gateway{
		cfg:           cfg,
		keys:          keys,
		client:        http.DefaultClient,
		webhookVerify: billing.RegisterWebhookVerifyMetric("alipay"),
	}, nil
}

// newGatewayWithClient is NewGateway's test-only twin, injecting a scripted
// httpDoer in place of http.DefaultClient.
func newGatewayWithClient(client httpDoer, cfg Config) (*Gateway, error) {
	keys, err := cfg.parseKeys()
	if err != nil {
		return nil, err
	}
	return &Gateway{cfg: cfg, keys: keys, client: client}, nil
}

// CreateCharge implements billing.PaymentGateway. For Alipay, it creates
// one Native (QR-code) trade order for req's amount via
// alipay.trade.precreate -- see billing.PaymentGateway.CreateCharge's own
// doc comment for why this is a single one-time order, called once per
// internally-tracked billing cycle, never a native recurring subscription
// (Alipay has none at this repository's target tier).
//
// req.TenantID/SubscriptionID/InvoiceID are JSON-encoded into the
// passback_params common request parameter, which Alipay echoes back
// verbatim on the matching notification -- normalizeNotify decodes it back
// out.
func (g *Gateway) CreateCharge(ctx context.Context, req billing.ChargeRequest) (billing.ChargeHandle, error) {
	if err := requireCNY(req.Amount.Currency); err != nil {
		return billing.ChargeHandle{}, err
	}
	if req.Amount.Cents <= 0 {
		// Alipay's total_amount must be a strictly positive decimal yuan
		// amount; a zero or negative request is refused at the boundary
		// before formatAmount ever renders it: -2950 cents would otherwise
		// reach Alipay as the garbage string "-29.-50" and 0 as "0.00",
		// both of which Alipay's own gateway rejects only after the
		// merchant has already signed and sent them.
		return billing.ChargeHandle{}, billing.ErrInvalidAmount.WithParam("amount", req.Amount.Cents)
	}

	outTradeNo := outTradeNoFor(req)
	passback, err := encodePassback(req)
	if err != nil {
		return billing.ChargeHandle{}, err
	}

	bizContent, err := json.Marshal(map[string]string{
		"out_trade_no": outTradeNo,
		"total_amount": formatAmount(req.Amount.Cents),
		"subject":      chargeSubject(req),
		"product_code": productCodeFaceToFace,
	})
	if err != nil {
		return billing.ChargeHandle{}, fmt.Errorf("billing/gateway/alipay: encode biz_content: %w", err)
	}

	var resp struct {
		Response struct {
			Code    string `json:"code"`
			Msg     string `json:"msg"`
			SubCode string `json:"sub_code"`
			SubMsg  string `json:"sub_msg"`
			QRCode  string `json:"qr_code"`
		} `json:"alipay_trade_precreate_response"`
		Sign string `json:"sign"`
	}
	if err := g.call(ctx, "alipay.trade.precreate", string(bizContent), passback, "alipay_trade_precreate_response", &resp); err != nil {
		return billing.ChargeHandle{}, err
	}
	if resp.Response.Code != alipayResponseCodeSuccess {
		return billing.ChargeHandle{}, fmt.Errorf(
			"billing/gateway/alipay: precreate failed: %s %s (%s %s)",
			resp.Response.Code, resp.Response.Msg, resp.Response.SubCode, resp.Response.SubMsg,
		)
	}

	return billing.ChargeHandle{
		ChannelReference: billing.ChannelReference(outTradeNo),
		QRCodeContent:    resp.Response.QRCode,
	}, nil
}

// alipayResponseCodeSuccess is the "code" value Alipay's response envelope
// carries for a successful call, per its own documented response
// convention -- shared by every alipay.trade.* operation's response
// envelope.
const alipayResponseCodeSuccess = "10000"

// chargeSubject falls back to a generic label when req.Description is
// empty -- Alipay's own subject field is required on every trade-creating
// call.
func chargeSubject(req billing.ChargeRequest) string {
	if req.Description != "" {
		return req.Description
	}
	return "Subscription"
}

// outTradeNoFor derives the merchant order number CreateCharge creates the
// trade under, and QueryStatus/notifications reference it by
// (ChannelReference for this provider IS the out_trade_no -- Alipay has no
// separate channel-generated order id at creation time; trade_no, its own
// internal id, only exists once the trade itself exists). Prefers
// req.IdempotencyKey (so a retried CreateCharge reaches the SAME Alipay
// order rather than creating a duplicate -- Alipay's own out_trade_no
// uniqueness is exactly the channel-native idempotency mechanism
// ChargeRequest.IdempotencyKey's own doc comment describes), falling back
// to req.InvoiceID when no idempotency key was given.
func outTradeNoFor(req billing.ChargeRequest) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return req.InvoiceID
}

// formatAmount renders cents as Alipay's own decimal yuan string
// ("total_amount"), e.g. 2900 -> "29.00". Alipay only ever settles in CNY
// for this product, so no currency conversion is performed here -- CreateCharge's
// own requireCNY call already refused any non-CNY req.Amount before this is
// reached.
//
// The CreateCharge boundary refuses every non-positive amount before this
// is ever reached (see that method's own check), so formatAmount's inputs
// here are always positive. A negative input -- reachable only by a
// hypothetical future direct call, never by this package's own flow -- is
// rendered SIGN-CORRECTLY ("-29.50") rather than as the garbage "-29.-50"
// (a "%02d" of the negative remainder -50 would fabricate a literal "-50"
// after the decimal point) or with the sign dropped; no code path in this
// package can ever send such a string to Alipay.
func formatAmount(cents int64) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	s := strconv.FormatInt(cents/100, 10) + "." + fmt.Sprintf("%02d", cents%100)
	if negative {
		return "-" + s
	}
	return s
}

// requireCNY refuses currency at the CreateCharge boundary unless it names
// CNY (case-insensitively) -- Alipay's Native (QR-code) product only ever
// settles in CNY (the domestic-leg trade-off), so a request naming any
// other currency must be refused here rather than silently collected as
// CNY --
// formatAmount's own cents-to-yuan conversion has no unit conversion of its
// own, so a caller-supplied USD/EUR/etc amount would otherwise be sent to
// Alipay, and collected from the payer, as if it were the same number of
// CNY cents.
func requireCNY(currency string) error {
	if !strings.EqualFold(currency, "CNY") {
		return billing.ErrUnsupportedCurrency.
			WithParam("currency", currency).
			WithParam("channel", "alipay").
			WithParam("supported_currency", "CNY")
	}
	return nil
}

// passbackPayload is what CreateCharge JSON-encodes into passback_params
// and normalizeNotify decodes back out.
type passbackPayload struct {
	TenantID       string `json:"t"`
	SubscriptionID string `json:"s"`
	InvoiceID      string `json:"i"`
}

func encodePassback(req billing.ChargeRequest) (string, error) {
	b, err := json.Marshal(passbackPayload{TenantID: req.TenantID, SubscriptionID: req.SubscriptionID, InvoiceID: req.InvoiceID})
	if err != nil {
		return "", fmt.Errorf("billing/gateway/alipay: encode passback_params: %w", err)
	}
	return string(b), nil
}

// QueryStatus implements billing.PaymentGateway: calls alipay.trade.query
// for ref (the out_trade_no CreateCharge created the order under) and maps
// its trade_status to a billing.ChannelStatus -- the authoritative re-query
// the never-trust-the-callback-body rule requires. The response envelope's
// own signature is verified against the configured Alipay public key
// before anything in it is trusted, exactly like an inbound notification.
func (g *Gateway) QueryStatus(ctx context.Context, ref billing.ChannelReference) (billing.ChannelStatus, billing.Money, error) {
	bizContent, err := json.Marshal(map[string]string{"out_trade_no": string(ref)})
	if err != nil {
		return "", billing.Money{}, fmt.Errorf("billing/gateway/alipay: encode biz_content: %w", err)
	}

	var resp struct {
		Response struct {
			Code        string `json:"code"`
			Msg         string `json:"msg"`
			SubCode     string `json:"sub_code"`
			SubMsg      string `json:"sub_msg"`
			TradeStatus string `json:"trade_status"`
			TotalAmount string `json:"total_amount"`
			// RefundFee is the response's own refund marker -- the refunded
			// amount (a decimal yuan string), per the alipay.trade.query response
			// documentation: present on a paid trade that has been refunded
			// and absent (or "0.00") on a trade nothing refunded.
			RefundFee string `json:"refund_fee"`
		} `json:"alipay_trade_query_response"`
		Sign string `json:"sign"`
	}
	if err = g.call(ctx, "alipay.trade.query", string(bizContent), "", "alipay_trade_query_response", &resp); err != nil {
		return "", billing.Money{}, err
	}
	if resp.Response.Code != alipayResponseCodeSuccess {
		if resp.Response.SubCode == "ACQ.TRADE_NOT_EXIST" {
			return "", billing.Money{}, billing.ErrChannelReferenceNotFound.WithParam("channel_reference", string(ref))
		}
		return "", billing.Money{}, fmt.Errorf(
			"billing/gateway/alipay: query failed: %s %s (%s %s)",
			resp.Response.Code, resp.Response.Msg, resp.Response.SubCode, resp.Response.SubMsg,
		)
	}

	amount, err := parseAmount(resp.Response.TotalAmount)
	if err != nil {
		return "", billing.Money{}, err
	}
	// Alipay's alipay.trade.query response carries no currency field of its
	// own (total_amount is a bare decimal yuan string) -- reporting "CNY"
	// here is honest, not a fabrication, precisely BECAUSE CreateCharge's own
	// requireCNY call refuses every non-CNY ChargeRequest before an order can
	// ever be created: every ref this method can be asked about genuinely is
	// a CNY order, by construction, never an assumption papering over a
	// silently-accepted foreign currency.
	return tradeStatusToChannelStatus(resp.Response.TradeStatus, resp.Response.RefundFee), billing.Money{Cents: amount, Currency: "CNY"}, nil
}

// parseAmount parses Alipay's decimal yuan string ("29.00") back into
// integer cents. A negative string is refused outright -- amounts Alipay
// actually sends (a query's total_amount, a notify's total_amount and
// refund_fee) are never negative, so a leading minus is a protocol anomaly
// this parser refuses rather than guessing at: parsing "-0.50" as 50 would
// silently drop the sign, turning a negative value into a small positive
// one, and parsing "-29.50" as -2850 would let the sign's arithmetic
// corrupt the value.
func parseAmount(s string) (int64, error) {
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("billing/gateway/alipay: parse amount %q: negative amount", s)
	}
	parts := strings.SplitN(s, ".", 2)
	yuan, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("billing/gateway/alipay: parse amount %q: %w", s, err)
	}
	var cents int64
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) > 2 {
			frac = frac[:2]
		}
		for len(frac) < 2 {
			frac += "0"
		}
		c, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("billing/gateway/alipay: parse amount %q: %w", s, err)
		}
		cents = c
	}
	return yuan*100 + cents, nil
}

// carriesRefund reports whether a decimal-yuan refund-fee string marks a
// genuine refund: absent or "0.00" means no refund happened, and a fee
// that does not parse as a positive amount is treated as no refund rather
// than guessed at (the refunds this predicate cares about are always
// positive sums). This is notifyCarriesRefund's refund_fee leg, shared with
// the poll path so the two payload shapes' refund detection can never
// drift apart.
func carriesRefund(refundFee string) bool {
	if refundFee == "" {
		return false
	}
	cents, err := parseAmount(refundFee)
	return err == nil && cents > 0
}

// tradeStatusToChannelStatus maps Alipay's own trade_status vocabulary
// (https://opendocs.alipay.com/open/194/103296) onto billing.ChannelStatus.
// refundFee is the payload's own "refund_fee" parameter (the refunded
// amount, a decimal yuan string) -- an empty string when the payload
// carries no such field. TRADE_CLOSED covers two distinct fates, exactly
// as the notify path's normalizeNotify already distinguishes: an
// unpaid trade closed by timeout, and a PAID trade closed by a FULL refund
// -- the alipay.trade.query response's own trade_status definition says so
// (closed by timeout unpaid, or fully refunded after payment), and the refund marker
// (refund_fee) is what tells them apart, the identical detection the
// notify payload uses. A closed-with-refund poll answer is a refund of the
// earlier succeeded charge -- ChannelStatusRefunded, never the
// ChannelStatusFailed a timed-out unpaid order gets. A PARTIAL refund
// leaves the trade at TRADE_SUCCESS (only a full refund moves the order
// off it), so a TRADE_SUCCESS answer stays Succeeded regardless of any
// refund_fee the response carries.
func tradeStatusToChannelStatus(status, refundFee string) billing.ChannelStatus {
	switch status {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		return billing.ChannelStatusSucceeded
	case "TRADE_CLOSED":
		if carriesRefund(refundFee) {
			return billing.ChannelStatusRefunded
		}
		return billing.ChannelStatusFailed
	default: // "WAIT_BUYER_PAY"
		return billing.ChannelStatusPending
	}
}

// call signs a common-params + biz_content Alipay open-platform request,
// posts it as application/x-www-form-urlencoded, verifies the response
// envelope's own signature against the configured Alipay public key, and
// JSON-decodes the whole envelope into out.
//
// The request's "timestamp" common parameter is rendered in ALIPAY'S OWN
// TIME ZONE, never the host's: Alipay's gateway interprets the parameter
// (and its gmt_* fields generally, despite the misleading prefix -- see
// response.go's own alipayLocation note) as China Standard Time, UTC+8.
// Formatting time.Now() in the host's local zone would make a host outside
// UTC+8 sign and send a timestamp whose wall clock is off by the host's
// whole UTC offset -- a skew Alipay's gateway refuses.
func (g *Gateway) call(ctx context.Context, method, bizContent, passback, responseField string, out any) error {
	params := map[string]string{
		"app_id":      g.cfg.AppID,
		"method":      method,
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   time.Now().In(alipayLocation()).Format(alipayTimeFormat),
		"version":     "1.0",
		"biz_content": bizContent,
		"notify_url":  g.cfg.NotifyURL,
	}
	if passback != "" {
		params["passback_params"] = passback
	}
	sig, err := signParams(params, g.keys.priv)
	if err != nil {
		return err
	}
	params["sign"] = sig

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.gatewayURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("billing/gateway/alipay: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("billing/gateway/alipay: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("billing/gateway/alipay: %s: read response: %w", method, err)
	}

	if err := verifyResponseEnvelope(body, responseField, g.keys.pub); err != nil {
		return fmt.Errorf("billing/gateway/alipay: %s: %w", method, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("billing/gateway/alipay: %s: decode response: %w", method, err)
	}
	return nil
}

// compile-time check that *Gateway satisfies billing.PaymentGateway.
var _ billing.PaymentGateway = (*Gateway)(nil)
