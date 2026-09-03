package totp

import (
	"crypto/sha1" //nolint:gosec // required by the RFC 4226/6238 test vectors this file pins against.
	"crypto/sha256"
	"crypto/sha512"
	"hash"
	"strings"
	"testing"
	"time"
)

// rfc4226Secret is the exact 20-byte ASCII secret RFC 4226 Appendix D uses
// for every one of its test vectors: "12345678901234567890".
var rfc4226Secret = []byte("12345678901234567890")

// TestHOTP_RFC4226AppendixD pins the generic HOTP core against every one of
// RFC 4226 Appendix D's official (counter, code) pairs -- SHA-1, 6 digits,
// the ASCII secret above -- so a change to hotp's truncation or its digit
// modulus cannot silently drift from the standard.
func TestHOTP_RFC4226AppendixD(t *testing.T) {
	t.Parallel()

	vectors := []struct {
		counter uint64
		want    string
	}{
		{0, "755224"},
		{1, "287082"},
		{2, "359152"},
		{3, "969429"},
		{4, "338314"},
		{5, "254676"},
		{6, "287922"},
		{7, "162583"},
		{8, "399871"},
		{9, "520489"},
	}

	for _, v := range vectors {
		if got := hotp(rfc4226Secret, v.counter, 6, sha1.New); got != v.want {
			t.Errorf("hotp(counter=%d) = %q, want %q", v.counter, got, v.want)
		}
	}
}

// TestTOTP_RFC6238AppendixB pins the generic core, combined with
// counterAt's 30-second time step, against RFC 6238 Appendix B's official
// (time, code) triples for all three algorithms it specifies -- SHA-1,
// SHA-256 and SHA-512, all 8 digits. The public Code/Validate API only ever
// uses SHA-1 (see the package doc comment for why), so SHA-256/SHA-512 are
// exercised here, directly against hotp and counterAt, and nowhere else.
func TestTOTP_RFC6238AppendixB(t *testing.T) {
	t.Parallel()

	// RFC 6238 Appendix B's three secrets are the ASCII strings
	// "12345678901234567890" (SHA-1), repeated to 32 bytes for SHA-256 and
	// to 64 bytes for SHA-512, per the RFC's own footnote.
	secret1 := []byte("12345678901234567890")
	secret256 := repeatTo(secret1, 32)
	secret512 := repeatTo(secret1, 64)

	vectors := []struct {
		unixTime int64
		newHash  func() hash.Hash
		secret   []byte
		want     string
	}{
		{59, sha1.New, secret1, "94287082"},
		{59, sha256.New, secret256, "46119246"},
		{59, sha512.New, secret512, "90693936"},
		{1111111109, sha1.New, secret1, "07081804"},
		{1111111109, sha256.New, secret256, "68084774"},
		{1111111109, sha512.New, secret512, "25091201"},
		{1111111111, sha1.New, secret1, "14050471"},
		{1111111111, sha256.New, secret256, "67062674"},
		{1111111111, sha512.New, secret512, "99943326"},
		{1234567890, sha1.New, secret1, "89005924"},
		{1234567890, sha256.New, secret256, "91819424"},
		{1234567890, sha512.New, secret512, "93441116"},
		{2000000000, sha1.New, secret1, "69279037"},
		{2000000000, sha256.New, secret256, "90698825"},
		{2000000000, sha512.New, secret512, "38618901"},
	}

	for _, v := range vectors {
		counter := counterAt(time.Unix(v.unixTime, 0).UTC(), 30*time.Second)
		if got := hotp(v.secret, counter, 8, v.newHash); got != v.want {
			t.Errorf("hotp(unixTime=%d) = %q, want %q", v.unixTime, got, v.want)
		}
	}
}

// TestValidate_CurrentCode_Accepted proves the public, fixed-convention API
// (SHA-1/6-digits/30s, via Code and Validate) agrees with itself: a code
// generated for "now" validates against "now".
func TestValidate_CurrentCode_Accepted(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}
	code, err := Code(secret, time.Now())
	if err != nil {
		t.Fatalf("Code() error = %v", err)
	}

	ok, step := Validate(secret, code, 1)
	if !ok {
		t.Fatalf("Validate(current code) = false, want true")
	}
	if step == 0 {
		t.Errorf("Validate() step = 0, want the matched counter")
	}
}

