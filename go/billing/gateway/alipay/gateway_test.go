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
	"sort"
	"strings"
	"testing"
	"time"

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
	// public half with the REQUEST-side canonical form
	// (verifyRequestSignature), proving CreateCharge really signs its own
	// requests over the parameter set Alipay's gateway verifies. The
	// notify-side VerifySignature must NOT be used for this check: it
	// canonicalizes with sign_type excluded, the inbound-notification rule,
	// and would pass an outgoing signature that failed to cover sign_type
	// (the very asymmetry P2-6 pins).
	merchantPub := mustPublicFromPrivatePEM(t, cfg.PrivateKeyPEM)
	params := map[string]string{}
	for k := range doer.lastForm {
		params[k] = doer.lastForm.Get(k)
	}
	if err := verifyRequestSignature(params, merchantPub); err != nil {
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

// TestGateway_CreateCharge_RefusesNonCNYCurrency is P1-3's regression test:
// CreateCharge used to ignore req.Amount.Currency entirely (formatAmount's
// own doc comment admitted a non-CNY request was "a caller error this
// package does not detect"), silently collecting a caller-supplied non-CNY
// amount as if it were the same number of CNY cents. This test fails on
// pre-fix code (which reaches the network instead of refusing) by
// asserting the call never reaches doer.Do at all.
func TestGateway_CreateCharge_RefusesNonCNYCurrency(t *testing.T) {
	_, alipayPubPEM, _ := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(url.Values) (int, []byte) {
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
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(form url.Values) (int, []byte) {
			var biz map[string]string
			if err := json.Unmarshal([]byte(form.Get("biz_content")), &biz); err != nil {
				t.Fatalf("decode biz_content: %v", err)
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

// verifyRequestSignature is an INDEPENDENT verifier for the signature on an
// OUTGOING request, built deliberately NOT from this package's own
// signContent: Alipay's request-signing rule (opendocs.alipay.com/common/02kdnc's
// implement-signing-yourself section, and Alipay's own reference SDK
// implementations) does
// NOT exclude sign_type from the merchant's canonical string -- the string
// is every non-empty request parameter except sign itself, which has not
// been added yet at signing time -- while the notify-verification rule this
// package's signContent implements DOES exclude sign_type. The two
// parameter sets are asymmetric by design, and tests that verify an
// outgoing request's signature with the notify-side canonical form would
// pass even when the request was signed over the wrong string. This helper
// recomputes the REQUEST-side canonical form from first principles, so the
// regression it powers fails whenever the request signature does not cover
// sign_type.
func verifyRequestSignature(params map[string]string, pub *rsa.PublicKey) error {
	sigB64 := params["sign"]
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode sign: %w", err)
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" { // the only parameter absent from an outgoing request at signing time
			continue
		}
		if params[k] == "" {
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
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
}

// TestGateway_CreateCharge_OutgoingSignatureCoversSignType is P2-6's
// regression test: signContent used to be shared by request signing and
// notify verification, excluding "sign_type" from BOTH canonical strings.
// Alipay's documented scheme is asymmetric -- the merchant's outgoing
// request string includes sign_type (sign.go's own doc comment now states
// the verified asymmetry and its sources), while notify verification
// excludes it -- so every outgoing request this package signed was signed
// over a string missing a parameter Alipay's own server includes when it
// verifies: a signature mismatch on the first real call. This test fails
// on the pre-fix signParams (whose outgoing signature does not verify
// against the request-side canonical form recomputed independently here,
// sign_type included) and passes once the request side signs the
// asymmetric string.
func TestGateway_CreateCharge_OutgoingSignatureCoversSignType(t *testing.T) {
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

	if _, err := gw.CreateCharge(context.Background(), billing.ChargeRequest{
		TenantID:       "tenant-a",
		SubscriptionID: "sub-1",
		InvoiceID:      "inv-1",
		Amount:         billing.Money{Cents: 2900, Currency: "CNY"},
		Description:    "Pro plan",
		IdempotencyKey: "idem-1",
	}); err != nil {
		t.Fatalf("CreateCharge: %v", err)
	}

	params := map[string]string{}
	for k := range doer.lastForm {
		params[k] = doer.lastForm.Get(k)
	}
	merchantPub := mustPublicFromPrivatePEM(t, cfg.PrivateKeyPEM)
	if err := verifyRequestSignature(params, merchantPub); err != nil {
		t.Errorf("outgoing request signature does not verify against the request-side canonical form (sign_type included): %v", err)
	}
	if params["sign_type"] != "RSA2" {
		t.Errorf("sign_type param = %q, want RSA2", params["sign_type"])
	}
}

// TestGateway_QueryStatus_TradeClosedWithFullRefund_ReportsRefunded is
// P2-9's regression test: alipay.trade.query reports TRADE_CLOSED for two
// distinct fates -- an unpaid trade closed by timeout, and a PAID trade
// closed by a FULL refund (the query response's own trade_status
// definition, mirroring the notify side's). The poll payload tells them
// apart the same way the P1-4 notify detection does: a closed-by-full-
// refund trade's response carries refund_fee (the refunded amount, a
// decimal yuan string), which a timeout closure never does. QueryStatus
// used to map every TRADE_CLOSED to ChannelStatusFailed, so a fully
// refunded charge was recorded -- by the active-polling fallback, the
// authoritative re-query -- as a failed payment. On pre-fix code this
// response shape reports ChannelStatusFailed; the fix reports
// ChannelStatusRefunded.
func TestGateway_QueryStatus_TradeClosedWithFullRefund_ReportsRefunded(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(url.Values) (int, []byte) {
			body := signAlipayResponse(t, "alipay_trade_query_response", map[string]any{
				"code": "10000", "msg": "Success",
				"trade_status": "TRADE_CLOSED", "total_amount": "29.00", "refund_fee": "29.00",
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
	if status != billing.ChannelStatusRefunded {
		t.Errorf("status = %q, want %q -- a TRADE_CLOSED query response carrying refund_fee is a full refund of a paid trade", status, billing.ChannelStatusRefunded)
	}
	if amount.Cents != 2900 || amount.Currency != "CNY" {
		t.Errorf("amount = %+v", amount)
	}
}

// TestGateway_QueryStatus_TradeClosedWithoutRefundFee_StillFailed pins the
// other half of the P2-9 distinction on the poll side: a trade closed by
// timeout without ever being paid carries no refund_fee and keeps mapping
// to ChannelStatusFailed exactly as before -- and a zero-valued refund_fee
// ("0.00", an explicit no-refund marker) must not be mistaken for a
// refund either.
func TestGateway_QueryStatus_TradeClosedWithoutRefundFee_StillFailed(t *testing.T) {
	for _, refundFee := range []string{"", "0.00"} {
		_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
		cfg := testGatewayConfig(t, alipayPubPEM)

		fields := map[string]any{
			"code": "10000", "msg": "Success",
			"trade_status": "TRADE_CLOSED", "total_amount": "29.00",
		}
		if refundFee != "" {
			fields["refund_fee"] = refundFee
		}
		doer := &fakeDoer{
			respond: func(url.Values) (int, []byte) {
				body := signAlipayResponse(t, "alipay_trade_query_response", fields, alipayPriv)
				return 200, body
			},
		}
		gw, err := newGatewayWithClient(doer, cfg)
		if err != nil {
			t.Fatalf("newGatewayWithClient: %v", err)
		}

		status, _, err := gw.QueryStatus(context.Background(), "ORD1")
		if err != nil {
			t.Fatalf("QueryStatus (refund_fee %q): %v", refundFee, err)
		}
		if status != billing.ChannelStatusFailed {
			t.Errorf("refund_fee %q: status = %q, want %q (an unpaid trade closed by timeout, never a refund)", refundFee, status, billing.ChannelStatusFailed)
		}
	}
}

// TestGateway_QueryStatus_TradeSuccessWithPartialRefundFee_StillSucceeded
// pins the poll-side boundary next to the full-refund case: a PARTIAL
// refund leaves the trade at TRADE_SUCCESS (only a full refund moves the
// order off it, per Alipay's own status definitions), so a query response
// reporting TRADE_SUCCESS alongside a refund_fee is a paid trade that has
// been partially refunded -- still a succeeded collection of the original
// amount, never a refunded order. The P2-9 fix's scope is the TRADE_CLOSED
// full-refund case and nothing beyond it.
func TestGateway_QueryStatus_TradeSuccessWithPartialRefundFee_StillSucceeded(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(url.Values) (int, []byte) {
			body := signAlipayResponse(t, "alipay_trade_query_response", map[string]any{
				"code": "10000", "msg": "Success",
				"trade_status": "TRADE_SUCCESS", "total_amount": "29.00", "refund_fee": "10.00",
			}, alipayPriv)
			return 200, body
		},
	}
	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	status, _, err := gw.QueryStatus(context.Background(), "ORD1")
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if status != billing.ChannelStatusSucceeded {
		t.Errorf("status = %q, want %q (a TRADE_SUCCESS trade with a partial refund_fee is still a succeeded collection)", status, billing.ChannelStatusSucceeded)
	}
}

// TestGateway_CreateCharge_TimestampRenderedInAlipayTimeZone is P2-10's
// regression test: the outgoing request's "timestamp" common parameter used
// to be rendered from time.Now() in the HOST's local time zone. Alipay's
// open-platform gateway interprets the parameter as China Standard Time
// (UTC+8) -- the same fixed zone alipayLocation() already parses Alipay's
// own gmt_* timestamps in -- so a server outside UTC+8 signed and sent a
// timestamp whose wall-clock time was wrong by the host's whole UTC offset.
// The test forces time.Local to UTC (so pre-fix code -- which renders in
// time.Local -- deterministically sends the UTC wall clock on every host,
// not just on non-UTC+8 CI machines), then asserts the timestamp this
// request actually carries parses, in the alipay CST zone, to within a
// small slack of the current instant. Pre-fix code's UTC-rendered
// timestamp, parsed back AS UTC+8 wall time, sits eight hours in the
// future and fails the window; post-fix code renders through
// time.Now().In(alipayLocation()) and passes. time.Local is restored
// before the test returns; no test in this package runs in parallel.
func TestGateway_CreateCharge_TimestampRenderedInAlipayTimeZone(t *testing.T) {
	_, alipayPubPEM, alipayPriv := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	var sentTimestamp string
	doer := &fakeDoer{
		respond: func(form url.Values) (int, []byte) {
			sentTimestamp = form.Get("timestamp")
			body := signAlipayResponse(t, "alipay_trade_precreate_response", map[string]any{
				"code": "10000", "msg": "Success",
				"out_trade_no": "idem-1", "qr_code": "https://qr.alipay.com/fake",
			}, alipayPriv)
			return 200, body
		},
	}

	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	oldLocal := time.Local
	time.Local = time.UTC
	defer func() { time.Local = oldLocal }()

	if _, chargeErr := gw.CreateCharge(context.Background(), billing.ChargeRequest{
		TenantID:       "tenant-a",
		SubscriptionID: "sub-1",
		InvoiceID:      "inv-1",
		Amount:         billing.Money{Cents: 2900, Currency: "CNY"},
		Description:    "Pro plan",
		IdempotencyKey: "idem-1",
	}); chargeErr != nil {
		t.Fatalf("CreateCharge: %v", chargeErr)
	}

	// The sent timestamp must be the CST wall clock: parsed in the alipay
	// zone it must sit at the current instant (two-second slack for the
	// second boundary), never eight hours away.
	sent, err := time.ParseInLocation(alipayTimeFormat, sentTimestamp, alipayLocation())
	if err != nil {
		t.Fatalf("sent timestamp %q does not parse in the alipay timezone: %v", sentTimestamp, err)
	}
	if diff := time.Since(sent); diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("sent timestamp %q is %v from now when parsed as UTC+8 -- the timestamp must be rendered in Alipay's own timezone, never the host's", sentTimestamp, diff)
	}
}

// TestGateway_CreateCharge_RefusesNonPositiveAmount is P3-12's boundary
// regression: Alipay's total_amount must be a positive decimal yuan
// amount, and CreateCharge used to validate nothing -- a zero or negative
// req.Amount.Cents reached formatAmount, which rendered -2950 as the
// garbage string "-29.-50" (Go's %02d of a negative remainder) and 0 as
// "0.00", and sent the result to Alipay as if it were a genuine amount.
// The fix refuses cents <= 0 at the CreateCharge boundary with
// billing.ErrInvalidAmount before anything is formatted or sent. This
// fails on pre-fix code (which reaches the network instead of refusing)
// by asserting the call never reaches doer.Do at all.
func TestGateway_CreateCharge_RefusesNonPositiveAmount(t *testing.T) {
	_, alipayPubPEM, _ := generateTestKeyPair(t)
	cfg := testGatewayConfig(t, alipayPubPEM)

	doer := &fakeDoer{
		respond: func(url.Values) (int, []byte) {
			t.Fatal("CreateCharge reached the network for a non-positive amount; the boundary check should have refused it first")
			return 0, nil
		},
	}
	gw, err := newGatewayWithClient(doer, cfg)
	if err != nil {
		t.Fatalf("newGatewayWithClient: %v", err)
	}

	for _, cents := range []int64{-2950, 0} {
		_, err = gw.CreateCharge(context.Background(), billing.ChargeRequest{
			TenantID:       "tenant-a",
			SubscriptionID: "sub-1",
			InvoiceID:      "inv-1",
			Amount:         billing.Money{Cents: cents, Currency: "CNY"},
			Description:    "Pro plan",
			IdempotencyKey: "idem-1",
		})
		if !hasCode(err, billing.ErrInvalidAmount.Code) {
			t.Errorf("CreateCharge with cents %d: err = %v, want billing.ErrInvalidAmount", cents, err)
		}
	}
}

// TestFormatAmount_Negative_NotGarbled is P3-12's formatting-leg
// regression: formatAmount(cents) used to render a negative input as the
// garbage "-29.-50" (the whole-yuan part "-29", a literal ".", then "%02d"
// of the negative remainder "-50"). The CreateCharge boundary now refuses
// every non-positive amount before formatting is ever reached (see
// TestGateway_CreateCharge_RefusesNonPositiveAmount) -- the actual fix for
// a caller-supplied negative -- and formatAmount itself renders a negative
// input sign-correctly ("-29.50") as a defensive backstop, so no code path
// can ever fabricate "-29.-50" or drop the sign. This fails on the pre-fix
// formatAmount (which returns "-29.-50" for -2950).
func TestFormatAmount_Negative_NotGarbled(t *testing.T) {
	if got := formatAmount(-2950); got != "-29.50" {
		t.Errorf("formatAmount(-2950) = %q, want %q -- never the pre-fix garbage \"-29.-50\", never a sign dropped", got, "-29.50")
	}
	if got := formatAmount(-50); got != "-0.50" {
		t.Errorf("formatAmount(-50) = %q, want %q", got, "-0.50")
	}
	if got := formatAmount(0); got != "0.00" {
		t.Errorf("formatAmount(0) = %q, want 0.00", got)
	}
	if got := formatAmount(2900); got != "29.00" {
		t.Errorf("formatAmount(2900) = %q, want 29.00", got)
	}
}

// TestParseAmount_RefusesNegativeOrSignDropping is P3-12's parsing-leg
// regression: parseAmount("-0.50") used to return 50 -- the sign silently
// dropped, turning a negative wire value into a small positive one -- and
// parseAmount("-29.50") returned -2850, corrupting the sign's arithmetic.
// Amounts Alipay actually sends are always non-negative, so a negative
// string is a protocol anomaly the parser refuses outright rather than
// guessing. This fails on the pre-fix parseAmount (which returns a value
// and nil error for both inputs).
func TestParseAmount_RefusesNegativeOrSignDropping(t *testing.T) {
	for _, s := range []string{"-0.50", "-29.50"} {
		if cents, err := parseAmount(s); err == nil {
			t.Errorf("parseAmount(%q) = %d, nil -- want an error, never a value with the sign dropped or corrupted", s, cents)
		}
	}
	cents, err := parseAmount("29.00")
	if err != nil {
		t.Fatalf("parseAmount(29.00): %v", err)
	}
	if cents != 2900 {
		t.Errorf("parseAmount(29.00) = %d, want 2900", cents)
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
