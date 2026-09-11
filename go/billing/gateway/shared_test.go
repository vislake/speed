package gateway

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

func TestChargeDescription_FallsBackOnlyForAnEmptyDescription(t *testing.T) {
	if got := ChargeDescription(billing.ChargeRequest{}); got != "Subscription" {
		t.Errorf("ChargeDescription(empty) = %q, want %q", got, "Subscription")
	}
	if got := ChargeDescription(billing.ChargeRequest{Description: "Pro plan"}); got != "Pro plan" {
		t.Errorf("ChargeDescription(set) = %q, want the caller's own description verbatim", got)
	}
}

// TestOutTradeNo_PrefersTheIdempotencyKey pins the derivation every
// domestic order number depends on (the request bodies' out_trade_no, the
// handle's ChannelReference and every later query all read it): a set
// idempotency key wins so a retried CreateCharge reaches the same
// channel-side order, the invoice id is the fallback otherwise.
func TestOutTradeNo_PrefersTheIdempotencyKey(t *testing.T) {
	if got := OutTradeNo(billing.ChargeRequest{IdempotencyKey: "idem-1", InvoiceID: "inv-1"}); got != "idem-1" {
		t.Errorf("OutTradeNo(with key) = %q, want idem-1", got)
	}
	if got := OutTradeNo(billing.ChargeRequest{InvoiceID: "inv-1"}); got != "inv-1" {
		t.Errorf("OutTradeNo(no key) = %q, want the invoice id inv-1", got)
	}
	if got := OutTradeNo(billing.ChargeRequest{}); got != "" {
		t.Errorf("OutTradeNo(empty) = %q, want the empty string", got)
	}
}

func TestRequireCNY_AcceptsCaseInsensitiveCNYAndNamesTheChannel(t *testing.T) {
	for _, currency := range []string{"CNY", "cny", "Cny"} {
		if err := RequireCNY("alipay", currency); err != nil {
			t.Errorf("RequireCNY(%q) = %v, want nil", currency, err)
		}
	}

	err := RequireCNY("wechat", "USD")
	if !apperr.HasCode(err, billing.ErrUnsupportedCurrency.Code) {
		t.Fatalf("RequireCNY(USD): err = %v, want %s", err, billing.ErrUnsupportedCurrency.Code)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("RequireCNY(USD): err = %T, want *apperr.Error", err)
	}
	if appErr.Params["currency"] != "USD" || appErr.Params["channel"] != "wechat" || appErr.Params["supported_currency"] != "CNY" {
		t.Errorf("Params = %v, want currency=USD channel=wechat supported_currency=CNY", appErr.Params)
	}
}

func TestFirstHeader_ReturnsTheFirstValueCaseSensitively(t *testing.T) {
	headers := map[string][]string{
		"Stripe-Signature": {"first", "second"},
		"lower-node":       {"x"},
	}
	if got := FirstHeader(headers, "Stripe-Signature"); got != "first" {
		t.Errorf("FirstHeader = %q, want the first value", got)
	}
	if got := FirstHeader(headers, "stripe-signature"); got != "" {
		t.Errorf("FirstHeader(lowercased name) = %q, want the lookup to stay case-sensitive", got)
	}
	if got := FirstHeader(headers, "Missing"); got != "" {
		t.Errorf("FirstHeader(missing) = %q, want the empty string", got)
	}
}

// TestParsePrivateKeyPEM_AcceptsBothEncodings pins the two forms the
// domestic platforms' key exports come in; a key that is neither is a
// configuration error surfaced at NewGateway time.
func TestParsePrivateKeyPEM_AcceptsBothEncodings(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	for name, pemBytes := range map[string][]byte{"PKCS#1": pkcs1, "PKCS#8": pkcs8} {
		got, err := ParsePrivateKeyPEM(pemBytes)
		if err != nil {
			t.Fatalf("ParsePrivateKeyPEM(%s): %v", name, err)
		}
		if !got.Equal(key) {
			t.Errorf("ParsePrivateKeyPEM(%s) = a different key", name)
		}
	}

	if _, err := ParsePrivateKeyPEM([]byte("not a PEM block")); err == nil {
		t.Error("ParsePrivateKeyPEM(non-PEM) = nil error, want a refusal")
	}
}

// TestParsePublicKeyPEM_AcceptsPKIXAndCertificate pins both accepted
// forms: the bare PKIX public key both consoles hand out, and the X.509
// certificate a platform certificate download endpoint returns -- either
// must yield the same public key.
func TestParsePublicKeyPEM_AcceptsPKIXAndCertificate(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	pkixBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal PKIX public key: %v", err)
	}
	pkixPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkixBytes})

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gateway.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create self-signed certificate: %v", err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	for name, pemBytes := range map[string][]byte{"PKIX": pkixPEM, "CERTIFICATE": cert} {
		got, err := ParsePublicKeyPEM(pemBytes)
		if err != nil {
			t.Fatalf("ParsePublicKeyPEM(%s): %v", name, err)
		}
		if !got.Equal(&key.PublicKey) {
			t.Errorf("ParsePublicKeyPEM(%s) = a different public key", name)
		}
	}

	if _, err := ParsePublicKeyPEM([]byte("not a PEM block")); err == nil {
		t.Error("ParsePublicKeyPEM(non-PEM) = nil error, want a refusal")
	}
}

func TestGatewayURL_OverrideWinsElseTheProductionDefault(t *testing.T) {
	// The helper is a pure selector: an override is used byte-for-byte
	// (sandbox URLs keep their own scheme, casing and query), the
	// provider's production default applies only when it is empty.
	const raw = "https://OpenAPI.alipay.com/gateway.do?x=1"
	if got := GatewayURL(raw, "https://prod.example"); got != raw {
		t.Errorf("GatewayURL(override) = %q, want the override byte-for-byte", got)
	}
	if got := GatewayURL("", "https://prod.example"); got != "https://prod.example" {
		t.Errorf("GatewayURL(no override) = %q, want the production default", got)
	}
}
