package alipay

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// alipayLocation is the fixed zone every Alipay timestamp
// (gmt_create/gmt_payment and this package's own "timestamp" request
// param) is rendered in -- China Standard Time, UTC+8, regardless of the
// literal "gmt_" field-name prefix Alipay's own API uses (a long-documented
// naming quirk in Alipay's open-platform API: these fields are NOT actually
// GMT/UTC). Loaded once, since time.LoadLocation performs I/O
// (tzdata lookup) that must not repeat on every parse.
var alipayCST = time.FixedZone("CST", 8*60*60)

func alipayLocation() *time.Location { return alipayCST }

// verifyResponseEnvelope verifies an Alipay open-platform JSON response
// envelope's own "sign" field against the RAW byte substring of
// responseField's value -- Alipay's documented response-verification
// algorithm (https://opendocs.alipay.com/common/02mse3) is byte-sensitive
// to the response object's exact serialization (key order, whitespace,
// escaping) as Alipay itself produced it, so this deliberately verifies
// against the untouched bytes json.RawMessage preserves, never a
// re-serialized/re-ordered reconstruction from a parsed map -- an RSA
// signature over a re-serialized copy would not match Alipay's own
// signature even for a completely unmodified, genuine response, because
// Go's own encoding/json re-serialization is not guaranteed byte-identical
// to whatever Alipay's server originally emitted.
func verifyResponseEnvelope(body []byte, responseField string, pub *rsa.PublicKey) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode response envelope: %w", err)
	}

	respRaw, ok := envelope[responseField]
	if !ok {
		return fmt.Errorf("response envelope has no %q field", responseField)
	}
	signRaw, ok := envelope["sign"]
	if !ok {
		return errors.New("response envelope has no \"sign\" field")
	}
	var sigB64 string
	if err := json.Unmarshal(signRaw, &sigB64); err != nil {
		return fmt.Errorf("decode \"sign\" field: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode sign base64: %w", err)
	}

	digest := sha256.Sum256(respRaw)
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return fmt.Errorf("verify response signature: %w", err)
	}
	return nil
}
