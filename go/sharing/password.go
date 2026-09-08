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
// The parameters travel INSIDE the stored value on purpose -- the identical
// design authn's own PHC strings use (authn/password.go's phcPrefix doc
// comment gives the full reasoning in the context this module mirrors): a
// future cost change writes new hashes under the new parameters while every
// existing share keeps verifying under the parameters its hash was created
// with, because the reading side re-derives the digest from the stored
// triple, never from package constants. The sharePassword* constants below
// are what this module's writer emits -- OWASP's first recommended
// argon2id configuration, the same floor authn.DefaultPasswordParams ships
// -- not what the reader assumes. A share's access password protects a
// single resource behind a link, not an account, so this module does not
// pull in authn (a heavy module several tiers away in the dependency graph)
// just to reuse its PasswordParams type; a self-contained argon2id
// implementation is a few dozen lines against golang.org/x/crypto, already
// a common, low-cost dependency elsewhere in this codebase. The writer's
// parameters are package constants -- no host-facing knob configures them;
// the reader already honors any valid stored parameters.
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

	// maxSharePasswordMemoryKiB and maxSharePasswordIterations bound the
	// COST PARAMETERS a stored PHC string may carry -- the symmetric half
	// of maxPHCFieldBytes's discipline over the salt and digest. This
	// package's reader honors whatever parameters a stored hash records
	// (decodeSharePasswordPHC's own doc comment), and a value is
	// parameters-travel-in-the-hash by design so a future cost bump or
	// another tool's configuration keeps verifying -- but the parameter
	// fields are parsed from stored text that a corrupt or hostile row
	// could fill with anything up to the parse's own ceiling (m and t are
	// uint32, so m=4294967295 would make argon2.IDKey attempt a ~4 TiB
	// allocation -- argon2.IDKey allocates roughly memory KiB on the
	// calling goroutine -- and t=4294967295 would loop for days). The
	// bounds below reject such values as corrupt BEFORE argon2.IDKey is
	// ever reached, with the same "orders of magnitude above any real
	// value" headroom maxPHCFieldBytes applies to the salt and digest:
	// this package writes m=19456 KiB and t=2, and no plausible deployment
	// or future cost bump approaches a 1 GiB memory cost or 65536
	// iterations, while both caps sit far below the uint32 ceilings that
	// would let a hostile stored row turn one verification into a memory
	// or CPU bomb. parallelism needs no cap of its own: it parses into a
	// uint8, so any value above 255 already fails the parse (fmt's %d
	// refuses an out-of-range value for the target type), and x/crypto's
	// argon2 itself accepts no more than 255 lanes.
	maxSharePasswordMemoryKiB  = 1 << 20 // 1 GiB
	maxSharePasswordIterations = 1 << 16
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

// dummySharePasswordHash is a fixed-parameter, fixed-content PHC string that
// decodes cleanly but names no real password -- its salt and digest are
// both all-zero byte slices of the same lengths a real hash uses, computed
// deterministically at package init rather than drawn from crypto/rand, so
// it costs nothing to construct and can never fail to construct.
//
// burnSharePasswordCheck runs verifySharePassword against this hash on
// every Service.Access refusal path that would otherwise skip password
// verification entirely -- see burnSharePasswordCheck's own doc comment.
var dummySharePasswordHash = encodeSharePasswordPHC(
	make([]byte, sharePasswordSaltLen),
	make([]byte, sharePasswordKeyLen),
)

// burnSharePasswordCheck runs one argon2id verification at the same cost
// parameters a real Service.Access password check uses, against
// dummySharePasswordHash, and discards the result.
//
// Service.Access must call this on every refusal path that would otherwise
// return without ever calling verifySharePassword: an unrecognized token
// (no Share row to check a password against at all), a share with no
// password configured, and a password-protected share accessed with no
// password supplied. Without it, those paths return in the time a single
// map/index lookup takes while a real password attempt against a
// password-protected share pays argon2id's tens-of-milliseconds cost,
// letting an external prober distinguish "this token names a
// password-protected share" from every other refusal reason purely by
// response latency -- undermining the module's outward-identical-answer
// property (see doc.go) even though every refusal already answers with
// the identical ErrNotAccessible.
//
// guess, when non-nil, is hashed as the attacker-supplied password would
// be, so a probe that supplies a password guess against an unprotected
// share or an unknown token pays the identical argon2id cost a guess
// against a real password-protected share pays.
func burnSharePasswordCheck(guess *string) {
	attempt := ""
	if guess != nil {
		attempt = *guess
	}
	_, _ = verifySharePassword(dummySharePasswordHash, attempt)
}

