package storage

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/storage/migrations"
)

const (
	testTenantID = "11111111-1111-4111-8111-111111111111"
	testObjectID = "22222222-2222-4222-8222-222222222222"
)

// TestObjectKey_BuildsTheCanonicalShape pins the key shape the module's
// doc comment promises: "<tenantID>/<objectID>/original". This is the shape
// the service round writes bytes to and the sweep round deletes them from;
// changing it without changing every consumer of those bytes would orphan
// stored content.
func TestObjectKey_BuildsTheCanonicalShape(t *testing.T) {
	key, err := ObjectKey(pkgcore.TenantID(testTenantID), testObjectID)
	if err != nil {
		t.Fatalf("ObjectKey: %v", err)
	}
	want := testTenantID + "/" + testObjectID + "/original"
	if key != want {
		t.Errorf("ObjectKey = %q, want %q", key, want)
	}
}

// TestDerivativeKey_BuildsTheCanonicalShape pins the derivative shape:
// "<tenantID>/<objectID>/derivatives/<kind>", with "derivatives" as the
// literal penultimate segment.
func TestDerivativeKey_BuildsTheCanonicalShape(t *testing.T) {
	key, err := DerivativeKey(pkgcore.TenantID(testTenantID), testObjectID, DerivativeKindThumbnail)
	if err != nil {
		t.Fatalf("DerivativeKey: %v", err)
	}
	want := testTenantID + "/" + testObjectID + "/derivatives/" + DerivativeKindThumbnail
	if key != want {
		t.Errorf("DerivativeKey = %q, want %q", key, want)
	}
}

// TestKeyBuilder_IsDeterministic pins the promise the key grammar is built
// on: the same tenant, object and kind always map to the same key, so a
// retried upload resumes against the exact bytes a previous attempt wrote.
func TestKeyBuilder_IsDeterministic(t *testing.T) {
	tenant := pkgcore.TenantID(testTenantID)
	first, err := ObjectKey(tenant, testObjectID)
	if err != nil {
		t.Fatalf("ObjectKey: %v", err)
	}
	for range 3 {
		again, repeatErr := ObjectKey(tenant, testObjectID)
		if repeatErr != nil {
			t.Fatalf("ObjectKey on repeat call: %v", repeatErr)
		}
		if again != first {
			t.Fatalf("ObjectKey is not deterministic: %q then %q", first, again)
		}
	}

	firstD, err := DerivativeKey(tenant, testObjectID, DerivativeKindThumbnail)
	if err != nil {
		t.Fatalf("DerivativeKey: %v", err)
	}
	for range 3 {
		again, err := DerivativeKey(tenant, testObjectID, DerivativeKindThumbnail)
		if err != nil {
			t.Fatalf("DerivativeKey on repeat call: %v", err)
		}
		if again != firstD {
			t.Fatalf("DerivativeKey is not deterministic: %q then %q", firstD, again)
		}
	}
}

// TestKeyLengthBoundary_512Accepted513Refused exercises the total-length
// rule at its exact edge: a key of 512 bytes (255-byte tenant, 247-byte
// object id, "/original": 255+1+247+1+8) is legal, and one byte more is
// rejected with ErrInvalidKey.
func TestKeyLengthBoundary_512Accepted513Refused(t *testing.T) {
	tenant := pkgcore.TenantID(strings.Repeat("a", 255))
	key, err := ObjectKey(tenant, strings.Repeat("b", 247))
	if err != nil {
		t.Fatalf("ObjectKey at exactly 512 bytes: %v", err)
	}
	if len(key) != 512 {
		t.Fatalf("boundary key is %d bytes, want 512", len(key))
	}

	if _, err := ObjectKey(tenant, strings.Repeat("b", 248)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("ObjectKey at 513 bytes error = %v, want ErrInvalidKey", err)
	}
}

// TestKeySegmentLengthBoundary_255Accepted256Refused exercises the
// per-segment rule at its edge: a 255-byte segment is legal even inside a
// key whose total length is far under 512, and a 256-byte segment is
// rejected for the segment rule, not the total-length rule.
func TestKeySegmentLengthBoundary_255Accepted256Refused(t *testing.T) {
	tenant := pkgcore.TenantID(strings.Repeat("a", 255))
	if _, err := ObjectKey(tenant, testObjectID); err != nil {
		t.Fatalf("ObjectKey with a 255-byte tenant segment: %v", err)
	}

	over := pkgcore.TenantID(strings.Repeat("a", 256))
	if _, err := ObjectKey(over, testObjectID); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("ObjectKey with a 256-byte tenant segment error = %v, want ErrInvalidKey", err)
	}
}

