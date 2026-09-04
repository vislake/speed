package wechat

import "crypto/rsa"

// defaultGatewayURL is WeChat Pay's production APIv3 gateway.
const defaultGatewayURL = "https://api.mch.weixin.qq.com"

// Config configures a Gateway over one WeChat Pay merchant account.
type Config struct {
	// MchID is the WeChat Pay merchant id ("mchid" on every request).
	MchID string
	// AppID is the WeChat Open Platform / Official Account appid this
	// merchant's Native payment orders are created under.
	AppID string
	// MchCertSerialNo is the serial number of the merchant's own API
	// certificate -- carried in every outgoing request's Authorization
	// header (the "serial_no" field of the WECHATPAY2-SHA256-RSA2048
	// scheme) so WeChat Pay knows which of the merchant's certificates to
	// verify the request signature against.
	MchCertSerialNo string
	// MchPrivateKeyPEM is the merchant's own RSA private key, PEM-encoded
	// (PKCS#1 or PKCS#8) -- every outgoing request this package makes is
	// signed with it. Never logged, never echoed into an error.
	MchPrivateKeyPEM []byte

	// APIv3Key is the merchant's 32-byte APIv3 key, used to decrypt the
	// AEAD_AES_256_GCM "resource" ciphertext every webhook notification
	// carries. Exactly 32 bytes -- WeChat Pay's own APIv3 key length
	// requirement, since AES-256 keys are always 32 bytes.
	APIv3Key []byte

	// PlatformPublicKeyPEM is WeChat Pay's own platform certificate public
	// key, PEM-encoded (PKIX/SubjectPublicKeyInfo) -- the counterpart
	// VerifySignature checks every inbound notification and every response
	// against. See doc.go's own "Known limitation: static platform
	// certificate" section: this package does not download or rotate it
	// automatically.
	PlatformPublicKeyPEM []byte

	// NotifyURL is the callback URL WeChat Pay POSTs asynchronous payment
	// notifications to -- attached to every CreateCharge call.
	NotifyURL string

	// GatewayURL overrides defaultGatewayURL -- WeChat Pay has no separate
	// sandbox host the way Alipay does (its sandbox uses request-header
	// flagging instead), but this stays overridable for tests. Empty uses
	// defaultGatewayURL.
	GatewayURL string
}

type resolvedKeys struct {
	priv     *rsa.PrivateKey
	platform *rsa.PublicKey
}

func (c Config) parseKeys() (resolvedKeys, error) {
	priv, err := ParsePrivateKeyPEM(c.MchPrivateKeyPEM)
	if err != nil {
		return resolvedKeys{}, err
	}
	pub, err := ParsePublicKeyPEM(c.PlatformPublicKeyPEM)
	if err != nil {
		return resolvedKeys{}, err
	}
	return resolvedKeys{priv: priv, platform: pub}, nil
}

func (c Config) gatewayURL() string {
	if c.GatewayURL != "" {
		return c.GatewayURL
	}
	return defaultGatewayURL
}
