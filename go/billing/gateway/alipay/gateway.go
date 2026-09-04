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

	"github.com/vislake/speed/go/billing"
)

// productCodeFaceToFace is Alipay's product_code for the Native (QR-code)
// payment product -- the domestic-leg one-time-order shape
// docs/internal/06-billing-and-metering.md names.
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
	return &Gateway{cfg: cfg, keys: keys, client: http.DefaultClient}, nil
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
// for this product, so no currency conversion is performed or checked here
// -- a caller passing a non-CNY Money is a caller error this package does
// not detect (see go/billing/gateway/AGENTS.md's Known limitations).
func formatAmount(cents int64) string {
	return strconv.FormatInt(cents/100, 10) + "." + fmt.Sprintf("%02d", cents%100)
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
// docs/internal/06-billing-and-metering.md's callbacks-cannot-be-trusted
// rule requires. The response envelope's own signature is verified against
// the configured Alipay public key before anything in it is trusted,
// exactly like an inbound notification.
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
	return tradeStatusToChannelStatus(resp.Response.TradeStatus), billing.Money{Cents: amount, Currency: "CNY"}, nil
}

// parseAmount parses Alipay's decimal yuan string ("29.00") back into
// integer cents.
func parseAmount(s string) (int64, error) {
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

// tradeStatusToChannelStatus maps Alipay's own trade_status vocabulary
// (https://opendocs.alipay.com/open/194/103296) onto billing.ChannelStatus.
func tradeStatusToChannelStatus(status string) billing.ChannelStatus {
	switch status {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		return billing.ChannelStatusSucceeded
	case "TRADE_CLOSED":
		return billing.ChannelStatusFailed
	default: // "WAIT_BUYER_PAY"
		return billing.ChannelStatusPending
	}
}

// call signs a common-params + biz_content Alipay open-platform request,
// posts it as application/x-www-form-urlencoded, verifies the response
// envelope's own signature against the configured Alipay public key, and
// JSON-decodes the whole envelope into out.
func (g *Gateway) call(ctx context.Context, method, bizContent, passback, responseField string, out any) error {
	params := map[string]string{
		"app_id":      g.cfg.AppID,
		"method":      method,
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   time.Now().Format(alipayTimeFormat),
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
