// This file holds the zero-setup development keys the app falls back to when
// neither the matching environment variable nor APP_ROOT_KEY supplies key
// material, and BootstrapDevDefaults, which assembles them into the declared
// defaults table the loader is given.

package app

// DevConfigKey is the cipher key config's config.cipher_key material falls
// back to when neither APP_CONFIG__CIPHER_KEY nor APP_ROOT_KEY supplies one.
// It is the ascending 0x00..0x1f byte sequence -- a recognizable constant,
// never a secret -- because zero-setup standalone development (`go run
// ./cmd/server`, `task dev`, this app's tests) must work with no environment
// at all, while config's Sensitive items demand a real 32-byte key the moment
// one is declared (Attach fails with ErrCipherRequired otherwise).
//
// This default is a documented trade-off, not a pattern to copy: it is a key
// committed to the repository, which real hosts must never do. A real
// deployment must set the material from a secret store (or refuse to start);
// the constant exists so the *demo* keeps working out of the box, and its name
// and doc comment are the guard rails that keep it honest.
var DevConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// DevOrgIndexKey is the HMAC key org's org.invitation_email_index_key material
// falls back to -- the descending 0xff..0xe0 byte sequence, chosen precisely
// so it is visibly a DIFFERENT 32 bytes from DevConfigKey's ascending
// 0x00..0x1f, because the two materials must never be the same secret: this
// app reuses the config cipher to also encrypt org's Invitation.Email column
// (registered through org.RegisterEmailSerializer), and dbkit's own rule is
// that an AES key must never double as an HMAC key. Like DevConfigKey, this is
// a recognizable constant for zero-setup standalone development, never a
// secret a real deployment should keep; an invitation whose address cannot be
// indexed can never be found again, so a real material must not change between
// restarts either.
var DevOrgIndexKey = []byte{
	0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8,
	0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0,
	0xef, 0xee, 0xed, 0xec, 0xeb, 0xea, 0xe9, 0xe8,
	0xe7, 0xe6, 0xe5, 0xe4, 0xe3, 0xe2, 0xe1, 0xe0,
}

// DevPKILocalKeyCipherKey, DevBlindIndexKey and DevPIICipherKey are authn's
// (and pki's) own committed-key placeholders, the same documented trade-off as
// DevConfigKey above -- a real deployment must replace every one of them with
// real secret-manager material, never commit real keys the way this demo
// commits these. Each has a real override path (APP_PKI__LOCAL_KEY_CIPHER_KEY,
// APP_AUTHN__BLIND_INDEX_KEY and APP_AUTHN__PII_CIPHER_KEY, the declaration's
// pki and authn fields; or APP_ROOT_KEY deriving all six materials at once --
// see loadHostConfig's own doc comment (bootstrap.go) for the root-key wiring
// and the three-tier precedence it applies), and each is the package-level
// default the loader leaves standing as its lowest-priority source -- never a
// direct environment read from BuildServer.
//
// Each protects something different and each MUST stay stable across restarts
// for a different reason: DevPKILocalKeyCipherKey seals go/pki's LocalSigner
// private-key column (pki_local_keys, via pki.RegisterLocalKeySerializer) --
// the signing key itself is generated once by pki.Service.EnsurePurpose
// (authn is constructed with pki's service as its key source, modules.go) and
// PERSISTS in cfg.SQLitePath across restarts; DevBlindIndexKey indexes
// users.email_index/phone_index through authn.WithBlindIndexKey's indexer and
// must stay IDENTICAL across restarts, or every already-stored blind index
// becomes unfindable; and DevPIICipherKey seals authn's encrypted PII columns
// (email, phone, TOTP secrets) via authn.RegisterPIISerializer, deliberately a
// DIFFERENT key from DevConfigKey -- dbkit's key-separation rule (never let
// one key double as two different AEAD constructions) applies across modules,
// not only within one.
var (
	DevPKILocalKeyCipherKey = []byte{
		0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
		0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f,
		0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
		0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
	}
	DevBlindIndexKey = []byte{
		0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47,
		0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f,
		0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57,
		0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f,
	}
	DevPIICipherKey = []byte{
		0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
		0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77,
		0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f,
	}
)

// DevNotificationIndexKey is the HMAC key notification's
// notification.contact_index_key material falls back to -- the ascending
// 0x80..0x9f byte sequence, the next free 32-byte region of this file's
// recognizable constants and chosen so it is visibly a DIFFERENT 32 bytes from
// every key above it, because the contact-index key and the config cipher key
// must never be the same secret: this app reuses the config cipher to also
// encrypt notification's Contact.Address column (registered through
// notification.RegisterContactAddressSerializer), and an AES key must never
// double as an HMAC key. One HMAC key serves both the email and the phone
// indexers (notification.NewContactEmailIndexer/NewContactPhoneIndexer), whose
// normalizers keep the two index columns' inputs in disjoint canonical forms.
// Like its siblings, this is a recognizable constant for zero-setup standalone
// development, never a secret a real deployment should keep, and a real
// material must not change between restarts.
var DevNotificationIndexKey = []byte{
	0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
	0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
	0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97,
	0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f,
}

// BootstrapDevDefaults returns this file's six development defaults as the
// declared defaults table: the values the assembly's declared key materials
// fall back to when neither their own variables nor APP_ROOT_KEY supply
// material. The keys are the declared key paths of the composed modules'
// components -- each module declares its own, so this table carries values
// only, no declaration -- and the table rides the loader options every
// assembly pass reads (assemble's LoadSpec.Options), which is what keeps the
// host's own pre-assembly pass and the engine's pass over the same sources.
func BootstrapDevDefaults() map[string][]byte {
	return map[string][]byte{
		"authn.blind_index_key":          DevBlindIndexKey,
		"authn.pii_cipher_key":           DevPIICipherKey,
		"config.cipher_key":              DevConfigKey,
		"notification.contact_index_key": DevNotificationIndexKey,
		"org.invitation_email_index_key": DevOrgIndexKey,
		"pki.local_key_cipher_key":       DevPKILocalKeyCipherKey,
	}
}
