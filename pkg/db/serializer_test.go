package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
)

// encryptedRootKey is the root key the cases below configure. It is a value
// rather than a secret: what they observe is behaviour, and a secret in the
// file would say nothing more about it.
const encryptedRootKey = "the-root-key-of-the-cases"

// encryptedRootKeyItem is that key as the configuration declares it.
const encryptedRootKeyItem = `"encryption-key":"` + encryptedRootKey + `"`

// encryptedModel is a model whose address is encrypted and indexed by the
// digest beside it.
type encryptedModel struct {
	ID    uint
	Email string `gorm:"serializer:encrypted"`
	// EmailIndex is the blind index column. Nothing here fills it: on both
	// the write and the query side it is the caller's, and a row written
	// without it is still written correctly.
	EmailIndex string
	// Nickname is a pointer, which is how a model says a column may be
	// absent. A serializer that reads and writes one has to keep NULL
	// meaning absent rather than sealing an empty value into it.
	Nickname *string `gorm:"serializer:encrypted"`
}

// intEncryptedModel declares the serializer on a type it does not carry.
type intEncryptedModel struct {
	ID  uint
	Age int `gorm:"serializer:encrypted"`
}

// sectionBody is the primary configuration source one case runs on: a SQLite
// database at dsn, plus whatever items the case configures beside it.
func sectionBody(dsn string, items ...string) string {
	body := fmt.Sprintf(`{"db":{"sqlite":{"dsn":%q`, dsn)
	for _, item := range items {
		body += "," + item
	}
	return body + "}}}"
}

// encryptedDSN is a locator in a directory of this case's own. Two assemblies
// that have to see the same rows take the locator once and share it.
func encryptedDSN(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "encrypted.db")
}

// assembleEncrypted runs the module's New over a SQLite database at dsn, with
// the given items configured, and returns the product it built. Without
// encryptedRootKeyItem among them, the assembly has no key material.
//
// These cases go through the assembly rather than calling the serializer or the
// derivation directly: what has to hold is the assembly's behaviour — a
// serializer registered and a key installed before the product exists — and a
// case that wired the two together itself would agree with an implementation
// that never wires them at all.
func assembleEncrypted(t *testing.T, dsn string, items ...string) *database {
	t.Helper()
	spec := sqliteSpec()
	product, err := spec.newDatabase(t.Context(), readerFor(t, spec, sectionBody(dsn, items...)), nil)
	if err != nil {
		t.Fatalf("assembling over %s: %v", sectionBody(dsn, items...), err)
	}
	t.Cleanup(func() {
		if err := Close(product.DB()); err != nil {
			t.Errorf("releasing the assembled pool: %v", err)
		}
	})
	return product
}

// createEncryptedTable brings the model's table up through the handle the
// product carries, which is the handle a dependant would use.
func createEncryptedTable(t *testing.T, product *database) {
	t.Helper()
	if err := product.DB().AutoMigrate(&encryptedModel{}); err != nil {
		t.Fatalf("creating the model's table: %v", err)
	}
}

// storedColumn reads one column of one row with native SQL, around GORM and
// around the serializer: it is the only way to see what the database holds
// rather than what a model makes of it.
func storedColumn(t *testing.T, product *database, id uint, column string) sql.NullString {
	t.Helper()
	query := "SELECT " + column + " FROM encrypted_models WHERE id = ?"
	var stored sql.NullString
	if err := product.DB().Raw(query, id).Row().Scan(&stored); err != nil {
		t.Fatalf("reading %s with native SQL: %v", column, err)
	}
	return stored
}

