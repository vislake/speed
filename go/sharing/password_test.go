package sharing

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashSharePassword_NeverStoresPlaintext(t *testing.T) {
	const password = "correct horse battery staple"
	hash, err := hashSharePassword(password)
	if err != nil {
		t.Fatalf("hashSharePassword: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatalf("hash %q contains the plaintext password", hash)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash %q does not look like a PHC argon2id string", hash)
	}
}

func TestHashSharePassword_TwoCallsProduceDifferentHashes(t *testing.T) {
	const password = "same password twice"
	a, err := hashSharePassword(password)
	if err != nil {
		t.Fatalf("hashSharePassword: %v", err)
	}
	b, err := hashSharePassword(password)
	if err != nil {
		t.Fatalf("hashSharePassword: %v", err)
	}
	if a == b {
		t.Errorf("two hashes of the same password matched; each call must draw its own salt")
	}
}

func TestVerifySharePassword_CorrectPasswordVerifies(t *testing.T) {
	const password = "hunter2 but longer"
	hash, err := hashSharePassword(password)
	if err != nil {
		t.Fatalf("hashSharePassword: %v", err)
	}
	ok, err := verifySharePassword(hash, password)
	if err != nil {
		t.Fatalf("verifySharePassword: %v", err)
	}
	if !ok {
		t.Errorf("verifySharePassword(correct password) = false, want true")
	}
}

func TestVerifySharePassword_WrongPasswordFails(t *testing.T) {
	hash, err := hashSharePassword("the real password")
	if err != nil {
		t.Fatalf("hashSharePassword: %v", err)
	}
	ok, err := verifySharePassword(hash, "not the real password")
	if err != nil {
		t.Fatalf("verifySharePassword: %v", err)
	}
	if ok {
		t.Errorf("verifySharePassword(wrong password) = true, want false")
	}
}

func TestVerifySharePassword_MalformedHashReportsError(t *testing.T) {
	_, err := verifySharePassword("not a phc string", "anything")
	if !errors.Is(err, ErrInvalidSharePasswordHash) {
		t.Errorf("verifySharePassword(malformed hash) error = %v, want ErrInvalidSharePasswordHash", err)
	}
}

// phcForTest encodes password as a PHC string under the caller's cost
// parameters -- the shape a hash written by a different configuration (a
// future cost bump, another tool) takes. The fixed 16-byte salt keeps the
// test deterministic; only the parameter triple varies.
func phcForTest(password string, memory uint32, iterations uint32, parallel uint8, paramsField string) string {
	digest := argon2.IDKey([]byte(password), []byte("0123456789abcdef"), iterations, memory, parallel, 32)
	enc := base64.RawStdEncoding
	if paramsField == "" {
		return fmt.Sprintf("$argon2id$v=%d$$%s$%s", argon2.Version, enc.EncodeToString([]byte("0123456789abcdef")), enc.EncodeToString(digest))
	}
	return fmt.Sprintf("$argon2id$v=%d$%s$%s$%s", argon2.Version, paramsField, enc.EncodeToString([]byte("0123456789abcdef")), enc.EncodeToString(digest))
}

// TestVerifySharePassword_VerifiesUnderStoredCostParams pins the reading
// side's contract: the digest is re-derived under the parameters recorded
// INSIDE the stored PHC string, never under this package's own constants.
// A stored hash written under different cost parameters (m=4096,t=1,p=1
// here, far below the package's m=19456,t=2 floor) verified under the
// package constants would recompute a different digest and report "wrong
// password" for the correct one -- failing before the fix, passing after.
func TestVerifySharePassword_VerifiesUnderStoredCostParams(t *testing.T) {
	const password = "same password, other cost parameters"
	phc := phcForTest(password, 4096, 1, 1, "m=4096,t=1,p=1")

	ok, err := verifySharePassword(phc, password)
	if err != nil {
		t.Fatalf("verifySharePassword: %v", err)
	}
	if !ok {
		t.Errorf("verifySharePassword(correct password, stored params m=4096,t=1,p=1) = false, want true -- the stored parameters must be honored, not the package constants")
	}

	// And the same hash still refuses a wrong password, under the stored
	// parameters.
	ok, err = verifySharePassword(phc, "not the password")
	if err != nil {
		t.Fatalf("verifySharePassword(wrong password): %v", err)
	}
	if ok {
		t.Errorf("verifySharePassword(wrong password, stored params) = true, want false")
	}
}

