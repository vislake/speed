package sharing

import (
	"errors"
	"strings"
	"testing"
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
