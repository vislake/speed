package dbkit

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm/schema"
)

// encryptionKeySize is the required key length in bytes for AES-256-GCM: 256
// bits.
const encryptionKeySize = 32

// ErrInvalidKeySize is returned by NewCipher and NewBlindIndexer when a key
// is not exactly encryptionKeySize bytes long. AES-256 has no shorter or
// longer key, and the blind-index HMAC keys share the same 32-byte policy,
// so there is no lenient fallback for either: a key of the wrong length is
// always a configuration mistake, never silently downgraded.
var ErrInvalidKeySize = errors.New("dbkit: key must be exactly 32 bytes (AES-256-GCM encryption keys and blind-index HMAC keys are 256-bit secrets)")

// ErrDecryptionFailed is returned by (*Cipher).Decrypt when ciphertext cannot
// be authenticated and opened under the active key or any retired key.
// Callers should not try to distinguish "wrong key" from "corrupted or
// tampered ciphertext" from this error: AES-GCM deliberately reveals nothing
// more specific than "not authentic" for either case.
var ErrDecryptionFailed = errors.New("dbkit: ciphertext could not be decrypted with the active key or any retired key")

// Cipher performs authenticated, randomized field-level encryption with
// AES-256-GCM, for the sensitive columns described in
// docs/internal/10-compliance-and-audit.md (national ID numbers, phone
// numbers, health-related free text, and similar).
//
// A Cipher holds exactly one active key, used for every new Encrypt call, and
// zero or more retired keys kept only so Decrypt can still read data written
// before a key rotation. Rotate by constructing a new Cipher whose active key
// is the new key and whose retired keys include the previous active key (see
// NewCipher and Decrypt); existing rows keep decrypting correctly until a
// normal application write re-encrypts them under the new active key. A bulk
// re-encryption of every row is a deliberate, separate jobs task, not
// something rotation does implicitly.
//
// A Cipher's key material is reachable only through Encrypt and Decrypt: the
// type exposes no accessor that returns the raw key bytes back out. That is
// deliberate, not an oversight — see BlindIndex for why the encryption key
// and a blind-index key must never be the same secret, and note that this
// design makes it structurally impossible to derive a "blind index key" from
// a *Cipher even by accident, because there is nothing exposed to derive it
// from.
//
// The zero Cipher is not valid; construct one with NewCipher. A *Cipher is
// safe for concurrent use by multiple goroutines: Encrypt and Decrypt only
// read the keys set up at construction time.
type Cipher struct {
	active  cipher.AEAD
	retired []cipher.AEAD
}

// NewCipher builds a Cipher whose active key is activeKey and whose retired
// keys are retiredKeys, tried by Decrypt in the order given. Every key,
// active or retired, must be exactly 32 bytes; NewCipher returns an error
// wrapping ErrInvalidKeySize otherwise, rather than let a wrong-length key
// silently produce a weaker or non-functional cipher.
//
// When rotating keys, pass the previous active key as (one of) the retired
// keys, so data encrypted before the rotation keeps decrypting; see Decrypt.
//
// activeKey and every entry in retiredKeys must be secrets used for nothing
// else, and in particular must never be reused as the key passed to
// BlindIndex. Mixing an encryption key and an HMAC blind-index key is a real
// cryptographic weakness, not a style nitpick: it couples the security
// analysis of two constructions — AES-GCM confidentiality and an HMAC used as
// a deterministic index — that were never designed to share key material,
// and a deterministic index is, by construction, exposing structured
// information (equality) about the plaintext that encryption is meant to
// hide. Keep the encryption key(s) and the blind-index key as separate
// entries in your secret manager and inject them separately.
func NewCipher(activeKey []byte, retiredKeys ...[]byte) (*Cipher, error) {
	active, err := newAEAD(activeKey)
	if err != nil {
		return nil, fmt.Errorf("dbkit: active key: %w", err)
	}

	retired := make([]cipher.AEAD, 0, len(retiredKeys))
	for i, key := range retiredKeys {
		aead, err := newAEAD(key)
		if err != nil {
			return nil, fmt.Errorf("dbkit: retired key %d: %w", i, err)
		}
		retired = append(retired, aead)
	}

	return &Cipher{active: active, retired: retired}, nil
}

// newAEAD validates key's length and wraps it as an AES-256-GCM AEAD.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != encryptionKeySize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("dbkit: build AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// Encrypt seals plaintext under c's active key and returns the ciphertext
// with a freshly generated random nonce prepended.
//
// Every call draws its own nonce from crypto/rand, so two calls with
// identical plaintext never produce identical ciphertext. This matters
// beyond appearances: reusing a nonce under the same AES-GCM key is a
// catastrophic break of confidentiality (it lets an attacker recover the
// XOR of the two plaintexts and forge subsequent messages), not a cosmetic
// property, which is why the nonce is generated here rather than accepted
// as a parameter that a caller could get wrong.
//
// The returned bytes are opaque and self-contained (nonce plus sealed data
// plus authentication tag); store them as-is, for example in a BLOB/bytea
// column, and pass them back to Decrypt unmodified. Encrypt never consults
// the retired keys — only Decrypt does.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.active.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("dbkit: generate nonce: %w", err)
	}
	return c.active.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt. It first tries c's active key, then each of c's
