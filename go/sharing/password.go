package sharing

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrInvalidSharePasswordHash reports that a stored Share.PasswordHash value
// is not a PHC string this package can read: malformed parameters, a wrong
// algorithm tag, or corrupt base64. It is a single sentinel deliberately --
// distinguishing the shapes of corruption tells an attacker nothing useful,
// mirroring authn.ErrInvalidPasswordHash's identical reasoning.
var ErrInvalidSharePasswordHash = errors.New("sharing: stored share password hash is not a valid argon2id PHC string")

// The PHC string this package writes and reads:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// sharePasswordParams are fixed, un-configurable cost parameters this round
// -- OWASP's first recommended argon2id configuration, the same floor
// authn.DefaultPasswordParams ships. A share's access password protects a
// single resource behind a link, not an account, so this module does not
// pull in authn (a heavy module several tiers away in the dependency graph)
// just to reuse its PasswordParams type; a self-contained, fixed-cost
// argon2id implementation is a few dozen lines against golang.org/x/crypto,
// already a common, low-cost dependency elsewhere in this codebase. Making
// these parameters host-configurable, the way authn's own are, is deferred
// to whichever round wires an actual host-facing knob for it (AGENTS.md's
// Known limitations).
const (
	sharePasswordAlgorithm  = "argon2id"
	sharePasswordVersion    = argon2.Version
	sharePasswordMemory     = 19456
	sharePasswordIterations = 2
	sharePasswordParallel   = 1
	sharePasswordSaltLen    = 16
	sharePasswordKeyLen     = 32

	// maxPHCFieldBytes bounds the decoded salt and digest, mirroring
	// authn's identical constant and identical reasoning: it makes the
	// int-to-uint32 narrowing below provably in range rather than merely
	// unlikely to overflow.
	maxPHCFieldBytes = 1024
)

// hashSharePassword derives an argon2id digest of password and returns it
// PHC-encoded, salt and parameters included, ready to store in
// Share.PasswordHash.
//
// Two calls with the same password never return the same string: each draws
// its own salt from crypto/rand.
func hashSharePassword(password string) (string, error) {
	salt := make([]byte, sharePasswordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("sharing: draw share password salt: %w", err)
	}
	digest := argon2.IDKey([]byte(password), salt, sharePasswordIterations, sharePasswordMemory, sharePasswordParallel, sharePasswordKeyLen)
	return encodeSharePasswordPHC(salt, digest), nil
}

// verifySharePassword reports whether password matches the PHC-encoded hash
// in encoded.
//
// A false result with a nil error means "wrong password". A non-nil error
// means the stored value could not be read at all, and is never a way to
// distinguish a wrong password from a valid one -- callers must treat both
// as "denied", per this module's outward-identical-answer rule (service.go's
// Service.Access).
func verifySharePassword(encoded, password string) (bool, error) {
	salt, want, err := decodeSharePasswordPHC(encoded)
	if err != nil {
		return false, err
	}
	// #nosec G115 -- decodeSharePasswordPHC already bounds len(want) by
	// maxPHCFieldBytes before returning, which gosec's analyzer does not
	// follow across the function boundary; the conversion cannot overflow.
	got := argon2.IDKey([]byte(password), salt, sharePasswordIterations, sharePasswordMemory, sharePasswordParallel, uint32(len(want)))
	// subtle.ConstantTimeCompare, not bytes.Equal: an early-exit compare
	// leaks how many leading bytes of the digest a guess got right.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// encodeSharePasswordPHC renders salt and digest as the fixed-parameter PHC
// string this package stores. Base64 is the unpadded standard alphabet the
// argon2 reference implementation uses, so the output interoperates with
// other argon2id readers.
func encodeSharePasswordPHC(salt, digest []byte) string {
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		sharePasswordAlgorithm, sharePasswordVersion, sharePasswordMemory, sharePasswordIterations, sharePasswordParallel,
		enc.EncodeToString(salt), enc.EncodeToString(digest))
}

// decodeSharePasswordPHC parses a PHC string written by
// encodeSharePasswordPHC. Every failure returns ErrInvalidSharePasswordHash
// so callers classify with errors.Is.
func decodeSharePasswordPHC(encoded string) (salt, digest []byte, err error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" {
		return nil, nil, fmt.Errorf("%w: expected 5 '$'-separated fields", ErrInvalidSharePasswordHash)
	}
	if fields[1] != sharePasswordAlgorithm {
		return nil, nil, fmt.Errorf("%w: algorithm %q is not %q", ErrInvalidSharePasswordHash, fields[1], sharePasswordAlgorithm)
	}

	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(fields[4]); err != nil {
		return nil, nil, fmt.Errorf("%w: unreadable salt", ErrInvalidSharePasswordHash)
	}
	if digest, err = enc.DecodeString(fields[5]); err != nil {
		return nil, nil, fmt.Errorf("%w: unreadable digest", ErrInvalidSharePasswordHash)
	}
	if len(salt) > maxPHCFieldBytes || len(digest) > maxPHCFieldBytes {
		return nil, nil, fmt.Errorf("%w: salt or digest is implausibly long", ErrInvalidSharePasswordHash)
	}
	return salt, digest, nil
}
