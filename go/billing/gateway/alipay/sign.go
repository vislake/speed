package alipay

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// signContent builds Alipay's canonical string-to-sign for VERIFYING an
// inbound async notification
// (https://opendocs.alipay.com/common/02kdnc, its verify section): every
// param whose key is neither "sign" nor "sign_type", and whose value is
// non-empty, sorted ascending by key (byte-wise, which for an
// all-ASCII-parameter-name API is the same as Alipay's own dictionary
// order), joined as "key1=value1&key2=value2&...&keyN=valueN" using each
// value's raw (already URL-decoded) form -- never the URL-encoded wire
// form.
//
// This canonical form is deliberately NOT the one used for signing this
// package's own outgoing requests: Alipay's documented parameter sets for
// the two directions are ASYMMETRIC, and conflating them (as an earlier
// revision of this package did, using one function for both) signed every
// outgoing request over a string missing a parameter Alipay's own server
// includes when it verifies -- a guaranteed signature mismatch on the
// first real call. The verified asymmetry, from the doc cited above and
// Alipay's own reference SDKs (the official
// alipay-sdk-java-all's AlipaySignature: request signing goes through
// getSignContent, which removes nothing but empty values, while inbound
// verification goes through getSignCheckContentV1, which removes both
// "sign" and "sign_type"):
//
//   - OUTGOING REQUEST signing (requestSignContent below): the canonical
//     string is every non-empty request parameter EXCEPT "sign" itself --
//     which does not exist yet at signing time, it is added only after the
//     signature is computed. "sign_type" PARTICIPATES: the string a
//     request is signed over includes "sign_type=RSA2", and Alipay's
//     gateway includes it too when it verifies the request.
//   - INBOUND NOTIFICATION verification (this function): the canonical
//     string excludes "sign" AND "sign_type" -- Alipay's own server signs
//     its notifications over that reduced set, and this package verifies
//     the identical reduced set.
//
// verifySignature with both names present therefore covers a parameter set
// a real Alipay notification carries, and only that.
func signContent(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" || k == "sign_type" {
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
	return b.String()
}

// requestSignContent builds Alipay's canonical string-to-sign for an
// OUTGOING request -- the asymmetric counterpart of signContent, per the
// verified parameter-participation rules signContent's own doc comment
// states (https://opendocs.alipay.com/common/02kdnc's own
// implement-signing-yourself section and Alipay's own reference SDKs):
// every non-empty request parameter
// except "sign" itself, which is only added to the parameter set AFTER the
// signature is computed. "sign_type" is deliberately NOT excluded -- it is
// a request parameter like any other and participates in the string,
// exactly as Alipay's gateway includes it when it verifies the request. An
// implementation that signs requests with the notify-side canonical form
// (sign_type excluded) produces a signature Alipay's server rejects.
func requestSignContent(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" {
			// Not present at signing time in this package's own flow (call()
			// adds it after signing); excluded defensively all the same.
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
	return b.String()
}

// signParams RSA2-signs params (Alipay's own canonical form for an
// outgoing REQUEST -- requestSignContent above, which includes sign_type)
// with priv and returns the base64-encoded signature -- the value Alipay's
// own "sign" request parameter carries, and what CreateCharge attaches to
// every outgoing alipay.trade.* call.
func signParams(params map[string]string, priv *rsa.PrivateKey) (string, error) {
	digest := sha256.Sum256([]byte(requestSignContent(params)))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("billing/gateway/alipay: sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifySignature verifies params' own "sign" value (base64-encoded RSA2
// signature) against Alipay's canonical string built from every OTHER
// param, using pub -- Alipay's own public key, never the merchant's. It
// performs no network call and no I/O of any kind, matching every other
// provider package's VerifyWebhook's own offline-verification contract.
func VerifySignature(params map[string]string, pub *rsa.PublicKey) error {
	sigB64 := params["sign"]
	if sigB64 == "" {
		return errors.New("billing/gateway/alipay: verify signature: no \"sign\" parameter")
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("billing/gateway/alipay: verify signature: decode sign: %w", err)
	}

	digest := sha256.Sum256([]byte(signContent(params)))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return fmt.Errorf("billing/gateway/alipay: verify signature: %w", err)
	}
	return nil
}

// ParsePublicKeyPEM parses a PEM-encoded RSA public key -- the form
// Alipay's open-platform console hands out as the platform's own Alipay
// public key, used to verify inbound notifications.
func ParsePublicKeyPEM(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("billing/gateway/alipay: parse public key: no PEM block found")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/alipay: parse public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("billing/gateway/alipay: parse public key: got %T, want *rsa.PublicKey", pub)
	}
	return rsaPub, nil
}

// ParsePrivateKeyPEM parses a PEM-encoded RSA private key in PKCS#1 or
// PKCS#8 form -- either is common among Alipay open-platform key-generation
// tools, so both are accepted.
func ParsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("billing/gateway/alipay: parse private key: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/alipay: parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("billing/gateway/alipay: parse private key: got %T, want *rsa.PrivateKey", key)
	}
	return rsaKey, nil
}

// decodeFormValues parses an application/x-www-form-urlencoded notify body
// into a flat map[string]string -- Alipay delivers exactly one value per
// key, so the []string url.Values shape is flattened to its first (only)
// element per key.
func decodeFormValues(body []byte) (map[string]string, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/alipay: parse notify body: %w", err)
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out, nil
}
