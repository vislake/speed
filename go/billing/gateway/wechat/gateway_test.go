package wechat

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// fakeDoer is a scripted httpDoer -- this package's own testing seam for
// CreateCharge/QueryStatus (doc.go's own testing-strategy section).
type fakeDoer struct {
	lastReq  *http.Request
	lastBody []byte
	respond  func(req *http.Request, body []byte) (statusCode int, respBody []byte)
	platform *rsa.PrivateKey // signs every response the way WeChat Pay's own server would
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	f.lastReq = req
	f.lastBody = body

	status, respBody := f.respond(req, body)

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "resp-nonce"
	sig, err := signRequest(notifySignMessage(timestamp, nonce, string(respBody)), f.platform)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set("Wechatpay-Signature", sig)
	header.Set("Wechatpay-Timestamp", timestamp)
	header.Set("Wechatpay-Nonce", nonce)

	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}, nil
}

func testGatewayConfig(t *testing.T, platformPub []byte) Config {
	t.Helper()
	mchPrivPEM, _, _ := generateTestKeyPair(t)
	return Config{
		MchID:                "1900000001",
		AppID:                "wx1234567890",
		MchCertSerialNo:      "SERIAL123",
		MchPrivateKeyPEM:     mchPrivPEM,
		APIv3Key:             testAPIv3Key(),
		PlatformPublicKeyPEM: platformPub,
		NotifyURL:            "https://example.test/billing/notify/wechat",
	}
}

func TestGateway_CreateCharge_SignsAndParsesNativeOrder(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)

	doer := &fakeDoer{
		platform: platformPriv,
		respond: func(req *http.Request, body []byte) (int, []byte) {
			if req.Method != http.MethodPost || req.URL.Path != nativePayPath {
				t.Fatalf("unexpected call: %s %s", req.Method, req.URL.Path)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			amount, _ := payload["amount"].(map[string]any)
			if amount["total"] != float64(2900) {
				t.Errorf("amount.total = %v, want 2900", amount["total"])
			}
			respBody, _ := json.Marshal(map[string]string{"code_url": "weixin://wxpay/bizpayurl?pr=fake"})
			return 200, respBody
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
	if handle.QRCodeContent != "weixin://wxpay/bizpayurl?pr=fake" {
		t.Errorf("QRCodeContent = %q", handle.QRCodeContent)
	}

	// The outgoing request itself must carry a genuine RSA-SHA256
	// signature the merchant private key produced -- parsed out of the
	// real Authorization header and re-verified here against the matching
	// public half, proving CreateCharge really signs the exact request it
	// sends, not just a value that merely looks like a signature.
	authz := doer.lastReq.Header.Get("Authorization")
	fields := parseAuthorizationHeader(t, authz)
	merchantPub := mustPublicFromPrivatePEM(t, cfg.MchPrivateKeyPEM)
	message := requestSignMessage(http.MethodPost, nativePayPath, fields["timestamp"], fields["nonce_str"], string(doer.lastBody))
	if err := verifyRawSignature(message, fields["signature"], merchantPub); err != nil {
		t.Errorf("outgoing request signature did not verify: %v", err)
	}
	if fields["mchid"] != cfg.MchID {
		t.Errorf("Authorization mchid = %q, want %q", fields["mchid"], cfg.MchID)
	}
	if fields["serial_no"] != cfg.MchCertSerialNo {
		t.Errorf("Authorization serial_no = %q, want %q", fields["serial_no"], cfg.MchCertSerialNo)
	}
}

// parseAuthorizationHeader extracts the WECHATPAY2-SHA256-RSA2048
// key="value" fields from a real Authorization header value -- test-only
// parsing, deliberately simple (this scheme's values never contain a
// literal quote), since production code only ever WRITES this header,
// never reads one back.
func parseAuthorizationHeader(t *testing.T, header string) map[string]string {
	t.Helper()
	const prefix = "WECHATPAY2-SHA256-RSA2048 "
	if !strings.HasPrefix(header, prefix) {
		t.Fatalf("Authorization header %q missing scheme prefix", header)
	}
	out := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(header, prefix), ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			t.Fatalf("Authorization header field %q is not key=value", part)
		}
		out[kv[0]] = strings.Trim(kv[1], `"`)
	}
	return out
}

// verifyRawSignature is VerifySignature's core minus the timestamp/nonce/
// body message-building -- used here because the outgoing REQUEST message
// shape (requestSignMessage) differs from the notify/response message
// shape (notifySignMessage) VerifySignature itself checks.
func verifyRawSignature(message, sigB64 string, pub *rsa.PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(message))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
}

func TestGateway_QueryStatus_MapsTradeState(t *testing.T) {
	tests := []struct {
		name       string
		tradeState string
		wantStatus billing.ChannelStatus
	}{
		{"notpay", "NOTPAY", billing.ChannelStatusPending},
		{"success", "SUCCESS", billing.ChannelStatusSucceeded},
		{"closed", "CLOSED", billing.ChannelStatusFailed},
		{"refund", "REFUND", billing.ChannelStatusRefunded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, platformPubPEM, platformPriv := generateTestKeyPair(t)
			cfg := testGatewayConfig(t, platformPubPEM)

			doer := &fakeDoer{
				platform: platformPriv,
				respond: func(req *http.Request, _ []byte) (int, []byte) {
					respBody, _ := json.Marshal(map[string]any{
						"trade_state": tt.tradeState,
						"amount":      map[string]any{"total": 2900, "currency": "CNY"},
					})
					return 200, respBody
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
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)

	doer := &fakeDoer{
		platform: platformPriv,
		respond: func(req *http.Request, _ []byte) (int, []byte) {
			respBody, _ := json.Marshal(map[string]string{"code": "ORDER_NOT_EXIST", "message": "order not found"})
			return 404, respBody
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
	_, platformPubPEM, _ := generateTestKeyPair(t)
	_, _, wrongPlatformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)

	doer := &fakeDoer{
		platform: wrongPlatformPriv, // signs the response with a key that does NOT match cfg.PlatformPublicKeyPEM
		respond: func(req *http.Request, _ []byte) (int, []byte) {
			respBody, _ := json.Marshal(map[string]any{"trade_state": "SUCCESS", "amount": map[string]any{"total": 1, "currency": "CNY"}})
			return 200, respBody
		},
	}
	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	_, _, err = gw.QueryStatus(context.Background(), "ORD1")
	if err == nil {
		t.Error("QueryStatus accepted a response signed by the wrong platform key")
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