// TestEncryptedFieldRoundTrips pins the one thing a caller writes such a model
// expecting: what it stored is what it reads back.
func TestEncryptedFieldRoundTrips(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	createEncryptedTable(t, product)

	written := encryptedModel{Email: "user@example.com"}
	if err := product.DB().Create(&written).Error; err != nil {
		t.Fatalf("writing an encrypted field: %v", err)
	}
	if written.ID == 0 {
		t.Fatal("the row came back without the id the read below needs")
	}

	var read encryptedModel
	if err := product.DB().First(&read, written.ID).Error; err != nil {
		t.Fatalf("reading the row back: %v", err)
	}
	if read.Email != "user@example.com" {
		t.Errorf("the field read back as %q, want the plaintext that was written", read.Email)
	}
}

// TestCiphertextDiffersForSamePlaintext pins that the sealed column carries the
// nonce, and with it what the blind index exists for: one plaintext has no one
// ciphertext, so an equality query against the encrypted column matches
// nothing, however it is written.
func TestCiphertextDiffersForSamePlaintext(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	createEncryptedTable(t, product)

	first := encryptedModel{Email: "user@example.com"}
	second := encryptedModel{Email: "user@example.com"}
	for _, row := range []*encryptedModel{&first, &second} {
		if err := product.DB().Create(row).Error; err != nil {
			t.Fatalf("writing an encrypted field: %v", err)
		}
	}

	one, two := storedColumn(t, product, first.ID, "email"), storedColumn(t, product, second.ID, "email")
	if !one.Valid || !two.Valid {
		t.Fatalf("the column is NULL after a write: %v and %v", one, two)
	}
	if one.String == two.String {
		t.Error("one plaintext landed as one column twice, so a ciphertext identifies its plaintext and " +
			"an equality query works on the very column the blind index exists beside")
	}
	for _, id := range []uint{first.ID, second.ID} {
		var read encryptedModel
		if err := product.DB().First(&read, id).Error; err != nil {
			t.Fatalf("reading row %d back: %v", id, err)
		}
		if read.Email != "user@example.com" {
			t.Errorf("row %d read back as %q, want the plaintext that was written", id, read.Email)
		}
	}
}

// TestEncryptedColumnIsNotStoredInClear pins the promise the whole mechanism
// exists for, on the bytes the database actually holds.
//
// The comparison against the plaintext alone would be satisfied by a column
// that carried the plaintext inside a wrapper, so the text is searched for the
// plaintext as well: the column holds a nonce and a sealed value, and neither
// of them contains the address.
func TestEncryptedColumnIsNotStoredInClear(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	createEncryptedTable(t, product)

	written := encryptedModel{Email: "user@example.com"}
	if err := product.DB().Create(&written).Error; err != nil {
		t.Fatalf("writing an encrypted field: %v", err)
	}
	stored := storedColumn(t, product, written.ID, "email")
	if !stored.Valid {
		t.Fatal("the column is NULL after a write, so nothing was stored at all")
	}
	if stored.String == written.Email {
		t.Error("the column holds the plaintext of the field")
	}
	if strings.Contains(stored.String, written.Email) {
		t.Errorf("the column holds the plaintext inside something else: %q", stored.String)
	}
}

// TestRetiredKeyStillDecrypts pins the rotation the design promises: a root key
// that is no longer current still reads what it wrote.
//
// The second half is what makes the first one mean something: the same rows,
// read by an assembly that does not list the old key, do not come back — so the
// case is about the retired key being consulted rather than about the read
// path never failing.
func TestRetiredKeyStillDecrypts(t *testing.T) {
	dsn := encryptedDSN(t)
	before := assembleEncrypted(t, dsn, encryptedRootKeyItem)
	createEncryptedTable(t, before)
	written := encryptedModel{Email: "user@example.com"}
	if err := before.DB().Create(&written).Error; err != nil {
		t.Fatalf("writing under the first root key: %v", err)
	}

	rotated := assembleEncrypted(t, dsn,
		`"encryption-key":"a-newer-root-key"`,
		`"encryption-retired-keys":["`+encryptedRootKey+`"]`)
	var read encryptedModel
	if err := rotated.DB().First(&read, written.ID).Error; err != nil {
		t.Fatalf("reading a row the retired key wrote on a rotated assembly: %v", err)
	}
	if read.Email != "user@example.com" {
		t.Errorf("the rotated assembly read %q, want the plaintext the retired key wrote", read.Email)
	}

	dropped := assembleEncrypted(t, dsn, `"encryption-key":"a-newer-root-key"`)
	var unreadable encryptedModel
	err := dropped.DB().First(&unreadable, written.ID).Error
	if err == nil {
		t.Fatal("a row written under a root key that is neither current nor retired was read back")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("the row was reported as absent rather than unreadable, which sends a reader to the "+
			"wrong thing: %v", err)
	}
	if !strings.Contains(err.Error(), "Email") {
		t.Errorf("the failure does not name the field it could not open: %v", err)
	}
}

