package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// encryptedSerializerName is what a model names to have one of its fields
// encrypted on the way in and decrypted on the way out:
// `gorm:"serializer:encrypted"`.
const encryptedSerializerName = "encrypted"

// keyInjectionCallbackName is the name the key injection is registered under,
// once per chain, on every handle this package opens. A statement's context
// carries no such key unless the injection put it there, so this name is what
// tells the statements this package assembled apart from the ones it did not.
const keyInjectionCallbackName = "db:encryption-keys"

// encryptionKeysContextKey is the key the derived material travels under on a
// statement's context. The type is unexported and empty, so no other package
// can address the value, and no other package's key can collide with it.
type encryptionKeysContextKey struct{}

// contextWithKeys returns a context that carries the material, keeping
// whatever the context already held. The statement's context is the caller's —
// a request's deadline and values travel on it — and the key is one more value
// on it rather than a replacement for it.
func contextWithKeys(ctx context.Context, keys *keyRing) context.Context {
	return context.WithValue(ctx, encryptionKeysContextKey{}, keys)
}

// keysInContext takes the material back out, and reports whether the context
// was one this package put it in. A context carrying a nil ring reports true:
// the value is there and says the assembly configured no root key, which is a
// different failure from a statement that never met this package's injection.
func keysInContext(ctx context.Context) (*keyRing, bool) {
	keys, ok := ctx.Value(encryptionKeysContextKey{}).(*keyRing)
	return keys, ok
}

// installEncryption registers the serializer and puts this section's key
// material on one handle, so that every statement running on it — or on
// anything derived from it by Session, WithContext or a transaction — finds
// the key in its context.
//
// It is called on every handle this package opens, the delivered one and each
// one a caller builds with Open: a second connection carries the same
// encryption the first does, and a caller that moved a query to another
// database does not silently lose it.
//
// Two properties of the shape are GORM's rather than this package's. The
// serializer is registered process-wide, because schema.RegisterSerializer
// writes a package-level table and a serializer's methods receive a field and
// a context but never the *gorm.DB — there is no per-instance registration in
// GORM to use instead. What is per-instance is the key, so the registered
// value holds no state at all and the key travels on each statement's context;
// two databases in one process, assembled from two configurations, therefore
// encrypt under their own keys and neither can reach the other's. And the key
// is put on the context at every statement rather than once on the handle,
// because the ordinary db.WithContext(requestCtx) call replaces the statement's
// context: a key left on the handle would be absent from exactly the statements
// that carry a request. The injection is registered ahead of the callback that
// builds a statement's SQL, so the material is in place before any field of it
// is serialised.
//
// An assembly with no root key installs a nil ring all the same. The serializer
// then reports which key is missing instead of reporting that the handle does
// not belong to this package, and the two are different answers for an
// operator.
func installEncryption(session *gorm.DB, namespace string, cfg Config) error {
	keys, err := newKeyRing(namespace, cfg)
	if err != nil {
		return err
	}
	// Registering the same value again is what keeps the registration inside
	// the assembly: the table is process-wide, so a serializer registered
	// once at package load would already be there, and the statement that
	// first needs it would find it whether or not this handle was assembled
	// at all.
	schema.RegisterSerializer(encryptedSerializerName, encryptedSerializer{})

	inject := func(tx *gorm.DB) {
		tx.Statement.Context = contextWithKeys(tx.Statement.Context, keys)
	}
	cb := session.Callback()
	// Every chain a statement can run on. A serialised field is written from
	// the create and update paths and read back from the query, row and raw
	// ones; the delete chain carries no serialised field today and is
	// registered anyway, so that the rule is "every chain" rather than a list
	// that has to stay right, and the cost of it is a context value on
	// statements that will not look at one.
	return errors.Join(
		cb.Create().Before("gorm:create").Register(keyInjectionCallbackName, inject),
		cb.Update().Before("gorm:update").Register(keyInjectionCallbackName, inject),
		cb.Delete().Before("gorm:delete").Register(keyInjectionCallbackName, inject),
		cb.Query().Before("gorm:query").Register(keyInjectionCallbackName, inject),
		cb.Row().Before("gorm:row").Register(keyInjectionCallbackName, inject),
		cb.Raw().Before("gorm:raw").Register(keyInjectionCallbackName, inject),
	)
}

// encryptedSerializer is what the name resolves to, in GORM's process-wide
// table of serializers.
//
// It holds nothing on purpose. Anything it held would be shared by every
// database in the process, including one assembled from another configuration
// and one whose key was rotated since, so the key it works under arrives with
// each statement instead — put there by the injection installEncryption
// registers.
type encryptedSerializer struct{}

// The serializer answers the interface GORM resolves a `serializer:` tag
// through, so a drift between the two is caught where the type is written
// rather than when a model that names it is first parsed.
var _ schema.SerializerInterface = encryptedSerializer{}

