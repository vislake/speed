package dbkit

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

func TestBlindIndex_Deterministic(t *testing.T) {
	key := testKey("blind-index-deterministic")

	tests := []struct {
		name       string
		normalized string
	}{
		{name: "e164 phone number", normalized: "+15550100001"},
		{name: "lowercased email", normalized: "user@example.com"},
		{name: "empty string", normalized: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := BlindIndex(key, tt.normalized)
			for i := 0; i < 5; i++ {
				if got := BlindIndex(key, tt.normalized); got != want {
					t.Fatalf("BlindIndex() call %d = %q, want %q (must be deterministic for the same key and input)", i, got, want)
				}
			}
		})
	}
}

// TestBlindIndex_DistinctInputs_NoCollisions checks a reasonably sized
// sample of distinct normalized inputs under one key and requires every
// index to be unique, guarding against a trivially broken implementation
// (e.g. one that truncates its input or its output).
func TestBlindIndex_DistinctInputs_NoCollisions(t *testing.T) {
	key := testKey("blind-index-collisions")

	const sampleSize = 500
	seen := make(map[string]string, sampleSize)
	for i := 0; i < sampleSize; i++ {
		normalized := fmt.Sprintf("+1555%07d", i)
		index := BlindIndex(key, normalized)
		if prior, exists := seen[index]; exists {
			t.Fatalf("BlindIndex collision: %q and %q both produced index %q", prior, normalized, index)
		}
		seen[index] = normalized
	}
}

// TestBlindIndex_DifferentKeys_ProduceDifferentOutputs proves the key
// parameter is actually mixed into the computation, not ignored.
func TestBlindIndex_DifferentKeys_ProduceDifferentOutputs(t *testing.T) {
	const normalized = "+15550100999"

	indexA := BlindIndex(testKey("blind-index-key-a"), normalized)
	indexB := BlindIndex(testKey("blind-index-key-b"), normalized)

	if indexA == indexB {
		t.Fatalf("BlindIndex() with two different keys produced the same index %q for the same input; the key is not being used", indexA)
	}
}

func TestNormalizeEmail_CanonicalForms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "already canonical", raw: "user@example.com", want: "user@example.com"},
		{name: "uppercase is lowercased", raw: "User@Example.COM", want: "user@example.com"},
		{name: "surrounding whitespace is trimmed", raw: "  User@Example.COM  ", want: "user@example.com"},
		{name: "internal whitespace preserved", raw: "a@b.c d", want: "a@b.c d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEmail(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeEmail(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.raw, got, tt.want)
			}
			// Idempotence: normalizing an already-normalized value changes
			// nothing, so write-side and query-side normalization can never
			// drift apart across repeated application.
			again, err := NormalizeEmail(got)
			if err != nil {
				t.Fatalf("NormalizeEmail(NormalizeEmail(%q)) error = %v", tt.raw, err)
			}
			if again != got {
				t.Errorf("NormalizeEmail(NormalizeEmail(%q)) = %q, want %q (normalization must be idempotent)", tt.raw, again, got)
			}
		})
	}

	for _, raw := range []string{"", "   "} {
		t.Run(fmt.Sprintf("rejects %q", raw), func(t *testing.T) {
			if got, err := NormalizeEmail(raw); err == nil {
				t.Errorf("NormalizeEmail(%q) = %q, nil error; want an error (an absent value has no canonical form)", raw, got)
			}
		})
	}
}