// TestValidate_WrongCode_Rejected proves a code that cannot possibly match
// (all zeros, vanishingly unlikely to collide with the real one) is refused.
func TestValidate_WrongCode_Rejected(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}
	real, err := Code(secret, time.Now())
	if err != nil {
		t.Fatalf("Code() error = %v", err)
	}
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	if ok, _ := Validate(secret, wrong, 1); ok {
		t.Fatalf("Validate(wrong code) = true, want false")
	}
}

// TestValidate_AdjacentStep_AcceptedWithinSkew proves the skew window
// tolerates ordinary clock drift: a code from one step in the past or the
// future still validates when skew allows it, and is refused at skew 0.
func TestValidate_AdjacentStep_AcceptedWithinSkew(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}
	previousStep := time.Now().Add(-Period)
	code, err := Code(secret, previousStep)
	if err != nil {
		t.Fatalf("Code() error = %v", err)
	}

	if ok, _ := Validate(secret, code, 0); ok {
		t.Fatalf("Validate(previous-step code, skew=0) = true, want false")
	}
	if ok, step := Validate(secret, code, 1); !ok {
		t.Fatalf("Validate(previous-step code, skew=1) = false, want true")
	} else if step >= int64(counterAt(time.Now(), Period)) {
		t.Errorf("Validate() step = %d, want the PREVIOUS step, not the current one", step)
	}
}

// TestValidate_InvalidSecret_Rejected proves a secret that cannot even be
// base32-decoded fails closed rather than panicking.
func TestValidate_InvalidSecret_Rejected(t *testing.T) {
	t.Parallel()

	if ok, step := Validate("not-valid-base32!!!", "123456", 1); ok || step != 0 {
		t.Fatalf("Validate(invalid secret) = (%v, %d), want (false, 0)", ok, step)
	}
	if _, err := Code("not-valid-base32!!!", time.Now()); err == nil {
		t.Fatalf("Code(invalid secret) error = nil, want ErrInvalidSecret")
	}
}

// TestDecodeSecret_TolerantOfHumanRetyping proves a secret typed back in by
// a person -- lower case, spaced in groups of four, with trailing padding --
// still decodes to exactly what GenerateSecret produced.
func TestDecodeSecret_TolerantOfHumanRetyping(t *testing.T) {
	t.Parallel()

	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}
	want, err := decodeSecret(secret)
	if err != nil {
		t.Fatalf("decodeSecret(canonical) error = %v", err)
	}

	spaced := strings.ToLower(insertSpacesEveryFour(secret)) + "=="
	got, err := decodeSecret(spaced)
	if err != nil {
		t.Fatalf("decodeSecret(retyped) error = %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("decodeSecret(retyped) = %x, want %x", got, want)
	}
}

// TestProvisioningURI_CarriesTheFixedConvention pins the query parameters a
// scanning app relies on, so a refactor cannot silently drop one.
func TestProvisioningURI_CarriesTheFixedConvention(t *testing.T) {
	t.Parallel()

	uri := ProvisioningURI("speed", "user@example.com", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{
		"otpauth://totp/",
		"secret=JBSWY3DPEHPK3PXP",
		"issuer=speed",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("ProvisioningURI() = %q, want it to contain %q", uri, want)
		}
	}
}

// TestGenerateSecret_ReturnsDistinctValidSecrets proves two calls draw
// independent randomness and every result decodes cleanly.
func TestGenerateSecret_ReturnsDistinctValidSecrets(t *testing.T) {
	t.Parallel()

	a, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}
	if a == b {
		t.Fatalf("GenerateSecret() returned the same value twice: %q", a)
	}
	if _, err := decodeSecret(a); err != nil {
		t.Errorf("decodeSecret(GenerateSecret()) error = %v", err)
	}
}

// repeatTo repeats seed until it is at least n bytes long, then truncates to
// exactly n -- RFC 6238 Appendix B's own footnote for building its SHA-256
// and SHA-512 secrets out of the SHA-1 one.
func repeatTo(seed []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, seed...)
	}
	return out[:n]
}

// insertSpacesEveryFour is a small test helper that mimics how a person
// reads a secret off a screen back to an authenticator app: in groups of
// four characters.
func insertSpacesEveryFour(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
