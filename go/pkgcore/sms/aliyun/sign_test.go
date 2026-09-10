package aliyun

import (
	"net/http"
	"testing"
)

// TestSign_RPCAliyunDocumentWorkedExample pins the whole RPC signing
// mechanism against the worked example in Aliyun's own signature
// documentation (help.aliyun.com, "RPC mechanism" -- the DescribeDedicatedHosts
// request signed with AccessKeyId testid / secret testsecret, nonce
// edb2b34af0af9a6d14deaf7c1a5315eb, timestamp 2023-03-13T08:34:30Z). The
// expected canonicalized query string, the string to sign and the base64
// signature in that documentation are reproduced below; any deviation in
// sort order, percent-encoding or key derivation fails here with the value
// Aliyun's own documentation fixes.
func TestSign_RPCAliyunDocumentWorkedExample(t *testing.T) {
	params := map[string]string{
		"AccessKeyId":      "testid",
		"Action":           "DescribeDedicatedHosts",
		"Format":           "JSON",
		"RegionId":         "cn-beijing",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   "edb2b34af0af9a6d14deaf7c1a5315eb",
		"SignatureVersion": "1.0",
		"Timestamp":        "2023-03-13T08:34:30Z",
		"Version":          "2014-05-26",
	}

	const wantCanonical = "AccessKeyId=testid&Action=DescribeDedicatedHosts&Format=JSON&RegionId=cn-beijing&SignatureMethod=HMAC-SHA1&SignatureNonce=edb2b34af0af9a6d14deaf7c1a5315eb&SignatureVersion=1.0&Timestamp=2023-03-13T08%3A34%3A30Z&Version=2014-05-26"
	if got := canonicalQueryString(params); got != wantCanonical {
		t.Errorf("canonicalQueryString() =\n%s\nwant\n%s", got, wantCanonical)
	}

	// The signature is base64(HMAC-SHA1) over the string-to-sign
	// "GET&%2F&" + percentEncode(canonical), keyed with secret + "&".
	if got := rpcSignature("testsecret", http.MethodGet, wantCanonical); got != "9NaGiOspFP5UPcwX8Iwt2YJXXuk=" {
		t.Errorf("rpcSignature() = %q, want the documentation's own 9NaGiOspFP5UPcwX8Iwt2YJXXuk=", got)
	}
}

// TestSign_SendSmsShape_SignatureMatchesIndependentVector pins a
// SendSms-shaped signature (the parameter set Send actually builds for the
// mapped template -- the template's own code, and a TemplateParam carrying
// its declared variables, with a fixed clock and nonce) against a value
// precomputed by an INDEPENDENT implementation (a python oracle re-deriving
// the specification by hand: canonical query with quote(safe='~') and the
// compact JSON object json.dumps produces) rather than by this package's own
// signing code, so a bug shared between canonicalization and the test cannot
// cancel out. The fixed inputs mirror aliyun_test.go's offline send.
func TestSign_SendSmsShape_SignatureMatchesIndependentVector(t *testing.T) {
	params := map[string]string{
		"AccessKeyId":      "LTAI-test-id",
		"Action":           apiAction,
		"Format":           "JSON",
		"PhoneNumbers":     "+8613800000000",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   "fixed-nonce-0000000000000000",
		"SignatureVersion": "1.0",
		"SignName":         "speed-test",
		"TemplateCode":     "SMS_0000001",
		"TemplateParam":    `{"code":"654321","minutes":"5"}`,
		"Timestamp":        "2026-09-08T02:00:00Z",
		"Version":          "2017-05-25",
	}

	if got := rpcSignature("LTAI-test-secret", http.MethodPost, canonicalQueryString(params)); got != "g1DBjm3d8FbyqTHtZ9qEnjp9Kpg=" {
		t.Errorf("rpcSignature() = %q, want the independently precomputed g1DBjm3d8FbyqTHtZ9qEnjp9Kpg=", got)
	}
}

// TestRfc3986Encode pins the encoding rules the canonical form depends on:
// space becomes %20 (never '+'), unreserved characters stay verbatim, '*' is
// encoded, and a non-ASCII byte sequence is encoded bytewise. These are the
// exact rows of Aliyun's documented encoding table.
func TestRfc3986Encode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "A-Za-z0-9-_.~", want: "A-Za-z0-9-_.~"},
		{in: " ", want: "%20"},
		{in: "a+b", want: "a%2Bb"},
		{in: "a*b", want: "a%2Ab"},
		{in: "2023-03-13T08:34:30Z", want: "2023-03-13T08%3A34%3A30Z"},
		{in: `{"code":"` + string([]byte{0xE6, 0x82, 0xA8}), want: `%7B%22code%22%3A%22%E6%82%A8`}, // non-ASCII (U+60A8) encoded bytewise
	}
	for _, tc := range tests {
		if got := rfc3986Encode(tc.in); got != tc.want {
			t.Errorf("rfc3986Encode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
