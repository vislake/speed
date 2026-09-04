package alipay

import "testing"

func TestSignContent_SortsKeysAndOmitsSignFields(t *testing.T) {
	got := signContent(map[string]string{
		"out_trade_no": "ORD1",
		"sign":         "should-be-omitted",
		"sign_type":    "RSA2",
		"app_id":       "2021000000000000",
		"empty":        "",
	})
	want := "app_id=2021000000000000&out_trade_no=ORD1"
	if got != want {
		t.Errorf("signContent = %q, want %q", got, want)
	}
}

// TestSignParams_VerifySignature_RoundTrip proves the RSA2 sign/verify
// round trip against a locally generated key pair -- this package's own
// sanctioned fixture strategy, per doc.go's own note that Alipay ships no
// published third-party test vector.
func TestSignParams_VerifySignature_RoundTrip(t *testing.T) {
	_, pubPEM, priv := generateTestKeyPair(t)
	pub, err := ParsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}

	params := map[string]string{
		"out_trade_no": "ORD123456",
		"trade_status": "TRADE_SUCCESS",
		"total_amount": "29.00",
	}
	sig, err := signParams(params, priv)
	if err != nil {
		t.Fatalf("signParams: %v", err)
	}
	params["sign"] = sig
	params["sign_type"] = "RSA2"

	if err := VerifySignature(params, pub); err != nil {
		t.Errorf("VerifySignature (genuine) failed: %v", err)
	}
}

// TestVerifySignature_TamperedParam proves a modified parameter is refused
// -- the real attack this verification exists to stop: anyone who can reach
// the notify endpoint sending a forged or altered body.
func TestVerifySignature_TamperedParam(t *testing.T) {
	_, pubPEM, priv := generateTestKeyPair(t)
	pub, err := ParsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}

	params := map[string]string{
		"out_trade_no": "ORD123456",
		"total_amount": "29.00",
	}
	sig, err := signParams(params, priv)
	if err != nil {
		t.Fatalf("signParams: %v", err)
	}
	params["sign"] = sig
	params["total_amount"] = "999.00" // tamper AFTER signing

	if err := VerifySignature(params, pub); err == nil {
		t.Error("VerifySignature accepted a tampered parameter set")
	}
}

func TestVerifySignature_WrongKey(t *testing.T) {
	_, _, priv := generateTestKeyPair(t)
	_, otherPubPEM, _ := generateTestKeyPair(t)
	otherPub, err := ParsePublicKeyPEM(otherPubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}

	params := map[string]string{"out_trade_no": "ORD1"}
	sig, err := signParams(params, priv)
	if err != nil {
		t.Fatalf("signParams: %v", err)
	}
	params["sign"] = sig

	if err := VerifySignature(params, otherPub); err == nil {
		t.Error("VerifySignature accepted a signature verified against the wrong public key")
	}
}

func TestVerifySignature_MissingSign(t *testing.T) {
	_, pubPEM, _ := generateTestKeyPair(t)
	pub, err := ParsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}
	if err := VerifySignature(map[string]string{"out_trade_no": "ORD1"}, pub); err == nil {
		t.Error("VerifySignature accepted a parameter set with no sign")
	}
}