func TestNormalizePhoneE164_CanonicalForms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "already canonical", raw: "+15550100", want: "+15550100"},
		{name: "spaces dropped", raw: "+1 555 0100", want: "+15550100"},
		{name: "spaces and dashes dropped", raw: "+1 555-0100", want: "+15550100"},
		{name: "parentheses dropped", raw: "+1 (555) 0100", want: "+15550100"},
		{name: "dots dropped", raw: "+1.555.0100", want: "+15550100"},
		{name: "uk number with spaces", raw: "+44 20 7946 0958", want: "+442079460958"},
		{name: "surrounding whitespace trimmed", raw: "  +1-555-0100  ", want: "+15550100"},
		{name: "fifteen digits accepted (E.164 limit)", raw: "+123456789012345", want: "+123456789012345"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizePhoneE164(tt.raw)
			if err != nil {
				t.Fatalf("NormalizePhoneE164(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("NormalizePhoneE164(%q) = %q, want %q", tt.raw, got, tt.want)
			}
			again, err := NormalizePhoneE164(got)
			if err != nil {
				t.Fatalf("NormalizePhoneE164(NormalizePhoneE164(%q)) error = %v", tt.raw, err)
			}
			if again != got {
				t.Errorf("NormalizePhoneE164(NormalizePhoneE164(%q)) = %q, want %q (normalization must be idempotent)", tt.raw, again, got)
			}
		})
	}

	// Rejections: each case would silently compute a different index than a
	// legitimate write if it were "best-effort fixed up" instead.
	testsErr := []struct {
		name    string
		raw     string
		reasons []string // stable fragments of the expected error message
	}{
		{name: "empty", raw: "", reasons: []string{"input is empty"}},
		{name: "whitespace only", raw: "  ", reasons: []string{"input is empty"}},
		{name: "national number without country code", raw: "15550100", reasons: []string{"missing leading", "country code"}},
		{name: "leading plus only", raw: "+", reasons: []string{"no digits"}},
		{name: "separators only", raw: "+--() ", reasons: []string{"no digits"}},
		{name: "dialing extension", raw: "+1 555-0100 ext 42", reasons: []string{"unexpected character"}},
		{name: "letters", raw: "+1a5550100", reasons: []string{"unexpected character"}},
		{name: "second plus sign", raw: "+1+5550100", reasons: []string{"unexpected character"}},
		{name: "sixteen digits", raw: "+1234567890123456", reasons: []string{"more than 15 digits"}},
	}
	for _, tt := range testsErr {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			got, err := NormalizePhoneE164(tt.raw)
			if err == nil {
				t.Fatalf("NormalizePhoneE164(%q) = %q, nil error; want an error", tt.raw, got)
			}
			for _, reason := range tt.reasons {
				if !strings.Contains(err.Error(), reason) {
					t.Errorf("NormalizePhoneE164(%q) error = %q, want it to mention %q", tt.raw, err, reason)
				}
			}
		})
	}
}

func TestNewBlindIndexer_Validation(t *testing.T) {
	key := testKey("blind-indexer-valid")

	tests := []struct {
		name      string
		column    string
		key       []byte
		normalize NormalizeFunc
		wantErrIs error
	}{
		{name: "valid", column: "email_index", key: key, normalize: NormalizeEmail},
		{name: "empty column", column: "", key: key, normalize: NormalizeEmail},
		{name: "nil key", column: "email_index", normalize: NormalizeEmail, wantErrIs: ErrInvalidKeySize},
		{name: "short key", column: "email_index", key: make([]byte, 31), normalize: NormalizeEmail, wantErrIs: ErrInvalidKeySize},
		{name: "long key", column: "email_index", key: make([]byte, 33), normalize: NormalizeEmail, wantErrIs: ErrInvalidKeySize},
		{name: "nil normalizer", column: "email_index", key: key},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewBlindIndexer(tt.column, tt.key, tt.normalize)
			if tt.name == "valid" {
				if err != nil {
					t.Fatalf("NewBlindIndexer() error = %v", err)
				}
				if got == nil {
					t.Fatal("NewBlindIndexer() = nil, nil error")
				}
				return
			}
			if err == nil {
				t.Fatalf("NewBlindIndexer(%q, key[%d], normalize) = %+v, nil error; want an error", tt.column, len(tt.key), got)
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Errorf("NewBlindIndexer() error = %v, want it to wrap %v", err, tt.wantErrIs)
			}
		})
	}
}