// TestVerifySharePassword_ParamsMissingFromPHC_FallsBackToPackageConstants
// pins the fallback for a stored value whose parameter field is empty -- a
// shape this module's own writer never produces, but one the reading side
// must still be able to check: with no recorded parameters there is nothing
// to re-derive under, so the package's own constants are the only set the
// digest can plausibly have been produced under. A digest actually produced
// under those constants verifies; one produced under other parameters does
// not.
func TestVerifySharePassword_ParamsMissingFromPHC_FallsBackToPackageConstants(t *testing.T) {
	const password = "password under package constants"

	ownDigest := phcForTest(password, sharePasswordMemory, sharePasswordIterations, sharePasswordParallel, "")
	ok, err := verifySharePassword(ownDigest, password)
	if err != nil {
		t.Fatalf("verifySharePassword(params-less, digest under package constants): %v", err)
	}
	if !ok {
		t.Errorf("verifySharePassword(params-less value under package constants) = false, want true -- the constants are the documented fallback")
	}

	otherDigest := phcForTest(password, 4096, 1, 1, "")
	ok, err = verifySharePassword(otherDigest, password)
	if err != nil {
		t.Fatalf("verifySharePassword(params-less, digest under other params): %v", err)
	}
	if ok {
		t.Errorf("verifySharePassword(params-less value under other params) = true, want false -- nothing can verify it without its parameters")
	}
}

// TestVerifySharePassword_RefusesParamsArgon2CannotRunWith pins the
// reading-side guard against a stored parameter triple argon2 would PANIC
// on (t=0, p=0) or could never have been legitimately produced under
// (m=0): such a value decodes to ErrInvalidSharePasswordHash instead of
// reaching argon2.IDKey, mirroring authn.decodePHC's identical refusal.
func TestVerifySharePassword_RefusesParamsArgon2CannotRunWith(t *testing.T) {
	const password = "password"
	for name, field := range map[string]string{
		"zero iterations":  "m=19456,t=0,p=1",
		"zero parallelism": "m=19456,t=2,p=0",
		"zero memory":      "m=0,t=2,p=1",
	} {
		phc := phcForTest(password, 4096, 2, 1, field)
		if _, err := verifySharePassword(phc, password); !errors.Is(err, ErrInvalidSharePasswordHash) {
			t.Errorf("%s: verifySharePassword error = %v, want ErrInvalidSharePasswordHash", name, err)
		}
	}
}

// TestVerifySharePassword_RefusesAbsurdCostParams pins the upper half of
// the reading-side parameter guard (see maxSharePasswordMemoryKiB's own
// doc comment, password.go) through the verification entry point: a
// stored PHC string whose cost parameters parse but are absurdly large is
// refused as ErrInvalidSharePasswordHash BEFORE any argon2.IDKey call,
// mirroring the maxPHCFieldBytes discipline the salt and digest already
// carry. The parameters travel in the stored value by design, so a
// corrupt or hostile row could carry anything up to the parse's own
// uint32 ceiling -- m=4294967295 would make argon2.IDKey attempt a ~4 TiB
// allocation on the request goroutine. Each case here uses a memory cost
// just past the cap (the smallest over-cap value is 1 GiB, so a reader
// without this guard's allocation attempt stays survivable under an
// address-space limit -- the m=4294967295 marquee case is pinned at the
// decode level in TestDecodeSharePasswordPHC_RefusesAbsurdCostParams
// instead, where the unguarded attempt is the ~4 TiB allocation itself).
// The digest
// bytes are those of a real argon2 derivation under the sane m=4096
// parameters, so the only thing wrong with these values is the cost field.
func TestVerifySharePassword_RefusesAbsurdCostParams(t *testing.T) {
	const password = "password"
	enc := base64.RawStdEncoding
	salt := enc.EncodeToString([]byte("0123456789abcdef"))
	digest := enc.EncodeToString(argon2.IDKey([]byte(password), []byte("0123456789abcdef"), 2, 4096, 1, 32))

	for name, field := range map[string]string{
		"memory one past the cap": fmt.Sprintf("m=%d,t=2,p=1", maxSharePasswordMemoryKiB+1),
		"memory far past the cap": "m=1073741824,t=2,p=1", // 1 TiB
	} {
		phc := fmt.Sprintf("$argon2id$v=%d$%s$%s$%s", argon2.Version, field, salt, digest)
		if _, err := verifySharePassword(phc, password); !errors.Is(err, ErrInvalidSharePasswordHash) {
			t.Errorf("%s: verifySharePassword error = %v, want ErrInvalidSharePasswordHash -- an absurd cost must be refused before any allocation", name, err)
		}
	}
}

