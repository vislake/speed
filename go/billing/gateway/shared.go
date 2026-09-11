package gateway

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/vislake/speed/go/billing"
)

// This file holds the helpers every provider package shares: the two
// domestic providers derive the same merchant order number, guard the same
// CNY-only settlement, read the same first-header shape, resolve the same
// gateway-URL override and parse the same PEM key material, and all three
// want the same non-empty description fallback. Each helper's behavior is
// the behavior its previous per-package copies had, character for
// character, so nothing observable about a provider's requests changes by
// living here.

// ChargeDescription answers the description every channel's create-charge
// call sends, falling back to a generic label when req.Description is
// empty: Alipay's own subject field is required on every trade-creating
// call, Stripe's product_data.name is required whenever price_data is
// used, and WeChat Pay's description is required too, so an empty string
// must never reach any of them.
func ChargeDescription(req billing.ChargeRequest) string {
	if req.Description != "" {
		return req.Description
	}
	return "Subscription"
}

// OutTradeNo derives the merchant order number CreateCharge creates the
// domestic channels' order under, and QueryStatus/notifications reference
// it by (ChannelReference for those providers IS the out_trade_no --
// neither has a separate channel-generated order id at creation time).
// Prefers req.IdempotencyKey, so a retried CreateCharge reaches the SAME
// channel-side order rather than creating a duplicate -- the channel's own
// out_trade_no uniqueness is exactly the channel-native idempotency
// mechanism ChargeRequest.IdempotencyKey's own doc comment describes --
// falling back to req.InvoiceID when no idempotency key was given.
func OutTradeNo(req billing.ChargeRequest) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return req.InvoiceID
}

// RequireCNY refuses currency at the CreateCharge boundary unless it names
// CNY (case-insensitively) -- both domestic providers' Native (QR-code)
// product only ever settles in CNY (the domestic-leg trade-off), so a
// request naming any other currency must be refused here rather than
// silently collected as CNY. Without the check, a caller-supplied
// USD/EUR/etc amount would ride into the request body's hardcoded CNY
// currency and be sent to the channel, and collected from the payer, as if
// it were the same number of CNY cents. channel is the provider name
// carried in the refused error's params.
func RequireCNY(channel, currency string) error {
	if !strings.EqualFold(currency, "CNY") {
		return billing.ErrUnsupportedCurrency.
			WithParam("currency", currency).
			WithParam("channel", channel).
			WithParam("supported_currency", "CNY")
	}
	return nil
}

// FirstHeader returns the first value of the named header, case-sensitively
// -- callers of VerifyWebhook are expected to pass headers exactly as
// received (net/http.Header's own canonical casing), matching the
// providers' own documented examples.
func FirstHeader(headers map[string][]string, name string) string {
	for _, v := range headers[name] {
		return v
	}
	return ""
}

// ParsePrivateKeyPEM parses a PEM-encoded RSA private key in PKCS#1 or
// PKCS#8 form -- either is common among the domestic platforms'
// key-generation tools and merchant-key exports, so both are accepted.
func ParsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("billing/gateway: parse private key: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway: parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("billing/gateway: parse private key: got %T, want *rsa.PrivateKey", key)
	}
	return rsaKey, nil
}

// ParsePublicKeyPEM parses a PEM-encoded RSA public key in
// PKIX/SubjectPublicKeyInfo form, or extracts the public key from a
// PEM-encoded X.509 certificate -- the form a platform certificate
// download endpoint returns, so both forms are accepted for the caller's
// convenience.
func ParsePublicKeyPEM(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("billing/gateway: parse public key: no PEM block found")
	}

	if block.Type == "CERTIFICATE" {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("billing/gateway: parse certificate: %w", err)
		}
		rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("billing/gateway: parse certificate: public key is %T, want *rsa.PublicKey", cert.PublicKey)
		}
		return rsaPub, nil
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway: parse public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("billing/gateway: parse public key: got %T, want *rsa.PublicKey", pub)
	}
	return rsaPub, nil
}

// GatewayURL resolves a provider Config's GatewayURL override: a non-empty
// override wins (a sandbox or test endpoint), the provider's own
// production default applies otherwise.
func GatewayURL(override, defaultURL string) string {
	if override != "" {
		return override
	}
	return defaultURL
}
