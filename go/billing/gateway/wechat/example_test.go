package wechat_test

// Runnable documentation for the WeChat-Pay-backed billing.PaymentGateway,
// compiled and executed by `go test`. Neither Example below reaches a real
// WeChat Pay account -- see doc.go's own "no integration leg" section. Both
// demonstrate construction and billing.PaymentGatewayRegistry usage only.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/billing/gateway/wechat"
)

// examplePEMKeyPair generates a throwaway RSA key pair, standing in for a
// real WeChat Pay merchant's own API certificate and the platform's public
// certificate -- no such credential is reachable from this Example.
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
	mchPrivPEM, _ := examplePEMKeyPair()
	_, platformPubPEM := examplePEMKeyPair()

	gw, err := wechat.NewGateway(wechat.Config{
		MchID:                "1900000001",
		AppID:                "wx1234567890",
		MchCertSerialNo:      "EXAMPLESERIAL",
		MchPrivateKeyPEM:     mchPrivPEM,
		APIv3Key:             []byte("01234567890123456789012345678901"), // 32 bytes
		PlatformPublicKeyPEM: platformPubPEM,
		NotifyURL:            "https://example.test/billing/notify/wechat",
	})
	if err != nil {
		fmt.Println("new gateway:", err)
		return
	}
	//nolint:staticcheck // QF1011: the assertion doubles as written doc that
	// NewGateway satisfies billing.PaymentGateway.
	var _ billing.PaymentGateway = gw

	fmt.Println("gateway wired; the first CreateCharge/QueryStatus call contacts WeChat Pay")
	// Output:
	// gateway wired; the first CreateCharge/QueryStatus call contacts WeChat Pay
}

// Example demonstrates the package's self-registration: importing it for
// side effect makes "gateway.wechat" build through
// billing.PaymentGatewayRegistry.
func Example() {
	mchPrivPEM, _ := examplePEMKeyPair()
	_, platformPubPEM := examplePEMKeyPair()

	cfg := pkgcore.Config{
		"mch_id":                  "1900000001",
		"app_id":                  "wx1234567890",
		"mch_cert_serial_no":      "EXAMPLESERIAL",
		"mch_private_key_pem":     string(mchPrivPEM),
		"api_v3_key":              "01234567890123456789012345678901",
		"platform_public_key_pem": string(platformPubPEM),
		"notify_url":              "https://example.test/billing/notify/wechat",
	}
	gw, caps, err := billing.PaymentGatewayRegistry.Build("gateway.wechat", cfg)
	fmt.Println("gateway.wechat:", err, gw != nil, caps)

	// Output:
	// gateway.wechat: <nil> true none
}