// TestBlindIndexDoesNotNormalize pins that the digest is computed over the
// string as it was given.
//
// Flattening is the caller's, on both sides. A digest that folded case or
// trimmed whitespace would match one spelling and not the other depending on
// which side the caller happened to flatten, and the difference shows up as an
// empty result rather than as an error — which is exactly why neither default
// is safe here and neither is taken.
func TestBlindIndexDoesNotNormalize(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)

	digest := product.BlindIndex("A@B.com")
	if product.BlindIndex("A@B.com") != digest {
		t.Error("the digest of one input changed between two calls, so a column written today would not " +
			"be found tomorrow")
	}
	if product.BlindIndex("a@b.com") == digest {
		t.Error("the digest folds case, so a caller that flattened on one side only would find or miss " +
			"a row without being told either way")
	}
	if product.BlindIndex(" A@B.com") == digest {
		t.Error("the digest strips surrounding whitespace, with the same effect as folding case")
	}
}

// TestBlindIndexColumnIsNotFilledAutomatically pins the documented shape of a
// model that forgets its digest column: the row is written, the write reports
// nothing, and only the equality query comes back empty.
//
// The control is the query itself. Without a row found through a digest the
// caller did write, a broken query would report an empty result too, and the
// case would agree with an implementation that filled the column in.
func TestBlindIndexColumnIsNotFilledAutomatically(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	createEncryptedTable(t, product)

	written := encryptedModel{Email: "user@example.com"}
	if err := product.DB().Create(&written).Error; err != nil {
		t.Fatalf("writing a row without its digest column: %v", err)
	}

	var found []encryptedModel
	if err := product.DB().Where("email_index = ?", product.BlindIndex(written.Email)).
		Find(&found).Error; err != nil {
		t.Fatalf("querying by the blind index reported %v, want an empty result", err)
	}
	if len(found) != 0 {
		t.Errorf("%d rows were found by a digest column nobody filled, so this module fills it", len(found))
	}

	indexed := encryptedModel{Email: written.Email, EmailIndex: product.BlindIndex(written.Email)}
	if err := product.DB().Create(&indexed).Error; err != nil {
		t.Fatalf("writing a row whose digest column is filled: %v", err)
	}
	var read encryptedModel
	if err := product.DB().Where("email_index = ?", indexed.EmailIndex).First(&read).Error; err != nil {
		t.Fatalf("querying by a digest the caller wrote: %v", err)
	}
	if read.Email != written.Email {
		t.Errorf("the row found by its digest read back as %q, want the plaintext", read.Email)
	}
}

