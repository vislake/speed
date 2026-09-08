package tencent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fixedTimestamp is the clock value the vector tests share: unix 1780000000
// (2026-05-28 20:26:40 UTC), whose UTC calendar date (2026-05-28) is what the
// credential scope derives from.
var fixedTimestamp = time.Date(2026, 5, 28, 20, 26, 40, 0, time.UTC)

// vectorPayload is the exact SendSms body the vectors sign -- the JSON
// marshaling of the sendSmsRequest the request tests drive, byte for byte.
func vectorPayload(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(sendSmsRequest{
		PhoneNumberSet:   []string{"+8613800000000"},
		SmsSdkAppID:      "1400006666",
		SignName:         "speed-test",
		TemplateID:       "1234567",
		TemplateParamSet: []string{"your code is 654321"},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

// TestSign_CanonicalRequest_MatchesIndependentVector pins the canonical
// request -- the exact newline layout, header casing and payload digest the
// TC3 specification produces for this adapter's SendSms POST -- against a
// value precomputed by an INDEPENDENT implementation (a python oracle
// re-deriving the specification by hand) rather than by this package's own
// signing code. Any deviation in the layout fails here.
func TestSign_CanonicalRequest_MatchesIndependentVector(t *testing.T) {
	payload := vectorPayload(t)

	const want = "POST\n/\n\ncontent-type:application/json\nhost:sms.tencentcloudapi.com\n\ncontent-type;host\n7e70749845302f973aae91a8203824ec9f60e487c404bdf4a206e8f781acb41e"
	if got := canonicalRequestTC3(gatewayHost, "application/json", payload); got != want {
		t.Errorf("canonicalRequestTC3() =\n%q\nwant\n%q", got, want)
	}
}

// TestSign_StringToSign_MatchesIndependentVector pins the string to sign
// against the independently precomputed value for the same fixed inputs.
func TestSign_StringToSign_MatchesIndependentVector(t *testing.T) {
	canonical := canonicalRequestTC3(gatewayHost, "application/json", vectorPayload(t))

	const want = "TC3-HMAC-SHA256\n1780000000\n2026-05-28/sms/tc3_request\ndce24e3a85656a4d935e632cf123647f378ec9de279cad9ce9ccd6a9d11dd950"
	if got := stringToSignTC3(fixedTimestamp, canonical); got != want {
		t.Errorf("stringToSignTC3() =\n%q\nwant\n%q", got, want)
	}
}

// TestSign_AuthorizationHeader_MatchesIndependentVector pins the whole
// derivation chain -- seeded with "TC3"+SecretKey, through the date, service
// and tc3_request keys -- against the independently precomputed final
// Authorization header, so a wrong key order or a hex-vs-binary mistake in
// the chain fails here.
func TestSign_AuthorizationHeader_MatchesIndependentVector(t *testing.T) {
	payload := vectorPayload(t)

	const want = "TC3-HMAC-SHA256 Credential=TC3-test-id/2026-05-28/sms/tc3_request, SignedHeaders=content-type;host, Signature=fb2b497248a5517203af23cceef807fd13136eeea441a631dbbbc8979ee93dd4"
	got := signTC3("TC3-test-id", "TC3-test-secret", fixedTimestamp, gatewayHost, "application/json", payload)
	if got != want {
		t.Errorf("signTC3() =\n%q\nwant\n%q", got, want)
	}
}

// TestSign_SignedHeaderSet_MatchesTheOfficialSDK pins the signing shape to
// the current official tencentcloud-sdk-go's own: content-type and host only,
// no x-tc-action line in the canonical request (the X-TC-* headers travel
// unsigned in the SDK's signer too -- the set signed is the set verified, so
// a future edit that adds a header to one side but not the other breaks a
// real send, and this test is the tripwire for the shape itself).
func TestSign_SignedHeaderSet_MatchesTheOfficialSDK(t *testing.T) {
	if signedHeaders != "content-type;host" {
		t.Errorf("signedHeaders = %q, want the official SDK's content-type;host", signedHeaders)
	}
	canonical := canonicalRequestTC3(gatewayHost, "application/json", vectorPayload(t))
	if strings.Contains(canonical, "x-tc-action") {
		t.Errorf("canonical request %q must not carry an x-tc-action line -- the official SDK does not sign it", canonical)
	}
	if !strings.Contains(canonical, "content-type:application/json\nhost:sms.tencentcloudapi.com\n") {
		t.Errorf("canonical request %q must carry the content-type and host lines", canonical)
	}
}

// TestCredentialDate_IsUtcRegardlessOfLocalZone proves the credential scope
// date derives from the timestamp in UTC, not the host's local calendar --
// a host east of UTC would otherwise sign a yesterday/today boundary
// timestamp under the wrong date (Tencent's own docs flag this exact bug
// class).
func TestCredentialDate_IsUtcRegardlessOfLocalZone(t *testing.T) {
	// 23:30 on 2026-05-28 UTC is 07:30 on 2026-05-29 in UTC+8.
	ts := time.Date(2026, 5, 28, 23, 30, 0, 0, time.UTC)
	if got := credentialDate(ts); got != "2026-05-28" {
		t.Errorf("credentialDate(23:30Z) = %q, want the UTC date 2026-05-28 regardless of local zone", got)
	}
}