// TestNewBlindIndexer_CopiesKey proves the caller's key slice is not
// retained: clobbering it after construction must not change the indexer's
// output. A retained slice would silently swap the blind-index secret out
// from under the column — and since BlindIndexer exposes no key accessor,
// the corruption would be invisible.
func TestNewBlindIndexer_CopiesKey(t *testing.T) {
	key := testKey("blind-indexer-copy-source")
	indexer, err := NewBlindIndexer("email_index", key, NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer() error = %v", err)
	}

	for i := range key {
		key[i] = 0 // clobber the caller's slice
	}

	got, err := indexer.Index("user@example.com")
	if err != nil {
		t.Fatalf("Index() after clobbering caller key error = %v", err)
	}
	want := BlindIndex(testKey("blind-indexer-copy-source"), "user@example.com")
	if got != want {
		t.Errorf("Index() after clobbering caller key = %q, want %q (the key must be copied at construction)", got, want)
	}
}

// TestBlindIndexer_Index_MatchesRawBlindIndex pins the mechanism layer to
// the raw primitive: Index(raw) must equal BlindIndex(key, normalize(raw)),
// so a row written through a BlindIndexer is findable by the same
// computation done by hand, and vice versa. It also proves the normalizer
// runs on the write side (messy input converges to the canonical index) and
// that the key is bound (a second key produces a different index).
func TestBlindIndexer_Index_MatchesRawBlindIndex(t *testing.T) {
	key := testKey("blind-indexer-mechanism-a")
	indexer, err := NewBlindIndexer("email_index", key, NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer() error = %v", err)
	}

	want := BlindIndex(key, "user@example.com")

	variants := []string{
		"user@example.com",
		"User@Example.COM",
		"  USER@example.com  ",
	}
	var got string
	for _, raw := range variants {
		got, err = indexer.Index(raw)
		if err != nil {
			t.Fatalf("Index(%q) error = %v", raw, err)
		}
		if got != want {
			t.Errorf("Index(%q) = %q, want %q (the column's normalizer must run on the write side, collapsing variants to one canonical index)", raw, got, want)
		}
	}

	otherKey := testKey("blind-indexer-mechanism-b")
	other, err := NewBlindIndexer("email_index", otherKey, NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer(other key) error = %v", err)
	}
	got, err = other.Index("user@example.com")
	if err != nil {
		t.Fatalf("Index() under the other key error = %v", err)
	}
	if got == want {
		t.Errorf("Index() under two different keys produced the same value %q; the key is not being used", got)
	}
}

func TestBlindIndexer_Equal_ConditionShape(t *testing.T) {
	key := testKey("blind-indexer-equal")
	indexer, err := NewBlindIndexer("email_index", key, NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer() error = %v", err)
	}

	cond, err := indexer.Equal("User@Example.COM")
	if err != nil {
		t.Fatalf("Equal() error = %v", err)
	}
	if cond.Column != "email_index" {
		t.Errorf("Equal().Column = %v, want %q (the condition must target the indexer's own column)", cond.Column, "email_index")
	}
	if want := BlindIndex(key, "user@example.com"); cond.Value != want {
		t.Errorf("Equal().Value = %v, want %q (the raw input must be normalized before hashing, exactly like Index)", cond.Value, want)
	}
}

// TestBlindIndexer_EmptyInput_Errors proves the fail-closed contract: an
// input with no canonical form is an error on both sides of the mechanism,
// write (Index) and query (Equal) — never a silently computed index for an
// empty or unnormalizable value, which could only ever match nothing (or
// worse, collude with a writer that stored an empty index).
func TestBlindIndexer_EmptyInput_Errors(t *testing.T) {
	indexer, err := NewBlindIndexer("email_index", testKey("blind-indexer-empty"), NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer() error = %v", err)
	}

	for _, raw := range []string{"", "   "} {
		if got, err := indexer.Index(raw); err == nil {
			t.Errorf("Index(%q) = %q, nil error; want an error", raw, got)
		} else if !strings.Contains(err.Error(), `blind index column "email_index"`) {
			t.Errorf("Index(%q) error = %q, want it to name the column", raw, err)
		}

		if got, err := indexer.Equal(raw); err == nil {
			t.Errorf("Equal(%q) = %+v, nil error; want an error", raw, got)
		} else if !strings.Contains(err.Error(), `blind index column "email_index"`) {
			t.Errorf("Equal(%q) error = %q, want it to name the column", raw, err)
		}
	}
}