// startedRun drives one real lifecycle over the modules given and returns the
// error it ended with, which is nil when the startup itself succeeded: a
// registry whose modules all came up then runs until it is stopped.
//
// It stops the run once the probe has reached its Init, which is after the
// database module applied its migrations — stopping it at delivery instead
// would cancel the context under a migration still in flight and report the
// interruption as a failed startup. A run that fails returns on its own, before
// the probe is reached at all.
//
// The shape follows the host fixture the external test package keeps, which a
// case in this package cannot use: what is observed here — the registration of
// the serializer and the key on the handle — is not exported.
func startedRun(t *testing.T, body string, modules ...core.Module) error {
	t.Helper()
	locator := writeConfig(t, body)

	reg := core.New()
	reg.Register(config.Module())
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: "SPEEDDBTEST", DefaultLocator: "file://" + locator}},
	})
	for _, module := range modules {
		reg.Register(module)
	}
	started := make(chan struct{})
	reg.Register(core.Module{
		Name:     "probe",
		Requires: []core.Requirement{{Token: (*Database)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			_, err := core.Resolve[Database](reg)
			return nil, err
		},
		Init: func(context.Context, *core.Registry, any) error {
			close(started)
			return nil
		},
	})

	// The loader reads os.Args unconditionally and a test binary carries
	// arguments of its own, so they are taken away for the call.
	previous := os.Args
	os.Args = []string{"db.test"}
	defer func() { os.Args = previous }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-started:
		cancel()
		return <-done
	}
}

// TestMissingEncryptionKeyStartsUpAndFailsAtUse pins the stance an assembly
// with no root key takes, in both directions.
//
// It starts up: whether any model carries an encrypted field is not known until
// one is used, so there is nothing to judge at startup. The first write that
// reaches such a field then fails, naming the item that would turn encryption
// on, and it fails instead of sealing under a subkey nobody chose — which would
// write a column nothing can open, with no error anywhere near it.
func TestMissingEncryptionKeyStartsUpAndFailsAtUse(t *testing.T) {
	dsn := encryptedDSN(t)
	// The whole lifecycle, not only the construction, so that "starts up" is
	// the run that reached delivery.
	if err := startedRun(t, sectionBody(dsn), NewModule(sqliteSpec())); err != nil {
		t.Fatalf("a startup with no encryption key failed: %v", err)
	}

	product := assembleEncrypted(t, dsn)
	createEncryptedTable(t, product)
	err := product.DB().Create(&encryptedModel{Email: "user@example.com"}).Error
	if err == nil {
		t.Fatal("writing an encrypted field on an assembly with no root key succeeded")
	}
	if !strings.Contains(err.Error(), encryptionKeyKey) {
		t.Errorf("the failure does not name %s, which is the item that would turn encryption on: %v",
			encryptionKeyKey, err)
	}
}

// TestKeyInjectionIsRegisteredOnTheDeliveredHandle pins the assembly's timing
// from the handle itself.
//
// The key reaches a statement through a callback of the handle the statement
// runs on, so a handle assembled without it reaches every encrypted field with
// no key at all — and the failure of the second half is silent in the way that
// matters: the statement runs, the row is written, and only a later read
// disagrees. A connection built with Open has to carry it too; the alternative
// is that moving a query to another database loses the encryption with nothing
// said.
func TestKeyInjectionIsRegisteredOnTheDeliveredHandle(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)

	cb := product.DB().Callback()
	chains := []struct {
		name string
		get  func(string) func(*gorm.DB)
	}{
		{"create", cb.Create().Get},
		{"update", cb.Update().Get},
		{"delete", cb.Delete().Get},
		{"query", cb.Query().Get},
		{"row", cb.Row().Get},
		{"raw", cb.Raw().Get},
	}
	for _, chain := range chains {
		if chain.get(keyInjectionCallbackName) == nil {
			t.Errorf("the %s chain of the delivered handle carries no key injection", chain.name)
		}
	}

	opened, err := product.Open(t.Context(), encryptedDSN(t))
	if err != nil {
		t.Fatalf("building a second connection: %v", err)
	}
	t.Cleanup(func() {
		if err := Close(opened); err != nil {
			t.Errorf("releasing the second connection: %v", err)
		}
	})
	if opened.Callback().Create().Get(keyInjectionCallbackName) == nil {
		t.Error("a connection built with Open carries no key injection, so the encryption a caller has on " +
			"the delivered handle disappears on the second database")
	}
}

