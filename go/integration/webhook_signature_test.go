package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

func TestSignWebhookPayload_MatchesManualComputation(t *testing.T) {
	secret := "whsec_test"
	var timestamp int64 = 1_700_000_000
	body := []byte(`{"event":{"type":"org.member.joined","version":"v1"},"data":{}}`)

	got := signWebhookPayload(secret, timestamp, body)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	want := webhookSignatureScheme + "=" + hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Errorf("signWebhookPayload = %q, want %q", got, want)
	}
}

func TestSignWebhookPayload_CarriesSchemePrefix(t *testing.T) {
	sig := signWebhookPayload("whsec_x", 1, []byte("{}"))
	if !strings.HasPrefix(sig, webhookSignatureScheme+"=") {
		t.Errorf("signature = %q, want it to start with %q", sig, webhookSignatureScheme+"=")
	}
}

func TestSignWebhookPayload_DifferentSecretsProduceDifferentSignatures(t *testing.T) {
	body := []byte(`{"a":1}`)
	a := signWebhookPayload("whsec_a", 1, body)
	b := signWebhookPayload("whsec_b", 1, body)
	if a == b {
		t.Error("two different secrets produced the same signature")
	}
}

func TestSignWebhookPayload_DifferentTimestampsProduceDifferentSignatures(t *testing.T) {
	body := []byte(`{"a":1}`)
	a := signWebhookPayload("whsec_x", 1, body)
	b := signWebhookPayload("whsec_x", 2, body)
	if a == b {
		t.Error("two different timestamps produced the same signature -- replay protection depends on the timestamp being covered by the signature")
	}
}

func TestSignWebhookPayload_DifferentBodiesProduceDifferentSignatures(t *testing.T) {
	a := signWebhookPayload("whsec_x", 1, []byte(`{"a":1}`))
	b := signWebhookPayload("whsec_x", 1, []byte(`{"a":2}`))
	if a == b {
		t.Error("two different bodies produced the same signature")
	}
}
