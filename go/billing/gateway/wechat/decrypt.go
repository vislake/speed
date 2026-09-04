package wechat

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
)

// algorithmAEADAES256GCM is the only "algorithm" value WeChat Pay's own
// resource envelope ever carries today -- checked defensively so a future
// algorithm change fails loudly (ErrWebhookPayloadUnrecognized) rather than
// being silently decrypted with the wrong cipher.
const algorithmAEADAES256GCM = "AEAD_AES_256_GCM"

// decryptResource decrypts one WeChat Pay APIv3 "resource" envelope's
// ciphertext with apiV3Key (the merchant's 32-byte APIv3 key,
// https://pay.weixin.qq.com/doc/v3/merchant/4012153196's own documented
// AEAD_AES_256_GCM scheme): AES-256 in GCM mode, the 12-byte nonce and
// associated-data string carried alongside the ciphertext in the envelope
// itself, and the 16-byte GCM authentication tag appended to the
// ciphertext's own tail -- exactly the shape Go's own
// cipher.AEAD.Open expects as a single combined slice, so no manual tag
// splitting is needed.
func decryptResource(algorithm, nonce, associatedData, ciphertextB64 string, apiV3Key []byte) ([]byte, error) {
	if algorithm != algorithmAEADAES256GCM {
		return nil, fmt.Errorf("billing/gateway/wechat: unsupported resource algorithm %q", algorithm)
	}
	if len(apiV3Key) != 32 {
		return nil, fmt.Errorf("billing/gateway/wechat: APIv3Key must be exactly 32 bytes, got %d", len(apiV3Key))
	}

	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/wechat: decode resource ciphertext: %w", err)
	}

	block, err := aes.NewCipher(apiV3Key)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/wechat: new AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("billing/gateway/wechat: new GCM: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("billing/gateway/wechat: resource nonce is %d bytes, want %d", len(nonce), gcm.NonceSize())
	}

	plaintext, err := gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
	if err != nil {
		// A wrong APIv3Key, a tampered ciphertext, or a tampered
		// associated_data/nonce all fail here identically -- GCM's
		// authentication tag check is what makes this decryption also a
		// second, independent integrity proof beneath the outer RSA
		// signature (VerifySignature): a forged notification would have to
		// forge BOTH the outer signature AND this AEAD tag.
		return nil, fmt.Errorf("billing/gateway/wechat: decrypt resource: %w", err)
	}
	return plaintext, nil
}