// verifySharePassword reports whether password matches the PHC-encoded hash
// in encoded.
//
// The digest is re-derived under the cost parameters recorded INSIDE
// encoded -- decodeSharePasswordPHC hands them back -- never under this
// package's own sharePassword* constants. That is what makes the stored
// parameters meaningful rather than decorative (see that function's doc
// comment): a share whose hash was written under different cost parameters
// -- by a future cost bump, or by a tool with its own configuration --
// still verifies, exactly as authn.VerifyPassword's identical
// parameters-travel-in-the-hash design already guarantees for account
// passwords.
//
// A false result with a nil error means "wrong password". A non-nil error
// means the stored value could not be read at all, and is never a way to
// distinguish a wrong password from a valid one -- callers must treat both
// as "denied", per this module's outward-identical-answer rule (service.go's
// Service.Access).
func verifySharePassword(encoded, password string) (bool, error) {
	params, salt, want, err := decodeSharePasswordPHC(encoded)
	if err != nil {
		return false, err
	}
	// #nosec G115 -- len(want) is bounded by maxPHCFieldBytes inside
	// decodeSharePasswordPHC before it is returned, which gosec's analyzer
	// does not follow across the function boundary; the conversion cannot
	// overflow. params' own fields are already argon2's own value types,
	// parsed in place by fmt.Sscanf (out-of-range values fail the parse).
	got := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallel, uint32(len(want)))
	// subtle.ConstantTimeCompare, not bytes.Equal: an early-exit compare
	// leaks how many leading bytes of the digest a guess got right.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// sharePasswordParams is the decoded cost-parameter triple of a stored PHC
// string. The field types are argon2.IDKey's own parameter types on
// purpose: decodeSharePasswordPHC parses the stored text straight into
// them (fmt's %d refuses an out-of-range value for an unsigned or narrow
// field), so no int-to-uint narrowing ever happens in this package's read
// path.
type sharePasswordParams struct {
	memory     uint32
	iterations uint32
	parallel   uint8
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
// encodeSharePasswordPHC and returns the cost parameters, salt and digest
// it records. Every failure returns ErrInvalidSharePasswordHash so callers
// classify with errors.Is.
//
// The returned params are the stored hash's own, parsed from the
// parameter field -- the values verifySharePassword re-derives the digest
// under, mirroring authn.decodePHC's identical treatment of its own stored
// parameters. A parameter field left EMPTY (a value whose writer omitted
// the m/t/p triple, which this module's own writer never does) falls back
// to this package's sharePassword* constants -- a stored digest can only
// be checked against SOME parameter set, and the package's own are the
// only ones a parameter-less value can plausibly have been produced under
// -- while a non-empty parameter field that does not parse, or whose
// values argon2 cannot run with, is refused as corrupt rather than guessed
// at: verification under the wrong parameters would silently report every
// password wrong (see the m>0/t>0/p>0 floor below, the same refusal
// authn's decodePHC applies so a corrupt stored row can never reach
// argon2.IDKey, which PANICS on t=0 and p=0).
func decodeSharePasswordPHC(encoded string) (params sharePasswordParams, salt, digest []byte, err error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: expected 5 '$'-separated fields", ErrInvalidSharePasswordHash)
	}
	if fields[1] != sharePasswordAlgorithm {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: algorithm %q is not %q", ErrInvalidSharePasswordHash, fields[1], sharePasswordAlgorithm)
	}

	var version int
	if _, err = fmt.Sscanf(fields[2], "v=%d", &version); err != nil {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable version field", ErrInvalidSharePasswordHash)
	}
	if version != sharePasswordVersion {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: version %d is not %d", ErrInvalidSharePasswordHash, version, sharePasswordVersion)
	}

	// Parsed straight into argon2's own value types: fmt's %d refuses a
	// value out of range for the target field (a parallelism of 300 or a
	// memory cost above uint32's ceiling fails the parse rather than
	// narrowing silently).
	switch fields[3] {
	case "":
		// See the doc comment above -- a defensive default no value this
		// module ever wrote exercises.
		params.memory, params.iterations, params.parallel = sharePasswordMemory, sharePasswordIterations, sharePasswordParallel
	default:
		if _, err = fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &params.memory, &params.iterations, &params.parallel); err != nil {
			return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable parameter field", ErrInvalidSharePasswordHash)
		}
	}
	// argon2.IDKey panics on t=0 and p=0 rather than returning an error --
	// a panic in a request goroutine turns one corrupt stored row into a
	// crashed request handler. m=0 is refused alongside them: argon2
	// silently clamps a zero memory cost up to its own floor, which would
	// verify (or burn) at a cost no legitimate hash of this package's ever
	// used -- a stored row claiming it is corruption, not a configuration.
	// This mirrors the reading-side refusal authn.decodePHC applies via
	// PasswordParams.validate. The upper bounds refuse the symmetric
	// hazard: an absurdly large m/t would make the argon2.IDKey call below
	// (verifySharePassword) attempt a multi-terabyte allocation or an
	// effectively endless loop on the request goroutine -- see
	// maxSharePasswordMemoryKiB's own doc comment for the numbers and why
	// the caps sit orders of magnitude above any legitimate value.
	if params.memory == 0 || params.iterations == 0 || params.parallel == 0 {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: cost parameters argon2 cannot run with", ErrInvalidSharePasswordHash)
	}
	if params.memory > maxSharePasswordMemoryKiB || params.iterations > maxSharePasswordIterations {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: cost parameters beyond this package's bounds", ErrInvalidSharePasswordHash)
	}

	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(fields[4]); err != nil {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable salt", ErrInvalidSharePasswordHash)
	}
	if digest, err = enc.DecodeString(fields[5]); err != nil {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: unreadable digest", ErrInvalidSharePasswordHash)
	}
	if len(salt) > maxPHCFieldBytes || len(digest) > maxPHCFieldBytes {
		return sharePasswordParams{}, nil, nil, fmt.Errorf("%w: salt or digest is implausibly long", ErrInvalidSharePasswordHash)
	}
	return params, salt, digest, nil
}
