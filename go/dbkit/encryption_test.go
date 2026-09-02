package dbkit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// testKey derives a deterministic 32-byte AES-256 key from label. Distinct
// labels always produce distinct keys, which is all these tests need; using
// a hash instead of literal key bytes keeps the source free of anything that
// could be mistaken for a real secret.
func testKey(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

func TestNewCipher_KeyLength(t *testing.T) {
	valid := testKey("valid-active")

	tests := []struct {
		name        string
		activeKey   []byte
		retiredKeys [][]byte
		wantErr     bool
	}{
		{name: "32 byte active key alone", activeKey: valid, wantErr: false},
		{
			name:        "32 byte active key with 32 byte retired keys",
			activeKey:   valid,
			retiredKeys: [][]byte{testKey("retired-1"), testKey("retired-2")},
			wantErr:     false,
		},
		{name: "active key too short", activeKey: make([]byte, 16), wantErr: true},
		{name: "active key too long", activeKey: make([]byte, 64), wantErr: true},
		{name: "active key nil", activeKey: nil, wantErr: true},
		{
			name:        "one retired key wrong size",
			activeKey:   valid,
			retiredKeys: [][]byte{testKey("ok"), make([]byte, 10)},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewCipher(tt.activeKey, tt.retiredKeys...)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewCipher() error = nil, want a non-nil error")
				}
				if !errors.Is(err, ErrInvalidKeySize) {
					t.Fatalf("NewCipher() error = %v, want it to wrap ErrInvalidKeySize", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewCipher() unexpected error: %v", err)
			}
			if c == nil {
				t.Fatalf("NewCipher() returned a nil *Cipher with a nil error")
			}
		})
	}
}

func TestCipher_EncryptDecrypt_RoundTrip(t *testing.T) {
	c, err := NewCipher(testKey("roundtrip-active"))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	tests := []struct {
		name      string
		plaintext []byte
	}{
		{name: "empty plaintext", plaintext: []byte("")},
		{name: "ascii phone number", plaintext: []byte("+15550100123")},
		// Intentional CJK test data (a Chinese ID-number string) for the Unicode round-trip case below; string literals/test data are exempt from this repo's comments-and-docs-only CJK-language rule, so this is not an oversight.
		{name: "unicode text", plaintext: []byte("身份证号：110101199003072316")},
		{name: "binary data", plaintext: []byte{0x00, 0x01, 0xff, 0xfe, 0x10, 0x7f}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ciphertext, err := c.Encrypt(tt.plaintext)
			if err != nil {
				t.Fatalf("Encrypt() error = %v", err)
			}
			got, err := c.Decrypt(ciphertext)
			if err != nil {
				t.Fatalf("Decrypt() error = %v", err)
			}
			if !bytes.Equal(got, tt.plaintext) {
				t.Fatalf("round trip = %q, want %q", got, tt.plaintext)
			}
		})
	}
}

// TestCipher_Encrypt_NonceIsRandom proves Encrypt draws a fresh random nonce
// on every call rather than reusing one: encrypting the same plaintext
// repeatedly must never produce the same ciphertext twice. Nonce reuse under
// AES-GCM is a catastrophic, silent confidentiality break, so this is a real
// correctness property, not a cosmetic one.
func TestCipher_Encrypt_NonceIsRandom(t *testing.T) {
	c, err := NewCipher(testKey("nonce-randomness-active"))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	const calls = 10
	plaintext := []byte("same-plaintext-every-call")
	seen := make(map[string]bool, calls)

	for i := 0; i < calls; i++ {
		ciphertext, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt() call %d error = %v", i, err)
		}

		key := string(ciphertext)
		if seen[key] {
			t.Fatalf("Encrypt() produced the same ciphertext twice for identical plaintext (call %d); the nonce is not random", i)
		}
		seen[key] = true

		got, err := c.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("Decrypt() call %d error = %v", i, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("Decrypt() call %d = %q, want %q", i, got, plaintext)
		}
	}
}

// TestCipher_Decrypt_KeyNotHeld_Fails proves that a Cipher which holds
// neither the encrypting key as active nor as retired cannot decrypt the
// resulting ciphertext.
func TestCipher_Decrypt_KeyNotHeld_Fails(t *testing.T) {
	encryptor, err := NewCipher(testKey("holder-of-the-real-key"))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext, err := encryptor.Encrypt([]byte("sensitive-value"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	stranger, err := NewCipher(testKey("unrelated-active"), testKey("unrelated-retired"))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}

	if _, err := stranger.Decrypt(ciphertext); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("Decrypt() error = %v, want ErrDecryptionFailed", err)
	}
}

