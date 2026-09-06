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

// TestGateway_CreateCharge_RefusesNonCNYCurrency is P1-3's regression test:
// CreateCharge used to hardcode "currency":"CNY" into its outgoing request
// body regardless of req.Amount.Currency, silently collecting a
// caller-supplied non-CNY amount as if it were the same number of CNY
// cents. This test fails on pre-fix code (which reaches the network and
// returns a fabricated success instead of refusing) by asserting the call
// never reaches doer.Do at all.
func TestGateway_CreateCharge_RefusesNonCNYCurrency(t *testing.T) {
	_, platformPubPEM, _ := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)

	doer := &fakeDoer{
		respond: func(*http.Request, []byte) (int, []byte) {
			t.Fatal("CreateCharge reached the network for a non-CNY currency; requireCNY should have refused it first")
			return 0, nil
		},
	}
	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	_, err = gw.CreateCharge(context.Background(), billing.ChargeRequest{
		TenantID:       "tenant-a",
		SubscriptionID: "sub-1",
		InvoiceID:      "inv-1",
		Amount:         billing.Money{Cents: 2900, Currency: "USD"},
		Description:    "Pro plan",
		IdempotencyKey: "idem-1",
	})
	if !hasCode(err, billing.ErrUnsupportedCurrency.Code) {
		t.Errorf("err = %v, want billing.ErrUnsupportedCurrency", err)
	}
}

// TestGateway_CreateCharge_AcceptsLowercaseCNY proves requireCNY's
// case-insensitive comparison: a caller spelling the currency "cny"
// (lowercase, e.g. matching Stripe's own lowercase currency convention
// elsewhere in this codebase) must not be refused.
func TestGateway_CreateCharge_AcceptsLowercaseCNY(t *testing.T) {
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)

	doer := &fakeDoer{
		platform: platformPriv,
		respond: func(*http.Request, []byte) (int, []byte) {
			respBody, _ := json.Marshal(map[string]string{"code_url": "weixin://wxpay/bizpayurl?pr=fake"})
			return 200, respBody
		},
	}
	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	_, err = gw.CreateCharge(context.Background(), billing.ChargeRequest{
		TenantID:       "tenant-a",
		SubscriptionID: "sub-1",
		InvoiceID:      "inv-1",
		Amount:         billing.Money{Cents: 2900, Currency: "cny"},
		Description:    "Pro plan",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Errorf("CreateCharge: %v, want lowercase \"cny\" accepted", err)
	}
}

// TestGateway_OutTradeNoWithSpecialCharacters_RoundTrips is P2-8's
// regression test: an out_trade_no derived from a caller's idempotency key
// may carry URL metacharacters (here "|" and "#"), and QueryStatus used to
// interpolate the reference RAW into both the Authorization header's
// canonical URL and the request URL. A raw "#" is a fragment delimiter:
// Go's HTTP client truncated the request path at it (the signed canonical
// URL and the request actually sent disagreed, and the mchid query
// parameter was swallowed into the fragment), so WeChat Pay could never
// have verified the signature -- the very "the signed URL and the
// requested URL must be identical" requirement of WeChat Pay's APIv3
// scheme. The fix percent-escapes the reference once, with url.PathEscape,
// before BOTH signing and requesting, so the two always agree and the
// reference round-trips: CreateCharge's handle stays usable for a later
// QueryStatus. On pre-fix code the request URI this test asserts is simply
// not what QueryStatus sends (the fragment eats the query, and the raw "|"
// stays unescaped), so this fails before the fix.
func TestGateway_OutTradeNoWithSpecialCharacters_RoundTrips(t *testing.T) {
	const idemKey = "ORD|1#x"
	_, platformPubPEM, platformPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, platformPubPEM)

	created := false
	wantQueryURI := "/v3/pay/transactions/out-trade-no/ORD%7C1%23x?mchid=" + cfg.MchID
	doer := &fakeDoer{
		platform: platformPriv,
		respond: func(req *http.Request, body []byte) (int, []byte) {
			// The request's WIRE identity is its escaped form (RequestURI,
			// what the HTTP request line carries and what WeChat Pay's
			// server compares the signature's canonical URL against) --
			// never req.URL.Path, which Go keeps percent-DECODED.
			switch {
			case req.Method == http.MethodPost && req.URL.RequestURI() == nativePayPath:
				created = true
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				// The order body carries the reference raw -- out_trade_no is
				// a JSON field there, not a URL segment, so no escaping
				// applies to it.
				if payload["out_trade_no"] != idemKey {
					t.Errorf("out_trade_no = %v, want %q", payload["out_trade_no"], idemKey)
				}
				respBody, _ := json.Marshal(map[string]string{"code_url": "weixin://wxpay/bizpayurl?pr=fake"})
				return 200, respBody
			case req.Method == http.MethodGet && req.URL.RequestURI() == wantQueryURI:
				// The Authorization header must sign the SAME escaped
				// canonical URL the request actually carries -- re-verified
				// here against the escaped form from first principles, so a
				// signature over a differently-escaped string can never
				// slip through.
				authz := req.Header.Get("Authorization")
				fields := parseAuthorizationHeader(t, authz)
				merchantPub := mustPublicFromPrivatePEM(t, cfg.MchPrivateKeyPEM)
				message := requestSignMessage(http.MethodGet, wantQueryURI, fields["timestamp"], fields["nonce_str"], "")
				if err := verifyRawSignature(message, fields["signature"], merchantPub); err != nil {
					t.Errorf("QueryStatus signature does not verify over the escaped canonical URL: %v", err)
				}
				respBody, _ := json.Marshal(map[string]any{
					"trade_state": "SUCCESS",
					"amount":      map[string]any{"total": 2900, "currency": "CNY"},
				})
				return 200, respBody
			default:
				t.Fatalf("unexpected call: %s %s", req.Method, req.URL.RequestURI())
				return 0, nil
			}
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
		IdempotencyKey: idemKey,
	})
	if err != nil {
		t.Fatalf("CreateCharge: %v", err)
	}
	if !created {
		t.Fatal("CreateCharge never reached the doer")
	}
	if handle.ChannelReference != billing.ChannelReference(idemKey) {
		t.Errorf("ChannelReference = %q, want %q", handle.ChannelReference, idemKey)
	}

	status, amount, err := gw.QueryStatus(context.Background(), handle.ChannelReference)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if status != billing.ChannelStatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if amount.Cents != 2900 || amount.Currency != "CNY" {
		t.Errorf("amount = %+v", amount)
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
