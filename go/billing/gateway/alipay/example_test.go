package alipay_test

// Runnable documentation for the Alipay-backed billing.PaymentGateway,
// compiled and executed by `go test`. None of the Examples below reach a
// real Alipay account -- see doc.go's own "no integration leg" section.
// ExampleNewGateway and Example demonstrate construction and
// billing.PaymentGatewayRegistry usage; ExampleGateway_VerifyWebhook is the
// package's runnable, doc-rendered proof of the real thing: it RSA2-signs a
// notify body with a locally generated key pair, exactly the algorithm
// sign.go's own signContent documents (Alipay ships no published
// third-party signature test vector the way Stripe's SDK does, hence a
// locally-constructed fixture rather than an SDK helper -- doc.go's own
// testing-strategy section), and verifies it through a real Gateway with no
// network call.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/billing/gateway/alipay"
)

// examplePrivateKeyPEM/examplePublicKeyPEM are a throwaway RSA key pair
// generated once for these Examples, standing in for a real Alipay
// application's merchant key and Alipay's own public key -- no such
// credential is reachable from this Example.
func examplePEMKeyPair() (privPEM, pubPEM []byte) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
}

// ExampleNewGateway shows constructing a Gateway directly, the escape hatch
// for a caller that wants to wire it with billing.WithGateways rather than
// going through billing.PaymentGatewayRegistry. Nothing is dialed here --
// the underlying HTTP client issues no request until the first
// CreateCharge/QueryStatus call.
func ExampleNewGateway() {
	privPEM, pubPEM := examplePEMKeyPair()

	gw, err := alipay.NewGateway(alipay.Config{
		AppID:              "2021000000000000",
		PrivateKeyPEM:      privPEM,
		AlipayPublicKeyPEM: pubPEM,
		NotifyURL:          "https://example.test/billing/notify/alipay",
	})
	if err != nil {
		fmt.Println("new gateway:", err)
		return
	}
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// NewGateway satisfies billing.PaymentGateway.
	var _ billing.PaymentGateway = gw

	fmt.Println("gateway wired; the first CreateCharge/QueryStatus call contacts Alipay")
	// Output:
	// gateway wired; the first CreateCharge/QueryStatus call contacts Alipay
}

// Example demonstrates the package's self-registration: importing it for
// side effect makes "gateway.alipay" build through
// billing.PaymentGatewayRegistry.
func Example() {
	privPEM, pubPEM := examplePEMKeyPair()

	cfg := pkgcore.Config{
		"app_id":                "2021000000000000",
		"private_key_pem":       string(privPEM),
		"alipay_public_key_pem": string(pubPEM),
		"notify_url":            "https://example.test/billing/notify/alipay",
	}
	gw, caps, err := billing.PaymentGatewayRegistry.Build("gateway.alipay", cfg)
	fmt.Println("gateway.alipay:", err, gw != nil, caps)

	// Output:
	// gateway.alipay: <nil> true none
}

// signNotifyParams RSA2-signs params the same way Alipay's own servers sign
// an outgoing async notification, per
// https://opendocs.alipay.com/common/02kdnc's documented algorithm: every
// param whose key is neither "sign" nor "sign_type", and whose value is
// non-empty, sorted ascending by key, joined as
// "key1=value1&key2=value2&...", SHA-256 hashed and RSA PKCS#1v1.5 signed --
// the exact canonical form this package's own sign.go signContent builds,
// so a fixture built here verifies against alipay.VerifySignature and
// against a real Gateway's VerifyWebhook identically.
func signNotifyParams(params map[string]string, priv *rsa.PrivateKey) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" || k == "sign_type" || params[k] == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}

	digest := sha256.Sum256([]byte(b.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// ExampleGateway_VerifyWebhook RSA2-signs a TRADE_SUCCESS notify fixture
// with a locally generated merchant key pair, verifies it through a real
// Gateway and prints the resulting billing.NormalizedEvent -- the signature
// verification plus payload normalization the VerifyWebhook doc comment on
// billing.PaymentGateway describes.
func ExampleGateway_VerifyWebhook() {
	privPEM, pubPEM := examplePEMKeyPair()
	priv, err := alipay.ParsePrivateKeyPEM(privPEM)
	if err != nil {
		fmt.Println("parse private key:", err)
		return
	}

	gw, err := alipay.NewGateway(alipay.Config{
		AppID:              "2021000000000000",
		PrivateKeyPEM:      privPEM,
		AlipayPublicKeyPEM: pubPEM,
		NotifyURL:          "https://example.test/billing/notify/alipay",
	})
	if err != nil {
		fmt.Println("new gateway:", err)
		return
	}

	// passback_params carries {"t":tenant,"s":subscription,"i":invoice} --
	// this package's own gateway.go passbackPayload JSON tags -- exactly
	// what CreateCharge attached when it created the order.
	params := map[string]string{
		"notify_id":       "notify_example_1",
		"out_trade_no":    "ORD_EXAMPLE_1",
		"trade_no":        "2026090422001",
		"trade_status":    "TRADE_SUCCESS",
		"total_amount":    "29.00",
		"passback_params": `{"t":"tenant-a","s":"sub-1","i":"inv-1"}`,
	}
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	values.Set("sign", signNotifyParams(params, priv))
	values.Set("sign_type", "RSA2")

	event, err := gw.VerifyWebhook(context.Background(), nil, []byte(values.Encode()))
	if err != nil {
		fmt.Println("verify webhook:", err)
		return
	}

	fmt.Println("event id:", event.EventID)
	fmt.Println("channel:", event.Channel)
	fmt.Println("channel reference:", event.ChannelReference)
	fmt.Println("type:", event.Type)
	fmt.Println("status:", event.Status)
	fmt.Println("amount:", event.Amount.Cents, event.Amount.Currency)
	fmt.Println("tenant:", event.TenantID, "subscription:", event.SubscriptionID, "invoice:", event.InvoiceID)
	// Output:
	// event id: ORD_EXAMPLE_1:TRADE_SUCCESS
	// channel: alipay
	// channel reference: ORD_EXAMPLE_1
	// type: charge_succeeded
	// status: succeeded
	// amount: 2900 CNY
	// tenant: tenant-a subscription: sub-1 invoice: inv-1
}