// TestCipher_Decrypt_AfterKeyRotation_OldCiphertextStillDecrypts models a
// full key rotation: data encrypted under the previous active key must keep
// decrypting once that key is demoted to retired, and new data encrypted
// under the new active key must round-trip too.
func TestCipher_Decrypt_AfterKeyRotation_OldCiphertextStillDecrypts(t *testing.T) {
	oldKey := testKey("rotation-old-active")
	newKey := testKey("rotation-new-active")

	before, err := NewCipher(oldKey)
	if err != nil {
		t.Fatalf("NewCipher(before) error = %v", err)
	}

	plaintext := []byte("data-written-before-rotation")
	oldCiphertext, err := before.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt(before) error = %v", err)
	}

	after, err := NewCipher(newKey, oldKey)
	if err != nil {
		t.Fatalf("NewCipher(after) error = %v", err)
	}

	gotOld, err := after.Decrypt(oldCiphertext)
	if err != nil {
		t.Fatalf("Decrypt(after, oldCiphertext) error = %v, want the retired key to still open it", err)
	}
	if !bytes.Equal(gotOld, plaintext) {
		t.Fatalf("Decrypt(after, oldCiphertext) = %q, want %q", gotOld, plaintext)
	}

	newCiphertext, err := after.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt(after) error = %v", err)
	}
	gotNew, err := after.Decrypt(newCiphertext)
	if err != nil {
		t.Fatalf("Decrypt(after, newCiphertext) error = %v", err)
	}
	if !bytes.Equal(gotNew, plaintext) {
		t.Fatalf("Decrypt(after, newCiphertext) = %q, want %q", gotNew, plaintext)
	}
}

// encryptedFieldModel is a throwaway GORM model used only by
// TestRegisterEncryptedSerializer_GORMIntegration, to exercise
// RegisterEncryptedSerializer end-to-end against a real SQLite database. Its
// schema is created inline by the test (encryptedFieldModelDDL) rather than
// through a migrations/ file, since the model has no meaning outside this
// one test. The literal serializer name in the tag must match the name
// passed to RegisterEncryptedSerializer in the test below.
type encryptedFieldModel struct {
	ID    uint   `gorm:"primaryKey"`
	Phone string `gorm:"column:phone;serializer:dbkit_test_phone_encrypted"`
}

// TableName pins the table name so it does not depend on GORM's pluralization
// of an unexported type name.
func (encryptedFieldModel) TableName() string { return "dbkit_test_encrypted_field_models" }

const encryptedFieldModelDDL = `
CREATE TABLE dbkit_test_encrypted_field_models (
	id    INTEGER PRIMARY KEY AUTOINCREMENT,
	phone BLOB NOT NULL
);`

// encryptionTestDBSeq gives every newEncryptionTestDB call its own in-memory
// SQLite database name, so repeated or parallel test runs never collide.
var encryptionTestDBSeq atomic.Uint64

// newEncryptionTestDB opens a private in-memory SQLite database for one
// test and applies ddl to it. It is pinned to a single connection so
// SQLite's shared in-memory cache is never torn down and reattached to an
// empty database between queries; see the equivalent, more detailed comment
// on testutil.NewTestSQLite, which this mirrors for a custom schema.
func newEncryptionTestDB(t *testing.T, ddl string) *gorm.DB {
	t.Helper()

	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", name, encryptionTestDBSeq.Add(1))

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.Exec(ddl).Error; err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	return db
}

// TestRegisterEncryptedSerializer_GORMIntegration writes and reads an
// encrypted field through a real GORM model backed by a real SQLite
// database, and confirms two things: the value round-trips correctly
// through GORM, and the bytes actually stored in the database are not the
// plaintext — checked by reading the raw column value directly, bypassing
// GORM's own Scan/serializer machinery entirely.
func TestRegisterEncryptedSerializer_GORMIntegration(t *testing.T) {
	cipher, err := NewCipher(testKey("gorm-integration-active"))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	RegisterEncryptedSerializer("dbkit_test_phone_encrypted", cipher)

	db := newEncryptionTestDB(t, encryptedFieldModelDDL)

	const plaintext = "+15550100777"
	record := encryptedFieldModel{Phone: plaintext}
	if err := db.Create(&record).Error; err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if record.ID == 0 {
		t.Fatalf("Create() left ID unset")
	}

	// Round trip through GORM: the serializer must transparently decrypt.
	var got encryptedFieldModel
	if err := db.First(&got, record.ID).Error; err != nil {
		t.Fatalf("First() error = %v", err)
	}
	if got.Phone != plaintext {
		t.Fatalf("Phone after round trip through GORM = %q, want %q", got.Phone, plaintext)
	}

	// Bypass GORM's scan entirely and read the raw column bytes directly, to
	// prove what is actually sitting in the database is not the plaintext.
	var raw []byte
	row := db.Raw("SELECT phone FROM dbkit_test_encrypted_field_models WHERE id = ?", record.ID).Row()
	if err := row.Scan(&raw); err != nil {
		t.Fatalf("scan raw phone column: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("raw stored phone column is empty")
	}
	if bytes.Contains(raw, []byte(plaintext)) {
		t.Fatalf("raw stored phone column contains the plaintext %q (raw = %x); the field was not actually encrypted", plaintext, raw)
	}
}
