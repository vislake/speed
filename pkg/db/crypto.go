package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// The two purposes one root key is split into.
//
// They are separate strings rather than two ranges of one derivation, and each
// is the info parameter of its own derivation: the subkeys are unrelated to
// each other. A ciphertext that leaks therefore says nothing about the digest
// of the plaintext it hides, and giving one purpose a new derivation does not
// move the other.
const (
	encryptionPurpose = "speed/db/field-encryption"
	blindIndexPurpose = "speed/db/blind-index"
)

// subkeyLength is how long each subkey is. It is the width AES-256 takes; for
// HMAC-SHA256 it is the natural key width, and any key at or below the hash's
// block length is used as it is.
const subkeyLength = 32

// keySet is what one root key yields.
type keySet struct {
	// aead seals and opens the encrypted columns. GCM authenticates what it
	// opens, so a value that was edited, truncated or written under another
	// key does not come back as plaintext at all.
	aead cipher.AEAD
	// blindIndex is the HMAC key the digests are computed under.
	blindIndex []byte
}

// keyRing is the material one assembled database works under: the subkeys of
// the configured root key, and one subkey set per retired root key.
//
// Only the current set writes. The retired ones are what a rotation leaves
// behind, and they open and never seal: rows written before the change stay
// readable, and nothing written after it is sealed under a key on its way out.
type keyRing struct {
	current keySet
	retired []keySet
}

// newKeyRing derives the material a section configures, keeping the retired
// keys in the order they are listed.
//
// A section with no root key yields no ring and no error. That is the shape a
// database nobody encrypts through has, and refusing it would fail a startup
// over an ability the assembly may never ask for. What it must not do is
// pretend to have key material: every use of a nil ring fails, where the
// alternative — a subkey derived from nothing — writes columns nothing can
// ever open, and writes them without an error anywhere near.
//
// A root key that cannot be derived fails the assembly instead. The one way it
// happens is a key shorter than 112 bits in a program running in FIPS 140-only
// mode, and an operator has two actions available for it — configure a longer
// key, or stop enforcing that mode — where a startup that went ahead would
// have encryption that quietly does nothing.
func newKeyRing(namespace string, cfg Config) (*keyRing, error) {
	if cfg.EncryptionKey == "" {
		return nil, nil
	}
	current, err := deriveKeySet(cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("db: %s.%s cannot be used as a root key: %w",
			namespace, encryptionKeyKey, err)
	}
	ring := &keyRing{current: current}
	for _, rootKey := range cfg.EncryptionRetiredKeys {
		retired, err := deriveKeySet(rootKey)
		if err != nil {
			return nil, fmt.Errorf("db: a key listed under %s.%s cannot be used as a root key: %w",
				namespace, encryptionRetiredKeysKey, err)
		}
		ring.retired = append(ring.retired, retired)
	}
	return ring, nil
}

// digestFor is the blind index function an assembled product answers with, and
// nil when the section configured no root key.
//
// The nil case is the state the product refuses to answer in. It is kept apart
// from the derivation error here, where the caller can still tell them apart:
// one is an assembly that never had a key, the other an assembly that was
// given one and cannot use it, and they send an operator to different items.
func digestFor(namespace string, cfg Config) (func(string) string, error) {
	keys, err := newKeyRing(namespace, cfg)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, nil
	}
	return keys.blindIndex, nil
}