// blindIndexAccount is the throwaway GORM model used by
// TestBlindIndexer_IndexAndEqual_GORMRoundTrip. Email is field-level
// encrypted through the serializer named by blindIndexAccountSerializerName;
// EmailIndex is the plain index column declared next to it, the exact shape
// docs/internal/10-compliance-and-audit.md and the BlindIndexer doc comment
// prescribe. The account itself carries no tenant — this fixture exercises
// the mechanism bare; tenant composition is tested separately with
// blindIndexTenantAccount below.
const blindIndexAccountSerializerName = "dbkit_test_blind_email_encrypted"

type blindIndexAccount struct {
	ID         uint   `gorm:"primaryKey"`
	Email      string `gorm:"column:email;serializer:dbkit_test_blind_email_encrypted"`
	EmailIndex string `gorm:"column:email_index;size:64;not null"`
}

// TableName pins the table name so it does not depend on GORM's
// pluralization of an unexported type name.
func (blindIndexAccount) TableName() string { return "dbkit_test_blind_index_accounts" }

const blindIndexAccountDDL = `
CREATE TABLE dbkit_test_blind_index_accounts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    email       BLOB NOT NULL,
    email_index VARCHAR(64) NOT NULL
);`

// TestBlindIndexer_IndexAndEqual_GORMRoundTrip exercises the mechanism end
// to end against a real SQLite database, in the exact arrangement the design
// prescribes: a field-level-encrypted Email column and, next to it, a plain
// EmailIndex column populated by the writer from Index(plaintext).
func TestBlindIndexer_IndexAndEqual_GORMRoundTrip(t *testing.T) {
	cipher, err := NewCipher(testKey("blind-account-encryption"))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	RegisterEncryptedSerializer(blindIndexAccountSerializerName, cipher)

	indexer, err := NewBlindIndexer("email_index", testKey("blind-account-key"), NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer() error = %v", err)
	}

	db := newEncryptionTestDB(t, blindIndexAccountDDL)

	// The write side, with deliberately messy input: the encrypted column
	// stores the value as given, while the index column stores the index of
	// the normalized value — proving the writer computed the index over the
	// plaintext it also encrypted, never over ciphertext, and never without
	// normalizing.
	const messy = " User@Example.COM "
	indexValue, err := indexer.Index(messy)
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	account := blindIndexAccount{Email: messy, EmailIndex: indexValue}
	if err = db.Create(&account).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// At rest: email holds ciphertext — decryptable with the cipher, never
	// the plaintext — and email_index holds exactly BlindIndex(key,
	// "user@example.com"), plain hex text a query can match against.
	var stored struct {
		Email      []byte
		EmailIndex string
	}
	if err = db.Raw(`SELECT email, email_index FROM dbkit_test_blind_index_accounts WHERE id = ?`, account.ID).Scan(&stored).Error; err != nil {
		t.Fatalf("raw at-rest read: %v", err)
	}
	decrypted, err := cipher.Decrypt(stored.Email)
	if err != nil {
		t.Errorf("cipher.Decrypt(email column) error = %v; the column must hold serializer ciphertext", err)
	} else if string(decrypted) != messy {
		t.Errorf("decrypted email column = %q, want the stored plaintext %q", decrypted, messy)
	}
	if bytes.Contains(stored.Email, []byte("example.com")) || bytes.Contains(stored.Email, []byte("User@")) {
		t.Errorf("email column %q contains plaintext fragments; the serializer must encrypt before writing", stored.Email)
	}
	if want := BlindIndex(testKey("blind-account-key"), "user@example.com"); stored.EmailIndex != want {
		t.Errorf("stored email_index = %q, want %q (the column must hold the index of the normalized value)", stored.EmailIndex, want)
	}

	// The query side: an equality lookup on the raw input — a differently
	// cased, whitespace-padded variant of the stored plaintext — finds the
	// row, and the model scans back with the encrypted field decrypted.
	cond, err := indexer.Equal(" USER@example.com ")
	if err != nil {
		t.Fatalf("Equal() error = %v", err)
	}
	var got blindIndexAccount
	if err = db.Where(cond).First(&got).Error; err != nil {
		t.Fatalf("First(Equal(variant)) error = %v; the variant must normalize to the stored index", err)
	}
	if got.ID != account.ID {
		t.Errorf("First(Equal(variant)) = %+v, want the row with id %d", got, account.ID)
	}
	if got.Email != messy {
		t.Errorf("First(Equal(variant)).Email = %q, want the stored plaintext %q", got.Email, messy)
	}

	// A second indexer bound to the same column under a different normalizer
	// must not match: its query value is the index of a different string, so
	// the lookup returns zero rows — the silent-empty-result shape the
	// mechanism makes impossible to hit except by deliberately constructing
	// that second indexer.
	wrongNormalizer := func(raw string) (string, error) { return raw, nil }
	other, err := NewBlindIndexer("email_index", testKey("blind-account-key"), wrongNormalizer)
	if err != nil {
		t.Fatalf("NewBlindIndexer(wrong normalizer) error = %v", err)
	}
	otherCond, err := other.Equal(" User@Example.COM ")
	if err != nil {
		t.Fatalf("Equal() under the wrong normalizer error = %v", err)
	}
	if err = db.Where(otherCond).First(&got).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("First(Equal under a different normalizer) error = %v, want gorm.ErrRecordNotFound (a lookup computed under different normalization must not match)", err)
	}

	// The index is deliberately not unique: two accounts with the same
	// email store the same index value, and an equality lookup matches both.
	duplicate := blindIndexAccount{Email: messy, EmailIndex: indexValue}
	if err = db.Create(&duplicate).Error; err != nil {
		t.Fatalf("Create(duplicate) error = %v", err)
	}
	var both []blindIndexAccount
	if err = db.Where(cond).Find(&both).Error; err != nil {
		t.Fatalf("Find(Equal) error = %v", err)
	}
	if len(both) != 2 {
		t.Errorf("Find(Equal) returned %d rows, want 2 (the index is an equality aid, not a uniqueness constraint)", len(both))
	}
}

