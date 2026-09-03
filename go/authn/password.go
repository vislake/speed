package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// ErrInvalidPasswordHash is returned when a stored hash is not a PHC string
// this package can read: a wrong algorithm, a wrong version, malformed
// parameters, or corrupt base64. It is deliberately a single sentinel --
// distinguishing the shapes of corruption tells an attacker nothing useful
// and tells an operator nothing the log line does not already say.
var ErrInvalidPasswordHash = errors.New("authn: stored password hash is not a valid argon2id PHC string")

// phcPrefix, phcVersion and the field separators of the PHC string format
// this package writes and reads:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// The parameters travel INSIDE the stored value on purpose. It is what makes
// raising the cost a configuration change rather than a migration: every
// existing hash keeps verifying under the parameters it was created with,
// NeedsRehash reports which ones are below the current policy, and the next
// successful sign-in upgrades that user's hash in place. A scheme that kept
// the parameters only in configuration would invalidate every stored password
// the moment the cost was raised.
const (
	phcAlgorithm = "argon2id"
	phcVersion   = argon2.Version

	// maxPHCFieldBytes bounds the decoded salt and digest. Anything this
	// package wrote is 16 and 32 bytes, and the whole encoded value lives
	// in a VARCHAR(255) column, so the bound is orders of magnitude above
	// any real value. It exists so the int-to-uint32 narrowing in
	// decodePHC is provably in range rather than merely unlikely to
	// overflow.
	maxPHCFieldBytes = 1024
)

// PasswordParams are the argon2id cost parameters used for NEW hashes.
//
// They are bootstrap configuration, not dynamic configuration: the right
// values depend on the machine the process runs on, they must be identical
// across every replica of one deployment, and changing them at runtime from
// an admin console would let an operator make sign-in either uselessly cheap
// or slow enough to be a self-inflicted denial of service. Password POLICY --
// minimum length, weak-password rejection -- is the opposite and is dynamic
// (see PasswordPolicy).
type PasswordParams struct {
	// Memory is the argon2id memory cost in KiB. It is the parameter that
	// actually defeats GPU and ASIC attackers, so prefer raising it over
	// raising Iterations.
	Memory uint32
	// Iterations is the argon2id time cost.
	Iterations uint32
	// Parallelism is the number of lanes.
	Parallelism uint8
	// SaltLength is the number of random bytes drawn per hash. A fresh
	// salt per hash is what makes two accounts with the same password
	// store different digests.
	SaltLength uint32
	// KeyLength is the length of the derived digest in bytes.
	KeyLength uint32
}

// DefaultPasswordParams returns OWASP's first recommended argon2id
// configuration (m=19456 KiB, t=2, p=1) with a 16-byte salt and a 32-byte
// digest. It is a deliberate floor rather than a maximum: a deployment on
// hardware that can afford more should raise Memory and let NeedsRehash
// migrate existing users on their next sign-in.
func DefaultPasswordParams() PasswordParams {
	return PasswordParams{
		Memory:      19456,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// validate rejects parameters argon2 cannot run with, so a misconfiguration
// surfaces at the first hash rather than as a panic inside the KDF.
func (p PasswordParams) validate() error {
	switch {
	case p.Memory == 0:
		return errors.New("authn: password params: Memory must be greater than zero")
	case p.Iterations == 0:
		return errors.New("authn: password params: Iterations must be greater than zero")
	case p.Parallelism == 0:
		return errors.New("authn: password params: Parallelism must be greater than zero")
	case p.SaltLength < 8:
		return errors.New("authn: password params: SaltLength must be at least 8 bytes")
	case p.KeyLength < 16:
		return errors.New("authn: password params: KeyLength must be at least 16 bytes")
	}
	return nil
}

// HashPassword derives an argon2id digest of password under p and returns it
// PHC-encoded, salt and parameters included, ready to store in
// User.PasswordHash.
//
// Two calls with the same password never return the same string: each draws
// its own salt from crypto/rand.
func HashPassword(password string, p PasswordParams) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("authn: draw password salt: %w", err)
	}

	digest := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return encodePHC(p, salt, digest), nil
}

// VerifyPassword reports whether password matches the PHC-encoded hash in
// encoded. It re-derives the digest under the parameters recorded inside
// encoded -- not under the current PasswordParams -- which is what lets a
// deployment raise its cost without invalidating anything.
//
// A false result with a nil error means "wrong password". A non-nil error
// means the stored value could not be read at all and is never a way to
// distinguish a wrong password from a valid one.
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, params.KeyLength)

	// subtle.ConstantTimeCompare, not bytes.Equal: an early-exit compare
	// leaks how many leading bytes of the digest a guess got right, which
	// over enough attempts is a byte-at-a-time oracle on the digest.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether encoded was produced under weaker parameters
// than want, so a caller that has just verified a password successfully can
// upgrade the stored hash in place while it still holds the plaintext.
//
// It compares every parameter, and reports true whenever any recorded value
// differs from the wanted one -- including a DOWNGRADE. Rehashing on a
// deliberate downgrade is the right behaviour: the point is that the stored
// corpus converges on the currently configured parameters, and a deployment
// that lowered its cost on purpose wants exactly that.
func NeedsRehash(encoded string, want PasswordParams) (bool, error) {
	have, _, _, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}
	return have != want, nil
}

// encodePHC renders p, salt and digest as the PHC string this package stores.
// Base64 is the unpadded standard alphabet the argon2 reference
// implementation uses, so the output interoperates with other argon2id
// readers rather than being a private encoding.
func encodePHC(p PasswordParams, salt, digest []byte) string {
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcAlgorithm, phcVersion, p.Memory, p.Iterations, p.Parallelism,
		enc.EncodeToString(salt), enc.EncodeToString(digest))
}

