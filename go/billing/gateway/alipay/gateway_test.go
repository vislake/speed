package alipay

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// fakeDoer is a scripted httpDoer -- this package's own testing seam for
// CreateCharge/QueryStatus (doc.go's own testing-strategy section), since
// Alipay has no Go SDK to stub the way go/pki/signer/kmsaws stubs
// *kms.Client.
type fakeDoer struct {
	lastForm url.Values
	respond  func(form url.Values) (statusCode int, body []byte)
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	f.lastForm = form

	status, respBody := f.respond(form)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}, nil
}

// signAlipayResponse signs body's responseField value with priv, the exact
// byte-substring algorithm verifyResponseEnvelope checks (response.go's
// own doc comment) -- used here to build a genuine, self-consistent fake
// Alipay response the way a real Alipay server would sign one.
func signAlipayResponse(t *testing.T, responseField string, fields map[string]any, priv *rsa.PrivateKey) []byte {
	t.Helper()
	respJSON, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal response fields: %v", err)
	}

	digest := sha256.Sum256(respJSON)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign response: %v", err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	envelope := fmt.Sprintf(`{"%s":%s,"sign":%q,"sign_type":"RSA2"}`, responseField, respJSON, sigB64)
	return []byte(envelope)
}

func testGatewayConfig(t *testing.T, alipayPub []byte) Config {
	t.Helper()
	merchantPrivPEM, _, _ := generateTestKeyPair(t)
	return Config{
		AppID:              "2021000000000000",
		PrivateKeyPEM:      merchantPrivPEM,
		AlipayPublicKeyPEM: alipayPub,
		NotifyURL:          "https://example.test/billing/notify/alipay",
	}
}

func TestGateway_CreateCharge_SignsAndParsesPrecreate(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(form url.Values) (int, []byte) {
			if form.Get("method") != "alipay.trade.precreate" {
				t.Fatalf("method = %q, want alipay.trade.precreate", form.Get("method"))
			}
			var biz map[string]string
			if err := json.Unmarshal([]byte(form.Get("biz_content")), &biz); err != nil {
				t.Fatalf("decode biz_content: %v", err)
			}
			if biz["total_amount"] != "29.00" {
				t.Errorf("total_amount = %q, want 29.00", biz["total_amount"])
			}
			body := signAlipayResponse(t, "alipay_trade_precreate_response", map[string]any{
				"code": "10000", "msg": "Success",
				"out_trade_no": biz["out_trade_no"], "qr_code": "https://qr.alipay.com/fake",
			}, alipayPriv)
			return 200, body
		},
	}

	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	handle, err := gw.CreateCharge(context.Background(), billing.ChargeRequest{
		TenantID:       "tenant-a",
		SubscriptionID: "sub-1",
		InvoiceID:      "inv-1",
		Amount:         billing.Money{Cents: 2900, Currency: "CNY"},
		Description:    "Pro plan",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("CreateCharge: %v", err)
	}
	if handle.ChannelReference != "idem-1" {
		t.Errorf("ChannelReference = %q, want idem-1", handle.ChannelReference)
	}
	if handle.QRCodeContent != "https://qr.alipay.com/fake" {
		t.Errorf("QRCodeContent = %q", handle.QRCodeContent)
	}

	// The outgoing request itself must carry a valid RSA2 signature the
	// merchant private key produced -- verified here against the matching
	// public half, proving CreateCharge really signs its own requests, not
	// just parses signed responses.
	merchantPub := mustPublicFromPrivatePEM(t, cfg.PrivateKeyPEM)
	params := map[string]string{}
	for k := range doer.lastForm {
		params[k] = doer.lastForm.Get(k)
	}
	if err := VerifySignature(params, merchantPub); err != nil {
		t.Errorf("outgoing request signature did not verify: %v", err)
	}
}

func TestGateway_QueryStatus_MapsTradeStatus(t *testing.T) {
	tests := []struct {
		name        string
		tradeStatus string
		wantStatus  billing.ChannelStatus
	}{
		{"wait_buyer_pay", "WAIT_BUYER_PAY", billing.ChannelStatusPending},
		{"trade_success", "TRADE_SUCCESS", billing.ChannelStatusSucceeded},
		{"trade_finished", "TRADE_FINISHED", billing.ChannelStatusSucceeded},
		{"trade_closed", "TRADE_CLOSED", billing.ChannelStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
			cfg := testGatewayConfig(t, alipayPubPEM)

			doer := &fakeDoer{
				respond: func(form url.Values) (int, []byte) {
					body := signAlipayResponse(t, "alipay_trade_query_response", map[string]any{
						"code": "10000", "msg": "Success",
						"trade_status": tt.tradeStatus, "total_amount": "29.00",
					}, alipayPriv)
					return 200, body
				},
			}
			gw, err := newGatewayWithClient(doer, cfg)
			if err != nil {
				t.Fatalf("newGatewayWithClient: %v", err)
			}

			status, amount, err := gw.QueryStatus(context.Background(), "ORD1")
			if err != nil {
				t.Fatalf("QueryStatus: %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
			if amount.Cents != 2900 || amount.Currency != "CNY" {
				t.Errorf("amount = %+v", amount)
			}
		})
	}
}

func TestGateway_QueryStatus_NotFound(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(url.Values) (int, []byte) {
			body := signAlipayResponse(t, "alipay_trade_query_response", map[string]any{
				"code": "40004", "msg": "Business Failed",
				"sub_code": "ACQ.TRADE_NOT_EXIST", "sub_msg": "交易不存在",
			}, alipayPriv)
			return 200, body
		},
	}
	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	_, _, err = gw.QueryStatus(context.Background(), "ORD_MISSING")
	if !hasCode(err, billing.ErrChannelReferenceNotFound.Code) {
		t.Errorf("err = %v, want billing.ErrChannelReferenceNotFound", err)
	}
}

func TestGateway_QueryStatus_ResponseSignedByWrongKey(t *testing.T) {
	_, alipayPubPEM, _ := generateTestKeyPair(t)
	_, _, wrongPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(url.Values) (int, []byte) {
			body := signAlipayResponse(t, "alipay_trade_query_response", map[string]any{
				"code": "10000", "msg": "Success", "trade_status": "TRADE_SUCCESS", "total_amount": "1.00",
			}, wrongPriv) // signed by a key that does NOT match cfg.AlipayPublicKeyPEM
			return 200, body
		},
	}
	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	_, _, err = gw.QueryStatus(context.Background(), "ORD1")
	if err == nil {
		t.Error("QueryStatus accepted a response signed by the wrong key")
	}
}

func TestNewGateway_RequiresConfig(t *testing.T) {
	if _, err := NewGateway(Config{}); err == nil {
		t.Error("NewGateway(Config{}) = nil error, want an error")
	}
}

func mustPublicFromPrivatePEM(t *testing.T, privPEM []byte) *rsa.PublicKey {
	t.Helper()
	priv, err := ParsePrivateKeyPEM(privPEM)
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM: %v", err)
	}
	return &priv.PublicKey
}

// hasCode mirrors billing's own unexported hasCode helper (errors.go).
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