// TestASelfBuiltConnectionCarriesTheSameEncryption pins the same assembly for
// the second half of the connection surface, on the behaviour rather than on
// the callback.
//
// A handle opened for another database encrypts and decrypts with the key the
// configuration gave — not with one of its own, and not with none. The second
// assembly over the same database is what separates those: a per-handle key
// would round-trip through its own handle and fail to read what was written
// here.
func TestASelfBuiltConnectionCarriesTheSameEncryption(t *testing.T) {
	dsn := encryptedDSN(t)
	product := assembleEncrypted(t, dsn, encryptedRootKeyItem)
	opened, err := product.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("building a second connection: %v", err)
	}
	t.Cleanup(func() {
		if err := Close(opened); err != nil {
			t.Errorf("releasing the second connection: %v", err)
		}
	})
	if err := opened.AutoMigrate(&encryptedModel{}); err != nil {
		t.Fatalf("creating the model's table through the second connection: %v", err)
	}

	written := encryptedModel{Email: "user@example.com"}
	if err := opened.Create(&written).Error; err != nil {
		t.Fatalf("writing an encrypted field through a connection built with Open: %v", err)
	}
	var read encryptedModel
	if err := opened.First(&read, written.ID).Error; err != nil {
		t.Fatalf("reading it back through the same connection: %v", err)
	}
	if read.Email != written.Email {
		t.Errorf("the second connection read %q, want the plaintext", read.Email)
	}

	other := assembleEncrypted(t, dsn, encryptedRootKeyItem)
	var byOther encryptedModel
	if err := other.DB().First(&byOther, written.ID).Error; err != nil {
		t.Fatalf("a second assembly over the same configuration does not open what the second connection "+
			"wrote, so the key is not the one the configuration gives: %v", err)
	}
	if byOther.Email != written.Email {
		t.Errorf("the second assembly read %q, want the plaintext", byOther.Email)
	}
}

// TestTheSerializerIsRegisteredByTheAssembly pins that the registration happens
// inside the assembly, which is what puts it before the handle is handed out.
//
// The registration is process-wide and written by every assembly, so in a
// binary where an earlier case assembled anything, a round trip through a
// handle says nothing about what this assembly did — that observation is the
// same for an implementation that registers late. Clearing the table first is
// what gives this case something to see: the control below shows the cleared
// state is one in which a model naming the serializer cannot be parsed, and the
// assertion after it shows one assembly puts the registration in place.
func TestTheSerializerIsRegisteredByTheAssembly(t *testing.T) {
	schema.RegisterSerializer(encryptedSerializerName, nil)
	t.Cleanup(func() {
		schema.RegisterSerializer(encryptedSerializerName, encryptedSerializer{})
	})
	if _, ok := schema.GetSerializer(encryptedSerializerName); ok {
		t.Fatal("the serializer is still registered after having been cleared, so this case cannot tell " +
			"an assembly that registers it from one that does not")
	}

	_, err := schema.Parse(&encryptedModel{}, &sync.Map{}, schema.NamingStrategy{})
	if err == nil {
		t.Fatal("a model naming an unregistered serializer parsed, so the assertion below would agree " +
			"with an implementation that never registers one")
	}
	if !strings.Contains(err.Error(), encryptedSerializerName) {
		t.Fatalf("parsing a model whose serializer is not registered gave %v, want GORM's "+
			"invalid-serializer failure", err)
	}

	assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	if _, err := schema.Parse(&encryptedModel{}, &sync.Map{}, schema.NamingStrategy{}); err != nil {
		t.Errorf("a model naming the encrypted serializer still does not parse after an assembly: %v", err)
	}
}

// contextMarker is a value a case puts on a statement's context to see whether
// the context the caller gave survived the key injection.
type contextMarker struct{}