// deriveKeySet turns one root key into its two subkeys.
//
// Both come out of HKDF with SHA-256 and no salt, which is what makes the
// derivation deterministic — every replica and every build has to arrive at
// the same subkeys, or a row one wrote would not open on another — and what
// keeps the purposes apart, since the info parameter is the only thing that
// differs between them. Nothing here is random and nothing here reads the
// clock: the subkeys are a function of the root key and the purpose alone.
func deriveKeySet(rootKey string) (keySet, error) {
	encryption, err := deriveSubkey(rootKey, encryptionPurpose)
	if err != nil {
		return keySet{}, err
	}
	blindIndex, err := deriveSubkey(rootKey, blindIndexPurpose)
	if err != nil {
		return keySet{}, err
	}
	// Neither call below can refuse a subkey this package derived: AES takes
	// exactly the 32 bytes HKDF was asked for, and GCM takes any 128-bit
	// block. Their errors travel back all the same, because the width of a
	// subkey is written down once, above, and a change to it has to fail
	// here rather than at the first statement that meets a short key.
	block, err := aes.NewCipher(encryption)
	if err != nil {
		return keySet{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return keySet{}, err
	}
	return keySet{aead: aead, blindIndex: blindIndex}, nil
}

// deriveSubkey is one derivation. It is a function of its two arguments alone:
// the same root key and the same purpose give the same subkey in every process
// that runs it.
func deriveSubkey(rootKey, purpose string) ([]byte, error) {
	return hkdf.Key(sha256.New, []byte(rootKey), nil, purpose, subkeyLength)
}

// encrypt seals a plaintext under the current subkey and returns what the
// column holds: the nonce followed by the sealed value, base64 encoded so that
// it is text on every engine.
//
// The nonce is drawn fresh for every call, which is what makes one plaintext
// land as two different columns on two writes, and what makes an equality
// query on this column impossible — hence the blind index beside it.
func (k *keyRing) encrypt(plaintext []byte) string {
	nonce := make([]byte, k.current.aead.NonceSize())
	// crypto/rand.Read fills the slice entirely and never returns an error:
	// it crashes the process rather than reporting one. There is no failure
	// here to handle or to report.
	rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(k.current.aead.Seal(nonce, nonce, plaintext, nil))
}

// decrypt opens a stored value, trying the current subkey and then each
// retired one.
//
// Nothing in the column and nothing in the configuration says which key a
// value was sealed under, and GCM refuses a key that is not the one: the
// search is over the keys that authenticate, and the column keeps exactly the
// shape the current key gives it. The price is one failed open per key that is
// not the right one, on a read that already went to the database.
func (k *keyRing) decrypt(stored string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return nil, fmt.Errorf("the column does not hold the text this module writes into it: %w", err)
	}
	for _, set := range k.sets() {
		if plaintext, err := set.open(raw); err == nil {
			return plaintext, nil
		}
	}
	return nil, errors.New("no configured root key opens this value: it was sealed under a key " +
		"that is neither the current one nor one of the retired ones")
}

// sets is the current subkey followed by the retired ones, in the order they
// are tried.
func (k *keyRing) sets() []keySet {
	sets := make([]keySet, 0, 1+len(k.retired))
	sets = append(sets, k.current)
	return append(sets, k.retired...)
}

// open opens one sealed value with one subkey.
//
// A value too short to hold a nonce is refused here rather than sliced: an
// unauthenticated column can hold anything, and a panic in a read path would
// take the process down over a row it could have reported.
func (s keySet) open(raw []byte) ([]byte, error) {
	if len(raw) < s.aead.NonceSize() {
		return nil, errors.New("the value is shorter than a nonce, so it cannot be a sealed one")
	}
	return s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], nil)
}

// blindIndex returns the digest of one plaintext under the current subkey.
//
// HMAC-SHA256 over the string as it was given: no case folding, no trimming,
// no canonicalisation of any kind. Two spellings of one address therefore have
// two digests, and a caller that flattens on one side only writes a column
// whose queries never match — which shows up as an empty result and not as an
// error, which is why the obligation sits with the caller on both sides.
//
// Only the current subkey computes it. A digest is not a ciphertext: there is
// nothing in it to open, so a retired key cannot help, and a rotation leaves
// every stored digest stale until the rows are rewritten under the new key.
// Rewriting them is the caller's, because what has to be rewritten is rows of
// models this package does not know.
func (k *keyRing) blindIndex(plaintext string) string {
	digest := hmac.New(sha256.New, k.current.blindIndex)
	digest.Write([]byte(plaintext))
	return hex.EncodeToString(digest.Sum(nil))
}
