package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/vislake/speed/go/billing"
)

const nativePayPath = "/v3/pay/transactions/native"

// httpDoer is the subset of *http.Client this package calls, declared as
// its own interface so unit tests can inject a scripted double without a
// real WeChat Pay account -- the identical stub-the-transport approach
// go/billing/gateway/alipay's own httpDoer uses, since neither provider has
// a Go SDK this package adopts (doc.go's own SDK-choice section).
// *http.Client already has this exact method signature, so it satisfies
// httpDoer structurally.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Gateway is this package's billing.PaymentGateway implementation over one
// WeChat Pay merchant account.
type Gateway struct {
	cfg    Config
	keys   resolvedKeys
	client httpDoer
}

// NewGateway returns a Gateway over cfg, parsing both configured RSA keys
// immediately -- a malformed key is a configuration error that should fail
// fast at construction, never surface as a mysterious signature failure on
// the first real request. Nothing is dialed: the default *http.Client
// issues no request until the first CreateCharge/QueryStatus call.
func NewGateway(cfg Config) (*Gateway, error) {
	if cfg.MchID == "" || cfg.AppID == "" || cfg.MchCertSerialNo == "" {
		return nil, fmt.Errorf("billing/gateway/wechat: NewGateway requires non-empty Config.MchID, Config.AppID and Config.MchCertSerialNo")
	}
	if cfg.NotifyURL == "" {
		return nil, fmt.Errorf("billing/gateway/wechat: NewGateway requires a non-empty Config.NotifyURL")
	}
	if len(cfg.APIv3Key) != 32 {
		return nil, fmt.Errorf("billing/gateway/wechat: NewGateway requires a 32-byte Config.APIv3Key, got %d bytes", len(cfg.APIv3Key))
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

// attachPayload is what CreateCharge JSON-encodes into the Native order's
// "attach" field and normalizeNotify decodes back out -- the identical
// round-trip shape go/billing/gateway/alipay's own passbackPayload uses for
// passback_params.
type attachPayload struct {
	TenantID       string `json:"t"`
	SubscriptionID string `json:"s"`
	InvoiceID      string `json:"i"`
}

// CreateCharge implements billing.PaymentGateway. For WeChat Pay, it
// creates one Native (QR-code) trade order for req's amount via
// `POST /v3/pay/transactions/native` -- see
// billing.PaymentGateway.CreateCharge's own doc comment for why this is a
// single one-time order, called once per internally-tracked billing cycle,
// never a native recurring subscription (WeChat Pay has none at this
// repository's target tier).
func (g *Gateway) CreateCharge(ctx context.Context, req billing.ChargeRequest) (billing.ChargeHandle, error) {
	outTradeNo := outTradeNoFor(req)
	attach, err := json.Marshal(attachPayload{TenantID: req.TenantID, SubscriptionID: req.SubscriptionID, InvoiceID: req.InvoiceID})
	if err != nil {
		return billing.ChargeHandle{}, fmt.Errorf("billing/gateway/wechat: encode attach: %w", err)
	}

	body, err := json.Marshal(map[string]any{
		"appid":        g.cfg.AppID,
		"mchid":        g.cfg.MchID,
		"description":  chargeDescription(req),
		"out_trade_no": outTradeNo,
		"notify_url":   g.cfg.NotifyURL,
		"attach":       string(attach),
		"amount": map[string]any{
			"total":    req.Amount.Cents,
			"currency": "CNY",
		},
	})
	if err != nil {
		return billing.ChargeHandle{}, fmt.Errorf("billing/gateway/wechat: encode request body: %w", err)
	}

	var resp struct {
		CodeURL string `json:"code_url"`
	}
	if err := g.call(ctx, http.MethodPost, nativePayPath, body, &resp); err != nil {
		return billing.ChargeHandle{}, err
	}

	return billing.ChargeHandle{
		ChannelReference: billing.ChannelReference(outTradeNo),
		QRCodeContent:    resp.CodeURL,
	}, nil
}

// chargeDescription falls back to a generic label when req.Description is
// empty -- WeChat Pay's own "description" field is required on every
// order-creating call.
func chargeDescription(req billing.ChargeRequest) string {
	if req.Description != "" {
		return req.Description
	}
	return "Subscription"
}

// outTradeNoFor mirrors go/billing/gateway/alipay's identical helper:
// prefers req.IdempotencyKey (so a retried CreateCharge reaches the SAME
// WeChat Pay order rather than creating a duplicate -- WeChat Pay's own
// out_trade_no uniqueness is exactly the channel-native idempotency
// mechanism ChargeRequest.IdempotencyKey's own doc comment describes),
// falling back to req.InvoiceID.
func outTradeNoFor(req billing.ChargeRequest) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return req.InvoiceID
}

// QueryStatus implements billing.PaymentGateway: calls
// `GET /v3/pay/transactions/out-trade-no/{out_trade_no}` for ref and maps
// its trade_state to a billing.ChannelStatus -- the authoritative re-query
// docs/internal/06-billing-and-metering.md's callbacks-cannot-be-trusted
// rule requires. The response's own Wechatpay-Signature header is verified
// against the configured platform public key before anything in the body
// is trusted, exactly like an inbound notification.
func (g *Gateway) QueryStatus(ctx context.Context, ref billing.ChannelReference) (billing.ChannelStatus, billing.Money, error) {
	path := fmt.Sprintf("/v3/pay/transactions/out-trade-no/%s?mchid=%s", string(ref), g.cfg.MchID)

	var resp struct {
		TradeState string `json:"trade_state"`
		Amount     struct {
			Total    int64  `json:"total"`
			Currency string `json:"currency"`
		} `json:"amount"`
	}
	err := g.call(ctx, http.MethodGet, path, nil, &resp)
	if err != nil {
		if isNotFound(err) {
			return "", billing.Money{}, billing.ErrChannelReferenceNotFound.WithParam("channel_reference", string(ref))
		}
		return "", billing.Money{}, err
	}

	return tradeStateToChannelStatus(resp.TradeState), billing.Money{Cents: resp.Amount.Total, Currency: resp.Amount.Currency}, nil
}

// tradeStateToChannelStatus maps WeChat Pay's own trade_state vocabulary
// (https://pay.weixin.qq.com/doc/v3/merchant/4012791863) onto
// billing.ChannelStatus.
func tradeStateToChannelStatus(state string) billing.ChannelStatus {
	switch state {
	case "SUCCESS":
		return billing.ChannelStatusSucceeded
	case "REFUND":
		return billing.ChannelStatusRefunded
	case "CLOSED", "REVOKED", "PAYERROR":
		return billing.ChannelStatusFailed
	default: // "NOTPAY", "USERPAYING"
		return billing.ChannelStatusPending
	}
}

// wechatAPIError is the shape of a WeChat Pay APIv3 error response body --
// {"code": "...", "message": "..."} -- distinct from the success shape
// every operation otherwise returns.
type wechatAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *wechatAPIError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func isNotFound(err error) bool {
	var apiErr *wechatAPIError
	return errors.As(err, &apiErr) && apiErr.Code == "ORDER_NOT_EXIST"
}

// call signs and sends one WeChat Pay APIv3 request, verifies the
// response's own Wechatpay-Signature against the configured platform
// public key, and JSON-decodes the response body into out (skipped for a
// nil out, e.g. a call whose only purpose is the side effect).
func (g *Gateway) call(ctx context.Context, method, pathAndQuery string, body []byte, out any) error {
	authz, err := authorizationHeader(g.cfg, g.keys.priv, method, pathAndQuery, body)
	if err != nil {
		return err
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.cfg.gatewayURL()+pathAndQuery, bodyReader)
	if err != nil {
		return fmt.Errorf("billing/gateway/wechat: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", authz)

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("billing/gateway/wechat: %s %s: %w", method, pathAndQuery, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("billing/gateway/wechat: %s %s: read response: %w", method, pathAndQuery, err)
	}

	if err := g.verifyResponseSignature(resp.Header, respBody); err != nil {
		return fmt.Errorf("billing/gateway/wechat: %s %s: %w", method, pathAndQuery, err)
	}

	if resp.StatusCode >= 400 {
		var apiErr wechatAPIError
		if err := json.Unmarshal(respBody, &apiErr); err != nil {
			return fmt.Errorf("billing/gateway/wechat: %s %s: status %d, undecodable body", method, pathAndQuery, resp.StatusCode)
		}
		return fmt.Errorf("billing/gateway/wechat: %s %s: %w", method, pathAndQuery, &apiErr)
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("billing/gateway/wechat: %s %s: decode response: %w", method, pathAndQuery, err)
	}
	return nil
}

// verifyResponseSignature verifies one API response's Wechatpay-Signature
// header against its exact body bytes -- the same
// timestamp+"\n"+nonce+"\n"+body+"\n" message shape VerifySignature checks
// for an inbound notification (notifySignMessage), since WeChat Pay signs
// both an API response and a webhook notification identically. A missing
// signature header is refused, never silently accepted as "unsigned but
// presumably fine".
func (g *Gateway) verifyResponseSignature(header http.Header, body []byte) error {
	sig := header.Get("Wechatpay-Signature")
	timestamp := header.Get("Wechatpay-Timestamp")
	nonce := header.Get("Wechatpay-Nonce")
	if sig == "" || timestamp == "" || nonce == "" {
		return fmt.Errorf("response missing Wechatpay-Signature/Wechatpay-Timestamp/Wechatpay-Nonce headers")
	}
	return VerifySignature(timestamp, nonce, body, sig, g.keys.platform)
}

// compile-time check that *Gateway satisfies billing.PaymentGateway.
var _ billing.PaymentGateway = (*Gateway)(nil)