// TestTheKeyInjectionKeepsTheContextTheCallerGave pins what the injection does
// to the context it finds.
//
// db.WithContext(requestCtx) is the ordinary call, and it replaces the
// statement's context. Everything the request put there — its deadline, its
// identifiers, its cancellation — has to reach the statement, with the key
// added to it rather than put in its place. An injection that built a fresh
// context would leave the write succeeding and the request's deadline behind.
func TestTheKeyInjectionKeepsTheContextTheCallerGave(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	createEncryptedTable(t, product)

	var carried any
	if err := product.DB().Callback().Create().Before("gorm:create").
		Register("test:observe-context", func(tx *gorm.DB) {
			carried = tx.Statement.Context.Value(contextMarker{})
		}); err != nil {
		t.Fatalf("registering the observing callback: %v", err)
	}

	ctx := context.WithValue(t.Context(), contextMarker{}, "the request's own value")
	written := encryptedModel{Email: "user@example.com"}
	if err := product.DB().WithContext(ctx).Create(&written).Error; err != nil {
		t.Fatalf("writing under a context the caller gave: %v", err)
	}
	if carried != "the request's own value" {
		t.Errorf("the statement carried %v where the caller's value should be, so the injection replaced "+
			"the context instead of adding to it", carried)
	}
	var read encryptedModel
	if err := product.DB().First(&read, written.ID).Error; err != nil {
		t.Fatalf("reading the row back: %v", err)
	}
	if read.Email != written.Email {
		t.Errorf("the field read back as %q, want the plaintext", read.Email)
	}
}

// TestAPointerFieldRoundTripsAndStaysNil pins the two states a nullable
// encrypted column has, and that the absent one stays absent.
//
// A serializer that sealed the absence would read it back as an allocated empty
// value, and the model would report a value where the caller stored none. The
// NULL in the column is the check on that: it is about the bytes the database
// holds rather than about what a read makes of them.
func TestAPointerFieldRoundTripsAndStaysNil(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	createEncryptedTable(t, product)

	nickname := "bob"
	present := encryptedModel{Email: "first@example.com", Nickname: &nickname}
	absent := encryptedModel{Email: "second@example.com"}
	for _, row := range []*encryptedModel{&present, &absent} {
		if err := product.DB().Create(row).Error; err != nil {
			t.Fatalf("writing a pointer field: %v", err)
		}
	}

	var readPresent, readAbsent encryptedModel
	if err := product.DB().First(&readPresent, present.ID).Error; err != nil {
		t.Fatalf("reading the row that set the field: %v", err)
	}
	if err := product.DB().First(&readAbsent, absent.ID).Error; err != nil {
		t.Fatalf("reading the row that left it unset: %v", err)
	}
	if readPresent.Nickname == nil || *readPresent.Nickname != nickname {
		t.Errorf("the field read back as %v, want %q", readPresent.Nickname, nickname)
	}
	if readAbsent.Nickname != nil {
		t.Errorf("a field that was never set came back as %q, so the absence was sealed into a value",
			*readAbsent.Nickname)
	}
	if stored := storedColumn(t, product, absent.ID, "nickname"); stored.Valid {
		t.Errorf("the column holds %q where the field was never set, want NULL", stored.String)
	}
}

// TestAFieldTypeTheSerializerCannotEncryptIsRefused pins that the refusal
// reaches the caller that wrote the model.
//
// The alternative to naming the field is a value that goes in as something else
// — a byte slice of a formatting, an empty string — and the model that declared
// an int reads back whatever that was, or fails on a type it cannot assign. The
// failure is put where the declaration is wrong, and it names it.
func TestAFieldTypeTheSerializerCannotEncryptIsRefused(t *testing.T) {
	product := assembleEncrypted(t, encryptedDSN(t), encryptedRootKeyItem)
	if err := product.DB().AutoMigrate(&intEncryptedModel{}); err != nil {
		t.Fatalf("creating the model's table: %v", err)
	}

	err := product.DB().Create(&intEncryptedModel{Age: 42}).Error
	if err == nil {
		t.Fatal("a field type this serializer cannot encrypt was written without a word")
	}
	if !strings.Contains(err.Error(), "Age") {
		t.Errorf("the failure does not name the field it refused, so a model with several of them is "+
			"left to guess: %v", err)
	}
}