// retired keys in the order given to NewCipher, and returns the plaintext
// produced by whichever key successfully authenticates ciphertext.
//
// This fallback chain is what keeps previously written data readable across
// a key rotation: construct a new Cipher with the new key as active and the
// old key appended to retiredKeys (see NewCipher), and every row encrypted
// under the old key keeps decrypting through this method, unchanged, until
// it happens to be rewritten by normal application traffic. When no key —
// active or retired — can open ciphertext, Decrypt returns
// ErrDecryptionFailed; this covers both a wrong/missing key and a corrupted
// or tampered ciphertext, since AES-GCM does not distinguish the two.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if plaintext, err := open(c.active, ciphertext); err == nil {
		return plaintext, nil
	}
	for _, retired := range c.retired {
		if plaintext, err := open(retired, ciphertext); err == nil {
			return plaintext, nil
		}
	}
	return nil, ErrDecryptionFailed
}

// open splits the leading nonce off ciphertext and opens the remainder under
// aead, returning the authenticated plaintext.
func open(aead cipher.AEAD, ciphertext []byte) ([]byte, error) {
	nonceSize := aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("dbkit: ciphertext shorter than a nonce")
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return aead.Open(nil, nonce, sealed, nil)
}

// encryptedSerializer adapts a *Cipher to schema.SerializerInterface, so GORM
// invokes it transparently for any field tagged `gorm:"serializer:<name>"`
// once RegisterEncryptedSerializer has registered it under <name>.
type encryptedSerializer struct {
	cipher *Cipher
}

var _ schema.SerializerInterface = encryptedSerializer{}

// Scan implements schema.SerializerInterface. A SQL NULL column value leaves
// the field at its zero value; any other value is decrypted with the
// underlying Cipher and assigned to the field via field.Set, which handles
// the conversion from the decrypted bytes into the field's own Go type
// (string and []byte are both supported, matching Value below).
func (s encryptedSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, dbValue interface{}) error {
	if dbValue == nil {
		return nil
	}

	var raw []byte
	switch v := dbValue.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("dbkit: encrypted serializer: field %s: unsupported column value type %T", field.Name, dbValue)
	}

	plaintext, err := s.cipher.Decrypt(raw)
	if err != nil {
		return fmt.Errorf("dbkit: encrypted serializer: field %s: %w", field.Name, err)
	}
	return field.Set(ctx, dst, plaintext)
}

// Value implements schema.SerializerValuerInterface (embedded in
// schema.SerializerInterface). It encrypts the field's current value with
// the underlying Cipher, and the returned bytes are exactly what GORM writes
// to the column — so the stored row never contains the plaintext.
//
// A nil fieldValue (an unset field, e.g. a nil pointer field) stores SQL
// NULL rather than an encrypted empty value, round-tripping back to nil
// through Scan. Otherwise the field's Go type must be string or []byte: the
// two representations that fit the sensitive text and binary data this
// package targets. Any other type is a model-configuration mistake and
// returns an error rather than silently guessing an encoding for it.
func (s encryptedSerializer) Value(ctx context.Context, field *schema.Field, dst reflect.Value, fieldValue interface{}) (interface{}, error) {
	if fieldValue == nil {
		return nil, nil
	}

	var raw []byte
	switch v := fieldValue.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return nil, fmt.Errorf("dbkit: encrypted serializer: field %s: unsupported field type %T; encrypted fields must be string or []byte", field.Name, fieldValue)
	}

	ciphertext, err := s.cipher.Encrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("dbkit: encrypted serializer: field %s: %w", field.Name, err)
	}
	return ciphertext, nil
}

// RegisterEncryptedSerializer wires cipher into GORM's serializer registry
// under name, so that any model field tagged `gorm:"serializer:<name>"` is
// transparently encrypted with cipher.Encrypt on write and decrypted with
// cipher.Decrypt on read:
//
//	dbkit.RegisterEncryptedSerializer("phone_enc", phoneCipher)
//
//	type Account struct {
//	    Phone string `gorm:"serializer:phone_enc"`
//	}
//
// GORM's underlying registry (schema.RegisterSerializer) is process-global
// and keyed by name, so call RegisterEncryptedSerializer once per name during
// application bootstrap — alongside other Module.Register-time wiring, never
// inside a request path — before opening any *gorm.DB whose models reference
// name. Registering the same name again replaces the previously registered
// Cipher for every model using it, which is how a completed key rotation
// switches the active key for new writes; register a distinct name per
// Cipher when different fields must rotate independently of one another.
func RegisterEncryptedSerializer(name string, cipher *Cipher) {
	schema.RegisterSerializer(name, encryptedSerializer{cipher: cipher})
}
