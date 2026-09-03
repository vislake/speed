package authn

import (
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// testParams are deliberately far below DefaultPasswordParams: these tests run
// hundreds of derivations, and the property under test is never the cost.
func testParams() PasswordParams {
	return PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

func TestHashPassword_ProducesAVerifiablePHCString(t *testing.T) {
	t.Parallel()

	encoded, err := HashPassword("correct horse battery staple", testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if strings.Contains(encoded, "correct horse") {
		t.Fatal("the stored hash contains the plaintext password")
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Fatalf("stored hash = %q, want the argon2id PHC prefix", encoded)
	}

	ok, err := VerifyPassword(encoded, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if !ok {
		t.Error("VerifyPassword() = false for the password that produced the hash")
	}
}

func TestVerifyPassword_WrongPasswordDoesNotVerify(t *testing.T) {
	t.Parallel()

	encoded, err := HashPassword("correct horse battery staple", testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple", "wrong"} {
		ok, err := VerifyPassword(encoded, wrong)
		if err != nil {
			t.Fatalf("VerifyPassword(%q) error = %v", wrong, err)
		}
		if ok {
			t.Errorf("VerifyPassword(%q) = true, want false", wrong)
		}
	}
}

// TestHashPassword_SamePasswordTwiceProducesDifferentHashes proves the salt is
// per-hash. Without it, two accounts with the same password would store
// identical digests, which hands an attacker who dumps the table a free
// grouping of users by shared password.
func TestHashPassword_SamePasswordTwiceProducesDifferentHashes(t *testing.T) {
	t.Parallel()

	first, err := HashPassword("the same password", testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	second, err := HashPassword("the same password", testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if first == second {
		t.Fatal("two hashes of the same password are identical; the salt is not per-hash")
	}

	for _, encoded := range []string{first, second} {
		ok, err := VerifyPassword(encoded, "the same password")
		if err != nil || !ok {
			t.Fatalf("VerifyPassword(%q) = %v, %v; both hashes must verify", encoded, ok, err)
		}
	}
}

func TestVerifyPassword_MalformedStoredHashIsRejected(t *testing.T) {
	t.Parallel()

	valid, err := HashPassword("password for the tamper cases", testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	fields := strings.Split(valid, "$")

	cases := []struct {
		name    string
		encoded string
	}{
		{name: "empty", encoded: ""},
		{name: "not PHC at all", encoded: "plaintext"},
		{name: "too few fields", encoded: "$argon2id$v=19$m=64,t=1,p=1$" + fields[4]},
		{name: "wrong algorithm", encoded: strings.Replace(valid, "argon2id", "argon2i", 1)},
		{name: "wrong version", encoded: strings.Replace(valid, "v=19", "v=18", 1)},
		{name: "unreadable parameters", encoded: strings.Replace(valid, "m=64,t=1,p=1", "m=sixtyfour", 1)},
		{name: "corrupt salt", encoded: "$" + fields[1] + "$" + fields[2] + "$" + fields[3] + "$!!!!$" + fields[5]},
		{name: "corrupt digest", encoded: "$" + fields[1] + "$" + fields[2] + "$" + fields[3] + "$" + fields[4] + "$!!!!"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.encoded, "password for the tamper cases")
			if ok {
				t.Error("VerifyPassword() = true for an unreadable stored hash")
			}
			if !errors.Is(err, ErrInvalidPasswordHash) {
				t.Errorf("VerifyPassword() error = %v, want ErrInvalidPasswordHash", err)
			}
		})
	}
}

// TestVerifyPassword_TamperedDigestDoesNotVerify covers the case a
// well-formed but altered hash creates: it parses, so it must simply fail to
// verify rather than error.
func TestVerifyPassword_TamperedDigestDoesNotVerify(t *testing.T) {
	t.Parallel()

	encoded, err := HashPassword("a password", testParams())
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	fields := strings.Split(encoded, "$")
	// Flip one base64 character of the digest to a different legal one.
	digest := []byte(fields[5])
	if digest[0] == 'A' {
		digest[0] = 'B'
	} else {
		digest[0] = 'A'
	}
	tampered := "$" + fields[1] + "$" + fields[2] + "$" + fields[3] + "$" + fields[4] + "$" + string(digest)

	ok, err := VerifyPassword(tampered, "a password")
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v, want a clean false", err)
	}
	if ok {
		t.Error("VerifyPassword() = true for a tampered digest")
	}
}

// TestNeedsRehash covers the migration path that lets a deployment raise its
// argon2id cost without invalidating a single stored password.
func TestNeedsRehash(t *testing.T) {
	t.Parallel()

	base := testParams()
	encoded, err := HashPassword("a password", base)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	cases := []struct {
		name string
		want PasswordParams
		need bool
	}{
		{name: "identical parameters", want: base, need: false},
		{name: "more memory", want: PasswordParams{Memory: 128, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, need: true},
		{name: "more iterations", want: PasswordParams{Memory: 64, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}, need: true},
		{name: "more parallelism", want: PasswordParams{Memory: 64, Iterations: 1, Parallelism: 2, SaltLength: 16, KeyLength: 32}, need: true},
		{name: "longer salt", want: PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 24, KeyLength: 32}, need: true},
		{name: "longer key", want: PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 64}, need: true},
		{name: "a deliberate downgrade also converges", want: PasswordParams{Memory: 32, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, need: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NeedsRehash(encoded, tc.want)
			if err != nil {
				t.Fatalf("NeedsRehash() error = %v", err)
			}
			if got != tc.need {
				t.Errorf("NeedsRehash() = %v, want %v", got, tc.need)
			}
		})
	}
}

// TestVerifyPassword_StillVerifiesAfterTheParametersChange is the point of
// putting the parameters inside the stored value: raising the cost must not
// lock anyone out.
func TestVerifyPassword_StillVerifiesAfterTheParametersChange(t *testing.T) {
	t.Parallel()

	old, err := HashPassword("a password", PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	ok, err := VerifyPassword(old, "a password")
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if !ok {
		t.Fatal("a hash created under the previous parameters no longer verifies")
	}
}

func TestHashPassword_RejectsUnrunnableParameters(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		params PasswordParams
	}{
		{name: "zero memory", params: PasswordParams{Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}},
		{name: "zero iterations", params: PasswordParams{Memory: 64, Parallelism: 1, SaltLength: 16, KeyLength: 32}},
		{name: "zero parallelism", params: PasswordParams{Memory: 64, Iterations: 1, SaltLength: 16, KeyLength: 32}},
		{name: "salt too short", params: PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 4, KeyLength: 32}},
		{name: "key too short", params: PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 8}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := HashPassword("a password", tc.params); err == nil {
				t.Error("HashPassword() error = nil, want a rejection")
			}
		})
	}
}

