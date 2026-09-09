package aliyun

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- Aliyun's RPC signature specification fixes HMAC-SHA1 as the only permitted signature algorithm; the key is an access-key secret and the digest choice is the vendor's contract, not a password hash.
	"encoding/base64"
	"sort"
	"strings"
)

// rfc3986Encode percent-encodes s with the exact rule Aliyun's RPC signature
// specification fixes: every byte outside the RFC 3986 unreserved set
// (A-Z a-z 0-9 - _ . ~) becomes %XX with uppercase hex, and space becomes
// %20, never '+'. Encoding is bytewise, so multi-byte UTF-8 sequences are
// each encoded per byte as the specification requires. This deliberately does
// NOT use url.QueryEscape, whose space-as-'+' and unreserved handling differ.
func rfc3986Encode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// canonicalQueryString builds the canonicalized query string Aliyun's RPC
// signature signs over: every parameter sorted by key in ascending byte
// order, each key and value percent-encoded with rfc3986Encode, joined as
// key=value with '&'. The Signature parameter itself must not be present --
// sign.go's caller computes the canonical form before adding it.
func canonicalQueryString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, rfc3986Encode(k)+"="+rfc3986Encode(params[k]))
	}
	return strings.Join(pairs, "&")
}

// rpcSignature computes the RPC signature for method over canonical:
//
//	stringToSign = method + "&" + "%2F" + "&" + rfc3986Encode(canonical)
//	signature    = base64(HMAC-SHA1(AccessKeySecret + "&", stringToSign))
//
// per Aliyun's RPC signature specification (the "/" of the canonical URI is
// percent-encoded once as %2F, and the whole canonicalized query string is
// percent-encoded a second time inside stringToSign).
func rpcSignature(secret, method, canonical string) string {
	stringToSign := method + "&%2F&" + rfc3986Encode(canonical)
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	_, _ = mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
