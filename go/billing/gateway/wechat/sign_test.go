package wechat

import (
	"strings"
	"testing"
)

func TestRequestSignMessage_Shape(t *testing.T) {
	got := requestSignMessage("POST", "/v3/pay/transactions/native", "1700000000", "nonce123", `{"a":1}`)
	want := "POST\n/v3/pay/transactions/native\n1700000000\nnonce123\n{\"a\":1}\n"
	if got != want {
		t.Errorf("requestSignMessage = %q, want %q", got, want)
	}
}

func TestNotifySignMessage_Shape(t *testing.T) {
	got := notifySignMessage("1700000000", "nonce123", `{"a":1}`)
	want := "1700000000\nnonce123\n{\"a\":1}\n"
	if got != want {
		t.Errorf("notifySignMessage = %q, want %q", got, want)
	}
}

// TestSignRequest_VerifySignature_RoundTrip proves the RSA-SHA256 sign/
// verify round trip against a locally generated key pair -- this package's
// own sanctioned fixture strategy, per doc.go's own note that WeChat Pay
// ships no published third-party test vector.
func TestSignRequest_VerifySignature_RoundTrip(t *testing.T) {
	_, pubPEM, priv := generateTestKeyPair(t)
	pub, err := ParsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}

	timestamp, nonce, body := "1700000000", "nonce123", `{"a":1}`
	message := notifySignMessage(timestamp, nonce, body)
	sig, err := signRequest(message, priv)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}

	if err := VerifySignature(timestamp, nonce, []byte(body), sig, pub); err != nil {
		t.Errorf("VerifySignature (genuine) failed: %v", err)
	}
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	_, pubPEM, priv := generateTestKeyPair(t)
	pub, err := ParsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}

	timestamp, nonce := "1700000000", "nonce123"
	sig, err := signRequest(notifySignMessage(timestamp, nonce, `{"a":1}`), priv)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}

	if err := VerifySignature(timestamp, nonce, []byte(`{"a":999}`), sig, pub); err == nil {
		t.Error("VerifySignature accepted a tampered body")
	}
}

func TestVerifySignature_WrongKey(t *testing.T) {
	_, _, priv := generateTestKeyPair(t)
	_, otherPubPEM, _ := generateTestKeyPair(t)
	otherPub, err := ParsePublicKeyPEM(otherPubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}

	timestamp, nonce, body := "1700000000", "n", `{}`
	sig, err := signRequest(notifySignMessage(timestamp, nonce, body), priv)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}

	if err := VerifySignature(timestamp, nonce, []byte(body), sig, otherPub); err == nil {
		t.Error("VerifySignature accepted a signature verified against the wrong public key")
	}
}

func TestVerifySignature_EmptySignature(t *testing.T) {
	_, pubPEM, _ := generateTestKeyPair(t)
	pub, err := ParsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}
	if err := VerifySignature("1700000000", "n", []byte("{}"), "", pub); err == nil {
		t.Error("VerifySignature accepted an empty signature")
	}
}

func TestAuthorizationHeader_CarriesExpectedFields(t *testing.T) {
	_, _, priv := generateTestKeyPair(t)
	cfg := Config{MchID: "1900000001", MchCertSerialNo: "SERIAL123"}

	header, err := authorizationHeader(cfg, priv, "POST", "/v3/pay/transactions/native", []byte(`{}`))
	if err != nil {
		t.Fatalf("authorizationHeader: %v", err)
	}
	for _, want := range []string{
		`WECHATPAY2-SHA256-RSA2048`,
		`mchid="1900000001"`,
		`serial_no="SERIAL123"`,
	} {
		if !strings.Contains(header, want) {
			t.Errorf("Authorization header %q missing %q", header, want)
		}
	}
}