// blindIndexTenantAccount is the tenant-scoped counterpart of
// blindIndexAccount: same encrypted Email plus plain EmailIndex layout, but
// with the (tenant_id, id) composite primary key every tenant-scoped table
// follows, so the mechanism can be exercised under tenantScopePlugin — the
// layer-1 guard that real business data-access goes through.
type blindIndexTenantAccount struct {
	TenantID   string `gorm:"primaryKey;size:26"`
	ID         string `gorm:"primaryKey;size:26"`
	Email      string `gorm:"column:email;serializer:dbkit_test_blind_email_encrypted"`
	EmailIndex string `gorm:"column:email_index;size:64;not null"`
}

// TableName pins the table name so it does not depend on GORM's
// pluralization of an unexported type name.
func (blindIndexTenantAccount) TableName() string { return "dbkit_test_blind_tenant_accounts" }

// GetTenantID gives blindIndexTenantAccount the GetTenantID() method that
// tenantScopePlugin requires of tenant-scoped models.
func (m blindIndexTenantAccount) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(m.TenantID)
}

const blindIndexTenantAccountDDL = `
CREATE TABLE dbkit_test_blind_tenant_accounts (
    tenant_id   VARCHAR(26) NOT NULL,
    id          VARCHAR(26) NOT NULL,
    email       BLOB NOT NULL,
    email_index VARCHAR(64) NOT NULL,
    PRIMARY KEY (tenant_id, id)
);`

// mustCreateBlindAccount seeds one blindIndexTenantAccount under tid,
// letting tenantScopePlugin populate the tenant_id column from the context.
func mustCreateBlindAccount(t *testing.T, db *gorm.DB, tid pkgcore.TenantID, id, email, indexValue string) {
	t.Helper()
	m := blindIndexTenantAccount{ID: id, Email: email, EmailIndex: indexValue}
	if err := db.WithContext(ctxFor(tid)).Create(&m).Error; err != nil {
		t.Fatalf("seed blind-index account %q under tenant %q: %v", id, tid, err)
	}
}