// TestDecodeSharePasswordPHC_RefusesAbsurdCostParams pins the refusal at
// the decode layer itself, where it actually happens -- and where every
// hostile case can be exercised without argon2.IDKey ever being reached
// (decode never allocates), so the fail-before run is the clean assertion
// failure "the parser accepted m=4294967295", not a ~4 TiB allocation
// attempt. That attempt is what a reader without this guard would
// attempt: m at the
// uint32 ceiling makes argon2.IDKey allocate roughly 4 TiB, and t at the
// ceiling would loop effectively forever -- both are exactly what these
// refusals exist to keep from ever starting.
func TestDecodeSharePasswordPHC_RefusesAbsurdCostParams(t *testing.T) {
	enc := base64.RawStdEncoding
	salt := enc.EncodeToString([]byte("0123456789abcdef"))
	digest := enc.EncodeToString(argon2.IDKey([]byte("password"), []byte("0123456789abcdef"), 2, 4096, 1, 32))

	for name, field := range map[string]string{
		"memory at the uint32 ceiling":        fmt.Sprintf("m=%d,t=2,p=1", uint32(math.MaxUint32)),
		"iterations at the uint32 ceiling":    fmt.Sprintf("m=19456,t=%d,p=1", uint32(math.MaxUint32)),
		"memory far past the cap":             "m=1073741824,t=2,p=1", // 1 TiB
		"iterations one past the cap":         fmt.Sprintf("m=19456,t=%d,p=1", maxSharePasswordIterations+1),
		"iterations far past the cap":         "m=19456,t=1000000,p=1",
		"parallelism past its own type bound": "m=19456,t=2,p=256",
	} {
		phc := fmt.Sprintf("$argon2id$v=%d$%s$%s$%s", argon2.Version, field, salt, digest)
		if _, _, _, err := decodeSharePasswordPHC(phc); !errors.Is(err, ErrInvalidSharePasswordHash) {
			t.Errorf("%s: decodeSharePasswordPHC error = %v, want ErrInvalidSharePasswordHash -- an absurd cost must be refused at decode, before any allocation", name, err)
		}
	}
}

func TestVerifySharePassword_RefusesUnreadableVersion(t *testing.T) {
	const password = "password"
	// A version argon2.IDKey does not compute with can never be
	// re-derived: the stored v=18 (argon2 v1.0) triple is refused as
	// corrupt rather than silently answered "wrong password".
	digest := argon2.IDKey([]byte(password), []byte("0123456789abcdef"), 2, 4096, 1, 32)
	enc := base64.RawStdEncoding
	phc := fmt.Sprintf("$argon2id$v=18$m=4096,t=2,p=1$%s$%s",
		enc.EncodeToString([]byte("0123456789abcdef")), enc.EncodeToString(digest))
	if _, err := verifySharePassword(phc, password); !errors.Is(err, ErrInvalidSharePasswordHash) {
		t.Errorf("verifySharePassword(v=18) error = %v, want ErrInvalidSharePasswordHash", err)
	}
}

// TestService_Access_PasswordHashWrittenUnderOtherParams_StillVerifies is
// the service-level proof of the same contract: a share whose stored
// PasswordHash was produced under cost parameters different from this
// package's own writer constants (here m=4096,t=1,p=1 -- a future cost
// bump's shape) still grants its access with the correct password, because
// Service.Access verifies through verifySharePassword's
// stored-parameters re-derivation. Before the fix the reader recomputed
// the digest under the package constants, so the correct password was
// answered as wrong and the access denied.
func TestService_Access_PasswordHashWrittenUnderOtherParams_StillVerifies(t *testing.T) {
	svc, _ := newTestService(t, nil)
	password := "stored under other cost params"
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r", Password: &password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	share, err := svc.Shares().FindByID(testCtx(), created.Share.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	alt := phcForTest(password, 4096, 1, 1, "m=4096,t=1,p=1")
	share.PasswordHash = &alt
	if updateErr := svc.Shares().Update(testCtx(), share); updateErr != nil {
		t.Fatalf("Update(storing the alt-params hash): %v", err)
	}

	if _, accessErr := svc.Access(testCtx(), created.Token, AccessParams{Password: &password}); accessErr != nil {
		t.Fatalf("Access(correct password, hash stored under m=4096,t=1,p=1): %v -- the stored parameters must be honored, not the package constants", accessErr)
	}

	wrong := "wrong guess"
	_, err = svc.Access(testCtx(), created.Token, AccessParams{Password: &wrong})
	assertCode(t, err, ErrNotAccessible.Code)
}
