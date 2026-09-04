package alipay_test

// Runnable documentation for the Alipay-backed billing.PaymentGateway,
// compiled and executed by `go test`. Neither Example below reaches a real
// Alipay account -- see doc.go's own "no integration leg" section. Both
// demonstrate construction and billing.PaymentGatewayRegistry usage only.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"

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