// TestKeyValidator_RejectsMalformedKeys is the grammar's rejection table:
// every input that must be refused, exercised through the same validator
// the builders call, so the rules live in exactly one enforced place.
func TestKeyValidator_RejectsMalformedKeys(t *testing.T) {
	longSegment := strings.Repeat("x", 256)
	longKey := strings.Repeat("y", keyMaxLen+1)

	tests := []struct {
		name string
		key  string
	}{
		{"empty key", ""},
		{"empty leading segment", "/a/b"},
		{"empty middle segment", "a//b"},
		{"empty trailing segment", "a/b/"},
		{"single empty segment key", "/"},
		{"dot segment", "a/./b"},
		{"leading dot segment", "./a"},
		{"dot-dot segment", "a/../b"},
		{"whole key is a dot", "."},
		{"whole key is a dot-dot", ".."},
		{"backslash inside a segment", `a\b/c`},
		{"NUL byte inside a segment", "a\x00b/c"},
		{"segment over 255 bytes", longSegment + "/b"},
		{"key over 512 bytes", longKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateKey(tc.key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("validateKey(%q) error = %v, want ErrInvalidKey", tc.key, err)
			}
		})
	}
}

// TestKeyBuilder_RejectsMalformedComponents proves the builders refuse the
// same grammar their joined output is validated against: an empty or dot
// tenant, object id or kind must not silently produce a key that means
// something else than the caller asked for.
func TestKeyBuilder_RejectsMalformedComponents(t *testing.T) {
	tenant := pkgcore.TenantID(testTenantID)

	tests := []struct {
		name     string
		tenant   pkgcore.TenantID
		objectID string
		kind     string
		build    func(pkgcore.TenantID, string, string) (string, error)
	}{
		{"empty tenant", "", testObjectID, "", func(t pkgcore.TenantID, o, _ string) (string, error) {
			return ObjectKey(t, o)
		}},
		{"empty object id", tenant, "", "", func(t pkgcore.TenantID, o, _ string) (string, error) {
			return ObjectKey(t, o)
		}},
		{"dot-dot tenant", "..", testObjectID, "", func(t pkgcore.TenantID, o, _ string) (string, error) {
			return ObjectKey(t, o)
		}},
		{"empty kind", tenant, testObjectID, "", func(t pkgcore.TenantID, o, k string) (string, error) {
			return DerivativeKey(t, o, k)
		}},
		{"dot kind", tenant, testObjectID, ".", func(t pkgcore.TenantID, o, k string) (string, error) {
			return DerivativeKey(t, o, k)
		}},
		{"kind over 255 bytes", tenant, testObjectID, strings.Repeat("k", 256), func(t pkgcore.TenantID, o, k string) (string, error) {
			return DerivativeKey(t, o, k)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.build(tc.tenant, tc.objectID, tc.kind); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("key builder error = %v, want ErrInvalidKey", err)
			}
		})
	}
}

// TestKeyMaxLen_MatchesTheKeyColumns pins keyMaxLen to the schema contract
// its own doc comment promises: the value equals the VARCHAR(512) width of
// every key column in every dialect's migration, so a key the grammar
// accepts always fits the column. The migration files are asserted directly
// because they are what a deployment actually runs.
func TestKeyMaxLen_MatchesTheKeyColumns(t *testing.T) {
	column := regexp.MustCompile(`^\s*key\s+VARCHAR\(([0-9]+)\)`)
	files := []string{
		"sqlite/0001_create_objects.sql",
		"sqlite/0002_create_object_derivatives.sql",
		"postgres/0001_create_objects.sql",
		"postgres/0002_create_object_derivatives.sql",
	}

	checked := 0
	for _, file := range files {
		raw, err := migrations.FS.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			match := column.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			width, err := strconv.Atoi(match[1])
			if err != nil {
				t.Fatalf("parse width in %s line %q: %v", file, line, err)
			}
			checked++
			if width != keyMaxLen {
				t.Errorf("%s declares a key width of %d, want keyMaxLen (%d)", file, width, keyMaxLen)
			}
		}
	}
	if checked != 4 {
		t.Errorf("found %d key columns across the migration files, want exactly 4 (one per file)", checked)
	}
}

// TestErrInvalidKey_IsAPlainError pins the one exception to the module's
// apperr rule: key violations are programmer errors in keys this module's
// own code builds, unreachable by any API consumer, so ErrInvalidKey is a
// plain sentinel with no code, no status and no locale entry (errors.go's
// own header records this).
func TestErrInvalidKey_IsAPlainError(t *testing.T) {
	if appErr, ok := apperr.As(ErrInvalidKey); ok {
		t.Errorf("ErrInvalidKey is an *apperr.Error (%+v); it must be a plain error (key.go's doc comment)", appErr)
	}
}
