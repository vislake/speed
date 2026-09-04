package wechat

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
	mathrand "math/rand"
	"strconv"
	"strings"
	"time"
)

// requestSignMessage builds the message WeChat Pay's APIv3
// WECHATPAY2-SHA256-RSA2048 Authorization scheme signs for an OUTGOING
// request: method + "\n" + canonicalURL + "\n" + timestamp + "\n" + nonce +
// "\n" + body + "\n" -- https://pay.weixin.qq.com/doc/v3/merchant/4012774368,
// its build-the-signing-string section. canonicalURL is the request path
// plus query string only (no scheme/host).
func requestSignMessage(method, canonicalURL, timestamp, nonce, body string) string {
	return strings.Join([]string{method, canonicalURL, timestamp, nonce, body, ""}, "\n")
}

// notifySignMessage builds the message WeChat Pay signs for an INBOUND
// webhook notification or an API RESPONSE: timestamp + "\n" + nonce + "\n"
// + body + "\n" -- same documented section, its own signature-verification
// subsection for the receiving side, which omits method and URL entirely
// (those exist only on the requester's side of an exchange).
func notifySignMessage(timestamp, nonce, body string) string {
	return strings.Join([]string{timestamp, nonce, body, ""}, "\n")
}

// signRequest RSA-SHA256-signs message with priv (PKCS1v15 padding, per
// WeChat Pay's own documented algorithm) and returns the base64-encoded
// signature -- the "signature" field of the Authorization header.
func signRequest(message string, priv *rsa.PrivateKey) (string, error) {
	digest := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("billing/gateway/wechat: sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifySignature verifies a base64-encoded RSA-SHA256 signature over
// timestamp+"\n"+nonce+"\n"+body+"\n" against pub -- WeChat Pay's own
// platform public key, never the merchant's. Used identically for an
// inbound webhook notification's Wechatpay-Signature header and an API
// response's own signature header, since both sides sign the identical
// message shape (notifySignMessage). Performs no network call and no I/O
// of any kind.
func VerifySignature(timestamp, nonce string, body []byte, sigB64 string, pub *rsa.PublicKey) error {
	if sigB64 == "" {
		return errors.New("billing/gateway/wechat: verify signature: empty signature")
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("billing/gateway/wechat: verify signature: decode signature: %w", err)
	}

	message := notifySignMessage(timestamp, nonce, string(body))
	digest := sha256.Sum256([]byte(message))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return fmt.Errorf("billing/gateway/wechat: verify signature: %w", err)
	}
	return nil
}

// authorizationHeader builds the full WECHATPAY2-SHA256-RSA2048
// Authorization header value for one outgoing request.
func authorizationHeader(cfg Config, priv *rsa.PrivateKey, method, canonicalURL string, body []byte) (string, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := generateNonce()

	message := requestSignMessage(method, canonicalURL, timestamp, nonce, string(body))
	sig, err := signRequest(message, priv)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(
		`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",timestamp="%s",serial_no="%s",signature="%s"`,
		cfg.MchID, nonce, timestamp, cfg.MchCertSerialNo, sig,
	), nil
}

// nonceAlphabet is the character set generateNonce draws from -- an
// ordinary alphanumeric nonce, matching WeChat Pay's own documented
// nonce_str shape (no requirement beyond "a random string").
const nonceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generateNonce returns a 32-character random nonce. It uses
// math/rand/v2-backed math/rand (auto-seeded since Go 1.20) rather than
// crypto/rand: unlike the RSA signature itself, the nonce's only job is
// avoiding an accidental collision, not resisting a determined adversary --
// WeChat Pay's own replay protection is the (timestamp, nonce) pair
// together with its own tolerance window, not the nonce's
// unpredictability alone.
func generateNonce() string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = nonceAlphabet[mathrand.Intn(len(nonceAlphabet))] //nolint:gosec // see doc comment: unpredictability is not the safety property here
	}
	return string(b)
}

// ParsePrivateKeyPEM parses a PEM-encoded RSA private key in PKCS#1 or
// PKCS#8 form -- either is common among WeChat Pay merchant-key exports.
func ParsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("billing/gateway/wechat: parse private key: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/wechat: parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("billing/gateway/wechat: parse private key: got %T, want *rsa.PrivateKey", key)
	}
	return rsaKey, nil
}

// ParsePublicKeyPEM parses a PEM-encoded RSA public key, or extracts the
// public key from a PEM-encoded X.509 certificate -- WeChat Pay's own
// platform certificate download endpoint returns the latter, so both forms
// are accepted for the caller's convenience.
func ParsePublicKeyPEM(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("billing/gateway/wechat: parse public key: no PEM block found")
	}

	if block.Type == "CERTIFICATE" {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("billing/gateway/wechat: parse certificate: %w", err)
		}
		rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("billing/gateway/wechat: parse certificate: public key is %T, want *rsa.PublicKey", cert.PublicKey)
		}
		return rsaPub, nil
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/wechat: parse public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("billing/gateway/wechat: parse public key: got %T, want *rsa.PublicKey", pub)
	}
	return rsaPub, nil
}
