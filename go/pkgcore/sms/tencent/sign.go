package tencent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// tc3Algorithm is the fixed algorithm name of Tencent Cloud's TC3 signature.
const tc3Algorithm = "TC3-HMAC-SHA256"

// serviceName is the Tencent Cloud service this adapter calls ("sms" --
// the service name participates in the TC3 credential scope and key
// derivation, so it must be the literal service identifier, not the product
// display name).
const serviceName = "sms"

// tc3Request is the fixed final segment of every TC3 credential scope.
const tc3Request = "tc3_request"

// signedHeaders lists the canonical header names the TC3 signature covers.
// It is exactly the set the current official tencentcloud-sdk-go signs
// (content-type and host only; the X-TC-* headers travel unsigned, bound to
// the request by TLS rather than by the signature) -- SDK parity, because
// that signing shape is the one exercised against Tencent's real gateway by
// every production SDK customer.
const signedHeaders = "content-type;host"

// sha256Hex is the lowercase hex SHA-256 digest of b -- the HashedRequestPayload
// form and every other digest the TC3 algorithm signs over.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hmacSHA256 computes the binary HMAC-SHA256 of data under key -- TC3
// derives each next key from the previous one's RAW digest, never from a hex
// rendering, so the intermediate keys stay binary.
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

// credentialDate renders the UTC calendar date of t in the yyyy-MM-dd form
// the TC3 credential scope and key derivation fix. It must be derived from
// the timestamp in UTC -- a host in another time zone must not skew it --
// exactly as the X-TC-Timestamp the date is derived from.
func credentialDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// credentialScope is the date/service/tc3_request scope of t.
func credentialScope(t time.Time) string {
	return credentialDate(t) + "/" + serviceName + "/" + tc3Request
}

// canonicalRequestTC3 builds the canonical request Tencent's TC3
// specification fixes for this adapter's POST:
//
//	POST + "\n" + "/" + "\n" + "" (no query string) + "\n"
//	+ the content-type and host canonical headers + "\n"
//	+ the signed header names + "\n" + the payload's SHA-256
//
// Content-Type and host must byte-for-byte equal what the request actually
// sends; Tencent verifies the signed headers against the received request.
func canonicalRequestTC3(host, contentType string, payload []byte) string {
	canonicalHeaders := "content-type:" + contentType + "\n" +
		"host:" + host + "\n"
	return strings.Join([]string{
		http.MethodPost,
		"/",
		"",
		canonicalHeaders,
		signedHeaders,
		sha256Hex(payload),
	}, "\n")
}

// stringToSignTC3 binds the algorithm, the exact Unix timestamp (which must
// equal the X-TC-Timestamp header), the credential scope and the canonical
// request's digest into the string the signature is computed over.
func stringToSignTC3(t time.Time, canonicalRequest string) string {
	return strings.Join([]string{
		tc3Algorithm,
		strconv.FormatInt(t.Unix(), 10),
		credentialScope(t),
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
}

// signTC3 implements Tencent Cloud's TC3-HMAC-SHA256 signature for this
// adapter's POST: the key derivation chain HMAC-SHA256s each previous BINARY
// key with the next scope segment, seeded with "TC3"+SecretKey, and the final
// signature is the hex digest over the string to sign.
func signTC3(secretID, secretKey string, t time.Time, host, contentType string, payload []byte) string {
	canonicalRequest := canonicalRequestTC3(host, contentType, payload)
	stringToSign := stringToSignTC3(t, canonicalRequest)

	secretDate := hmacSHA256([]byte("TC3"+secretKey), credentialDate(t))
	secretService := hmacSHA256(secretDate, serviceName)
	secretSigning := hmacSHA256(secretService, tc3Request)
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	return fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		tc3Algorithm, secretID, credentialScope(t), signedHeaders, signature)
}