func TestPasswordPolicy_Validate(t *testing.T) {
	t.Parallel()

	policy := DefaultPasswordPolicy()

	cases := []struct {
		name     string
		password string
		wantCode string
	}{
		{name: "a long passphrase is accepted", password: "a reasonably long passphrase"},
		{name: "exactly the minimum length is accepted", password: strings.Repeat("a", policy.MinLength)},
		{name: "one below the minimum is rejected", password: strings.Repeat("a", policy.MinLength-1), wantCode: ErrPasswordTooShort.Code},
		{name: "empty is rejected as too short", password: "", wantCode: ErrPasswordTooShort.Code},
		{name: "one above the maximum is rejected", password: strings.Repeat("a", policy.MaxLength+1), wantCode: ErrPasswordTooLong.Code},
		{name: "a denylisted password is rejected however it is cased", password: "PassWord123", wantCode: ErrPasswordTooWeak.Code},
		// Length is counted in runes, not bytes: a twelve-character
		// non-ASCII passphrase must not be rejected for its encoding.
		{name: "length is counted in runes", password: "éééééééééééé"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.Validate(tc.password)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if !hasCode(err, tc.wantCode) {
				t.Fatalf("Validate() error = %v, want code %q", err, tc.wantCode)
			}
		})
	}
}

// TestPasswordPolicy_TooShortCarriesTheLimit proves the structured error
// carries the parameter the client needs to render the message, rather than
// the module rendering text itself.
func TestPasswordPolicy_TooShortCarriesTheLimit(t *testing.T) {
	t.Parallel()

	policy := PasswordPolicy{MinLength: 14, MaxLength: 40}
	err := policy.Validate("short")

	appErr, ok := asAppError(err)
	if !ok {
		t.Fatalf("Validate() error = %v, want an *apperr.Error", err)
	}
	if appErr.Code != ErrPasswordTooShort.Code {
		t.Fatalf("code = %q, want %q", appErr.Code, ErrPasswordTooShort.Code)
	}
	if got := appErr.Params["min_length"]; got != 14 {
		t.Errorf("params[min_length] = %v, want 14", got)
	}
	if ErrPasswordTooShort.Params != nil {
		t.Error("the package-level sentinel gained parameters; WithParam must derive a new value, never mutate the receiver")
	}
}

// asAppError is a local alias for apperr.As, so the test reads as one thought
// rather than as an import detail.
func asAppError(err error) (*apperr.Error, bool) { return apperr.As(err) }