// Value encrypts one field on its way into the database.
func (encryptedSerializer) Value(ctx context.Context, field *schema.Field, _ reflect.Value, fieldValue any) (any, error) {
	keys, err := keysOfStatement(ctx, field)
	if err != nil {
		return nil, err
	}
	plaintext, ok := plaintextOf(fieldValue)
	if !ok {
		return nil, fmt.Errorf("db: field %s is marked serializer:%s, which encrypts a string or a "+
			"byte slice, and it holds %T", fieldName(field), encryptedSerializerName, fieldValue)
	}
	if plaintext == nil {
		// A nil pointer and a nil slice are the absence of a value, and it
		// stays absent: a column holding a seal over nothing reads back as a
		// value that was never there.
		return nil, nil
	}
	return keys.encrypt(plaintext), nil
}

// Scan decrypts one field on its way out of the database.
func (encryptedSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, dbValue any) error {
	if dbValue == nil {
		// The column is NULL, so there is nothing to open and the field takes
		// its zero value. No key takes part in this, and an assembly without
		// one reads a NULL column as the absence it is.
		field.ReflectValueOf(ctx, dst).Set(reflect.Zero(field.FieldType))
		return nil
	}
	keys, err := keysOfStatement(ctx, field)
	if err != nil {
		return err
	}
	var stored []byte
	switch value := dbValue.(type) {
	case string:
		stored = []byte(value)
	case []byte:
		stored = value
	default:
		return fmt.Errorf("db: field %s is marked serializer:%s and its column came back as %T, "+
			"which is not the text this module writes into one", fieldName(field),
			encryptedSerializerName, dbValue)
	}
	plaintext, err := keys.decrypt(string(stored))
	if err != nil {
		return fmt.Errorf("db: reading encrypted field %s: %w", fieldName(field), err)
	}
	return setPlaintext(ctx, field, dst, plaintext)
}

// keysOfStatement takes the material the handle put on this statement.
//
// Both ways of finding none are reported rather than encrypted around. A
// statement whose context carries no material was not built by a handle this
// package opened — a handle of the caller's own, or one whose statement was
// replaced wholesale — and one whose material is nil ran on an assembly whose
// configuration gave no root key. Sealing under a key nobody chose would write
// a column nothing can ever open, and the data would be wrong with no error
// anywhere near it, so both are refused where the field is reached.
func keysOfStatement(ctx context.Context, field *schema.Field) (*keyRing, error) {
	keys, ok := keysInContext(ctx)
	switch {
	case !ok:
		return nil, fmt.Errorf("db: field %s is encrypted, and the statement it travelled on "+
			"carries no encryption key: it was not built by a handle this module opened",
			fieldName(field))
	case keys == nil:
		return nil, fmt.Errorf("db: field %s is encrypted, and the database this statement ran on "+
			"was assembled without %s: set it to encrypt this field",
			fieldName(field), encryptionKeyKey)
	}
	return keys, nil
}

// plaintextOf is the plaintext a field value carries, and whether the type is
// one this serializer reads and writes. A nil pointer and a nil slice report
// true with no plaintext: the type is one it carries, and the value is absent.
func plaintextOf(fieldValue any) ([]byte, bool) {
	switch value := fieldValue.(type) {
	case string:
		return []byte(value), true
	case []byte:
		return value, true
	case *string:
		if value == nil {
			return nil, true
		}
		return []byte(*value), true
	case *[]byte:
		if value == nil {
			return nil, true
		}
		return *value, true
	default:
		return nil, false
	}
}

// setPlaintext writes a decrypted value back into the field, in the type the
// model declared it as: a model reading a column into a pointer gets a pointer,
// because a value of the pointed-to type would not compile there.
func setPlaintext(ctx context.Context, field *schema.Field, dst reflect.Value, plaintext []byte) error {
	target := field.ReflectValueOf(ctx, dst)
	holder, pointer := target, false
	if target.Kind() == reflect.Pointer {
		// The pointer is allocated to hold the value. reflect.New gives the
		// pointer and its element is the addressable value to write into;
		// the pointer goes into the field once that value is there.
		holder = reflect.New(target.Type().Elem()).Elem()
		pointer = true
	}
	switch {
	case holder.Kind() == reflect.String:
		holder.SetString(string(plaintext))
	case holder.Kind() == reflect.Slice && holder.Type().Elem().Kind() == reflect.Uint8:
		holder.SetBytes(plaintext)
	default:
		return fmt.Errorf("db: field %s is marked serializer:%s and is declared %s; a column it "+
			"encrypts is read back into a string, a byte slice, or a pointer to either",
			fieldName(field), encryptedSerializerName, field.FieldType)
	}
	if pointer {
		target.Set(holder.Addr())
	}
	return nil
}

// fieldName names a field the way the model declares it, so that a failure
// points at the declaration rather than at a column name the caller never
// wrote.
func fieldName(field *schema.Field) string {
	if field.Schema != nil && field.Schema.Name != "" {
		return field.Schema.Name + "." + field.Name
	}
	return field.Name
}