// TestBlindIndexer_Equal_TenantScopedModel_IsolationHolds proves the
// mechanism composes with the layer-1 tenant guard the way the design
// promises: the Equal condition carries no tenant filter of its own, and the
// surrounding plugin appends its own — so two tenants holding the same email
// (and therefore the identical index value, in the shared table) each see
// exactly their own row, and a context without a tenant fails closed rather
// than querying unfiltered.
func TestBlindIndexer_Equal_TenantScopedModel_IsolationHolds(t *testing.T) {
	cipher, err := NewCipher(testKey("blind-tenant-encryption"))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	RegisterEncryptedSerializer(blindIndexAccountSerializerName, cipher)

	key := testKey("blind-tenant-key")
	indexer, err := NewBlindIndexer("email_index", key, NormalizeEmail)
	if err != nil {
		t.Fatalf("NewBlindIndexer() error = %v", err)
	}

	db := newEncryptionTestDB(t, blindIndexTenantAccountDDL)
	if err = db.Use(newTenantScopePlugin()); err != nil {
		t.Fatalf("install tenantScopePlugin: %v", err)
	}

	// Both tenants hold the same email; its index value is identical across
	// the two rows — determinism and the shared-table layout guarantee it,
	// and the tenant filter is what keeps the two rows apart.
	const sharedEmail = "shared@example.com"
	indexValue, err := indexer.Index(sharedEmail)
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	mustCreateBlindAccount(t, db, tenantA, "acc-a", sharedEmail, indexValue)
	mustCreateBlindAccount(t, db, tenantB, "acc-b", sharedEmail, indexValue)

	cond, err := indexer.Equal(sharedEmail)
	if err != nil {
		t.Fatalf("Equal() error = %v", err)
	}

	var tenantAView []blindIndexTenantAccount
	if err = db.WithContext(ctxFor(tenantA)).Where(cond).Find(&tenantAView).Error; err != nil {
		t.Fatalf("Find(Equal) under tenant A error = %v", err)
	}
	if len(tenantAView) != 1 || tenantAView[0].ID != "acc-a" {
		t.Errorf("Find(Equal) under tenant A = %+v, want exactly [acc-a] (tenant A must not see tenant B's row despite the identical index value)", tenantAView)
	}

	var tenantBView []blindIndexTenantAccount
	if err = db.WithContext(ctxFor(tenantB)).Where(cond).Find(&tenantBView).Error; err != nil {
		t.Fatalf("Find(Equal) under tenant B error = %v", err)
	}
	if len(tenantBView) != 1 || tenantBView[0].ID != "acc-b" {
		t.Errorf("Find(Equal) under tenant B = %+v, want exactly [acc-b]", tenantBView)
	}

	// An email only tenant B holds, queried as tenant A: zero rows, not an
	// error and not tenant B's row.
	bOnly, err := indexer.Index("b-only@example.com")
	if err != nil {
		t.Fatalf("Index(b-only) error = %v", err)
	}
	mustCreateBlindAccount(t, db, tenantB, "acc-b2", "b-only@example.com", bOnly)
	bOnlyCond, err := indexer.Equal("b-only@example.com")
	if err != nil {
		t.Fatalf("Equal(b-only) error = %v", err)
	}
	var aLookup []blindIndexTenantAccount
	if err = db.WithContext(ctxFor(tenantA)).Where(bOnlyCond).Find(&aLookup).Error; err != nil {
		t.Fatalf("Find(Equal, other tenant's email) under tenant A error = %v", err)
	}
	if len(aLookup) != 0 {
		t.Errorf("Find(Equal, other tenant's email) under tenant A = %+v, want no rows", aLookup)
	}

	// No tenant in context: the plugin fails the query closed — the equality
	// condition alone never runs unfiltered.
	var nobody []blindIndexTenantAccount
	if err = db.Where(cond).Find(&nobody).Error; !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("Find(Equal) with no tenant context error = %v, want it to wrap pkgcore.ErrNoTenant (a blind-index equality lookup must never run tenant-unfiltered)", err)
	}
	if len(nobody) != 0 {
		t.Errorf("Find(Equal) with no tenant context returned %d rows alongside the error, want none", len(nobody))
	}
}
