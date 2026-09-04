package alipay

import "crypto/rsa"

// defaultGatewayURL is Alipay's production open-platform gateway. A
// sandbox integration overrides Config.GatewayURL with Alipay's sandbox
// endpoint instead.
const defaultGatewayURL = "https://openapi.alipay.com/gateway.do"

// Config configures a Gateway over one Alipay open-platform application.
type Config struct {
	// AppID is the Alipay open-platform application id ("appid" on every
	// request).
	AppID string

	// PrivateKeyPEM is the merchant's own RSA2 private key, PEM-encoded
	// (PKCS#1 or PKCS#8) -- every outgoing request CreateCharge/QueryStatus
	// makes is signed with it. Never logged, never echoed into an error.
	PrivateKeyPEM []byte

	// AlipayPublicKeyPEM is Alipay's own RSA public key, PEM-encoded
	// (PKIX/SubjectPublicKeyInfo) -- the counterpart VerifySignature checks
	// every inbound notification and every response envelope against.
	AlipayPublicKeyPEM []byte

	// NotifyURL is the callback URL Alipay POSTs asynchronous trade
	// notifications to -- attached to every CreateCharge call.
	NotifyURL string

	// GatewayURL overrides defaultGatewayURL -- set to Alipay's sandbox
	// endpoint for a non-production integration. Empty uses
	// defaultGatewayURL.
	GatewayURL string
}

// resolvedKeys is Config's two parsed RSA keys, computed once by
// NewGateway.
type resolvedKeys struct {
	priv *rsa.PrivateKey
	pub  *rsa.PublicKey
}

func (c Config) parseKeys() (resolvedKeys, error) {
	priv, err := ParsePrivateKeyPEM(c.PrivateKeyPEM)
	if err != nil {
		return resolvedKeys{}, err
	}
	pub, err := ParsePublicKeyPEM(c.AlipayPublicKeyPEM)
	if err != nil {
		return resolvedKeys{}, err
	}
	return resolvedKeys{priv: priv, pub: pub}, nil
}

func (c Config) gatewayURL() string {
	if c.GatewayURL != "" {
		return c.GatewayURL
	}
	return defaultGatewayURL
}
