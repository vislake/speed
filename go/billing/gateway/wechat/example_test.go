package wechat_test

// Runnable documentation for the WeChat-Pay-backed billing.PaymentGateway,
// compiled and executed by `go test`. None of the Examples below reach a
// real WeChat Pay account -- see doc.go's own "no integration leg" section.
// ExampleNewGateway and Example demonstrate construction and
// billing.PaymentGatewayRegistry usage; ExampleGateway_VerifyWebhook is the
// package's runnable, doc-rendered proof of the real thing: it
// AEAD_AES_256_GCM-encrypts a transaction resource with a locally generated
// APIv3 key and RSA-SHA256-signs the notify envelope with a locally
// generated platform key pair -- the exact two-layer scheme decrypt.go's
// decryptResource and sign.go's VerifySignature document (WeChat Pay ships
// no published third-party Go test vector, hence a locally-constructed
// fixture rather than an SDK helper -- doc.go's own testing-strategy
// section) -- and verifies it through a real Gateway with no network call.

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// encryptResource is decryptResource's inverse (this package's own
// decrypt.go), used to build a realistic AEAD_AES_256_GCM resource envelope
// the way a real WeChat Pay server would produce one. The nonce is
// JSON-string-carried, so its raw bytes must be valid UTF-8 -- restricted to
// an ASCII alphabet here, exactly like a real WeChat Pay nonce.
func encryptResource(plaintext []byte, associatedData string, apiV3Key []byte) (nonce, ciphertextB64 string) {
	const nonceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

	block, err := aes.NewCipher(apiV3Key)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}

	idx := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(idx); err != nil {
		panic(err)
	}
	nonceBytes := make([]byte, gcm.NonceSize())
	for i, b := range idx {
		nonceBytes[i] = nonceAlphabet[int(b)%len(nonceAlphabet)]
	}

	ciphertext := gcm.Seal(nil, nonceBytes, plaintext, []byte(associatedData))
	return string(nonceBytes), base64.StdEncoding.EncodeToString(ciphertext)
}

// signNotify RSA-SHA256-signs timestamp+"\n"+nonce+"\n"+body+"\n" with priv
// -- the exact message shape sign.go's own notifySignMessage builds for an
// inbound webhook notification, so a fixture signed here verifies against
// wechat.VerifySignature and against a real Gateway's VerifyWebhook
// identically.
func signNotify(timestamp, nonce, body string, priv *rsa.PrivateKey) string {
	message := strings.Join([]string{timestamp, nonce, body, ""}, "\n")
	digest := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// ExampleGateway_VerifyWebhook encrypts a TRANSACTION.SUCCESS resource
// under a locally generated APIv3 key, signs the notify envelope with a
// locally generated platform key pair, verifies it through a real Gateway
// and prints the resulting billing.NormalizedEvent -- the signature
// verification plus payload normalization the VerifyWebhook doc comment on
// billing.PaymentGateway describes.
func ExampleGateway_VerifyWebhook() {
	mchPrivPEM, _ := examplePEMKeyPair()
	platformPrivPEM, platformPubPEM := examplePEMKeyPair()
	platformPriv, err := wechat.ParsePrivateKeyPEM(platformPrivPEM)
	if err != nil {
		fmt.Println("parse platform private key:", err)
		return
	}
	apiV3Key := []byte("01234567890123456789012345678901") // 32 bytes

	gw, err := wechat.NewGateway(wechat.Config{
		MchID:                "1900000001",
		AppID:                "wx1234567890",
		MchCertSerialNo:      "EXAMPLESERIAL",
		MchPrivateKeyPEM:     mchPrivPEM,
		APIv3Key:             apiV3Key,
		PlatformPublicKeyPEM: platformPubPEM,
		NotifyURL:            "https://example.test/billing/notify/wechat",
	})
	if err != nil {
		fmt.Println("new gateway:", err)
		return
	}

	// attach carries {"t":tenant,"s":subscription,"i":invoice} -- this
	// package's own gateway.go attachPayload JSON tags -- exactly what
	// CreateCharge attached to the order.
	transaction, err := json.Marshal(map[string]any{
		"out_trade_no": "ORD_EXAMPLE_1",
		"trade_state":  "SUCCESS",
		"attach":       `{"t":"tenant-a","s":"sub-1","i":"inv-1"}`,
		"success_time": time.Now().Format(time.RFC3339),
		"amount": map[string]any{
			"total":    2900,
			"currency": "CNY",
		},
	})
	if err != nil {
		fmt.Println("marshal transaction:", err)
		return
	}

	const associatedData = "transaction"
	nonce, ciphertextB64 := encryptResource(transaction, associatedData, apiV3Key)

	body, err := json.Marshal(map[string]any{
		"id":            "evt_example_1",
		"create_time":   time.Now().Format(time.RFC3339),
		"resource_type": "encrypt-resource",
		"event_type":    "TRANSACTION.SUCCESS",
		"resource": map[string]any{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      ciphertextB64,
			"nonce":           nonce,
			"associated_data": associatedData,
		},
	})
	if err != nil {
		fmt.Println("marshal envelope:", err)
		return
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sigNonce := "notify-nonce-example"
	sig := signNotify(timestamp, sigNonce, string(body), platformPriv)

	event, err := gw.VerifyWebhook(context.Background(), map[string][]string{
		"Wechatpay-Signature": {sig},
		"Wechatpay-Timestamp": {timestamp},
		"Wechatpay-Nonce":     {sigNonce},
	}, body)
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
	// event id: ORD_EXAMPLE_1:SUCCESS
	// channel: wechat
	// channel reference: ORD_EXAMPLE_1
	// type: charge_succeeded
	// status: succeeded
	// amount: 2900 CNY
	// tenant: tenant-a subscription: sub-1 invoice: inv-1
}