// decodePHC parses a PHC string written by encodePHC. Every failure returns
// ErrInvalidPasswordHash so callers classify with errors.Is.
//
// The returned PasswordParams describes the stored hash completely, including
// SaltLength and KeyLength recovered from the decoded values -- which is what
// lets VerifyPassword re-derive a digest of the right length and NeedsRehash
// compare every parameter.
func decodePHC(encoded string) (p PasswordParams, salt, digest []byte, err error) {
	fields := strings.Split(encoded, "$")
	// A well-formed value starts with "$", so the split yields an empty
	// leading field followed by algorithm, version, parameters, salt and
	// digest.
	if len(fields) != 6 || fields[0] != "" {
		return p, nil, nil, fmt.Errorf("%w: expected 5 '$'-separated fields", ErrInvalidPasswordHash)
	}
	if fields[1] != phcAlgorithm {
		return p, nil, nil, fmt.Errorf("%w: algorithm %q is not %q", ErrInvalidPasswordHash, fields[1], phcAlgorithm)
	}

	var version int
	if _, err = fmt.Sscanf(fields[2], "v=%d", &version); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable version field", ErrInvalidPasswordHash)
	}
	if version != phcVersion {
		return p, nil, nil, fmt.Errorf("%w: version %d is not %d", ErrInvalidPasswordHash, version, phcVersion)
	}

	if _, err = fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable parameter field", ErrInvalidPasswordHash)
	}

	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(fields[4]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable salt", ErrInvalidPasswordHash)
	}
	if digest, err = enc.DecodeString(fields[5]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable digest", ErrInvalidPasswordHash)
	}

	// Bound before narrowing. The values come from a stored column, and
	// converting an int of unchecked length to uint32 is exactly the
	// conversion that wraps silently; past this check both lengths are
	// provably far inside uint32's range.
	if len(salt) > maxPHCFieldBytes || len(digest) > maxPHCFieldBytes {
		return PasswordParams{}, nil, nil, fmt.Errorf("%w: salt or digest is implausibly long", ErrInvalidPasswordHash)
	}
	// #nosec G115 -- both lengths are bounded by maxPHCFieldBytes on the
	// line above, which gosec's analyzer does not follow across the
	// comparison; the conversion cannot overflow.
	p.SaltLength, p.KeyLength = uint32(len(salt)), uint32(len(digest))
	return p, salt, digest, nil
}

// PasswordPolicy is the set of rules a NEW password must satisfy. Unlike
// PasswordParams it is DYNAMIC configuration: it is a product decision an
// operator tunes per deployment and, where the deployment allows it, per
// tenant. Module.Register declares the matching schema keys.
//
// The rules follow NIST SP 800-63B: length is the requirement that actually
// helps, and mandatory character-class composition is not one, because it
// mostly produces predictable substitutions. Rejecting known-weak passwords
// outright is the second half of the same recommendation.
type PasswordPolicy struct {
	// MinLength is the fewest runes a password may have. Counted in runes,
	// not bytes, so a non-ASCII passphrase is not penalised for its
	// encoding.
	MinLength int
	// MaxLength bounds the input. It exists to bound the KDF's work, not
	// to restrict the user: an unbounded password is an unbounded amount
	// of memory-hard hashing per request.
	MaxLength int
	// Denylist holds passwords rejected regardless of length, compared
	// case-insensitively and checked BEFORE the length rules. It is
	// deliberately small here -- shipping a breached-password corpus in a
	// library would put megabytes in every consumer's binary -- and a
	// deployment that wants one supplies it through this field.
	Denylist []string
}

// DefaultPasswordPolicy returns the shipped default: at least 12 runes, at
// most 128, and a small denylist of the passwords that top every published
// breach corpus.
func DefaultPasswordPolicy() PasswordPolicy {
	return PasswordPolicy{
		MinLength: 12,
		MaxLength: 128,
		Denylist: []string{
			// Short entries are still worth listing even though
			// they would also fail the length rule: Validate
			// checks this list first, so they report the reason
			// that actually helps rather than "too short".
			"password", "passw0rd", "password1", "password123",
			"123456", "1234567890", "12345678", "123456789",
			"qwertyuiop", "administrator", "letmein123", "iloveyou1",
			// Entries long enough to pass the default minimum,
			// which is where a denylist earns its keep.
			"password1234", "passwordpassword", "qwertyuiopasdf",
			"123456789012", "iloveyou1234", "letmeinletmein",
		},
	}
}

// Validate reports whether password satisfies p, returning one of this
// module's structured errors -- never localized text -- so the client renders
// the message from its own catalog.
func (p PasswordPolicy) Validate(password string) error {
	// The denylist is checked FIRST, before length. Two reasons, and the
	// second is the one that matters: "that password is too easy to guess"
	// is more useful to whoever typed it than "too short", and -- since
	// most published-corpus passwords are shorter than any sane minimum --
	// a length-first order would make nearly every denylist entry
	// unreachable, turning the list into decoration.
	for _, weak := range p.Denylist {
		if strings.EqualFold(password, weak) {
			return ErrPasswordTooWeak
		}
	}

	length := utf8.RuneCountInString(password)
	if length < p.MinLength {
		return ErrPasswordTooShort.WithParam("min_length", p.MinLength)
	}
	if p.MaxLength > 0 && length > p.MaxLength {
		return ErrPasswordTooLong.WithParam("max_length", p.MaxLength)
	}
	return nil
}
