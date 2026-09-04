package pki

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// LocalKeySerializerName is the gorm serializer name LocalKey.EncryptedPrivateKey
// is tagged with. Register it once at host bootstrap, before opening the
// *gorm.DB LocalSigner is given -- see RegisterLocalKeySerializer.
const LocalKeySerializerName = "pki_local_key_enc"

// RegisterLocalKeySerializer wires cipher into GORM's serializer registry
// under LocalKeySerializerName, so LocalKey.EncryptedPrivateKey is
// transparently sealed on write and opened on read.
//
// Call it during host bootstrap, BEFORE opening the *gorm.DB LocalSigner is
// constructed over. GORM's serializer registry is process-global and
// consulted while a model's schema is parsed, so registering afterward
// leaves the parsed schema pointing at nothing -- the same rule
// authn.RegisterPIISerializer and org.WithEmailIndexer's cipher document.
// This is deliberately NOT done inside Module.Register: by then the
// database is already open, and Register is documented to only declare.
func RegisterLocalKeySerializer(cipher *dbkit.Cipher) error {
	if cipher == nil {
		return fmt.Errorf("pki: RegisterLocalKeySerializer requires a cipher for %q", LocalKeySerializerName)
	}
	dbkit.RegisterEncryptedSerializer(LocalKeySerializerName, cipher)
	return nil
}

// LocalKeyRepository is the plain, non-tenant-scoped accessor for
// pki_local_keys. LocalKey is platform data (see its own doc comment), so
// this wraps a bare *gorm.DB rather than embedding dbkit.Repository[T],
// exactly like SigningKeyRepository and AuthorityRepository.
type LocalKeyRepository struct {
	db *gorm.DB
}

// NewLocalKeyRepository returns a LocalKeyRepository over db. db is expected
// to come from dbkit.Open with this module's migrations already applied.
func NewLocalKeyRepository(db *gorm.DB) *LocalKeyRepository {
	return &LocalKeyRepository{db: db}
}

// Create inserts key.
func (r *LocalKeyRepository) Create(ctx context.Context, key *LocalKey) error {
	return r.db.WithContext(ctx).Create(key).Error
}

// FindByKeyRef returns the row for keyRef, or (nil, ErrKeyNotFound).
func (r *LocalKeyRepository) FindByKeyRef(ctx context.Context, keyRef string) (*LocalKey, error) {
	var key LocalKey
	err := r.db.WithContext(ctx).Where("key_ref = ?", keyRef).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// Delete removes the row for keyRef. Deleting an already-absent keyRef
// reports ErrKeyNotFound, matching Signer.Destroy's documented contract.
func (r *LocalKeyRepository) Delete(ctx context.Context, keyRef string) error {
	res := r.db.WithContext(ctx).Where("key_ref = ?", keyRef).Delete(&LocalKey{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// LocalSigner is the Signer implementation with zero external dependencies
// beyond the standard library's crypto/ed25519 plus dbkit and pkgcore --
// what "task dev" runs and what every unit test in this module and its
// consumers can use as a test double, per docs/internal/22-pki.md's "three
// implementations" table.
//
// It does NOT have the KeyNeverLeavesBoundary capability the vault and
// kmsaws implementations declare in direct-sign mode (round 4): a Sign call
// decrypts the private key into this process's memory for the duration of
// the crypto/ed25519 call. That is the deliberate, documented cost of a
// zero-dependency signer, not an oversight -- see AGENTS.md's Known
// limitations for the pkgcore.Capability wiring this implies and why it is
// not shipped yet.
type LocalSigner struct {
	keys *LocalKeyRepository
}

// NewLocalSigner returns a LocalSigner storing keys through db. db is
// expected to come from dbkit.Open with this module's migrations already
// applied, and LocalKeySerializerName already registered
// (RegisterLocalKeySerializer) against the cipher the host injected.
func NewLocalSigner(db *gorm.DB) *LocalSigner {
	return &LocalSigner{keys: NewLocalKeyRepository(db)}
}

// GenerateKey implements Signer. Only AlgorithmEd25519 is supported; any
// other value reports ErrAlgorithmUnsupportedBySigner.
func (s *LocalSigner) GenerateKey(ctx context.Context, algorithm string) (string, crypto.PublicKey, error) {
	if algorithm != AlgorithmEd25519 {
		return "", nil, ErrAlgorithmUnsupportedBySigner.WithParam("algorithm", algorithm)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("pki: generate ed25519 key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", nil, fmt.Errorf("pki: marshal private key: %w", err)
	}

	keyRef := uuid.NewString()
	if err := s.keys.Create(ctx, &LocalKey{
		KeyRef:              keyRef,
		Algorithm:           algorithm,
		EncryptedPrivateKey: string(pkcs8),
	}); err != nil {
		return "", nil, fmt.Errorf("pki: store local key: %w", err)
	}

	return keyRef, pub, nil
}

// Sign implements Signer. It decrypts the private key for keyRef, signs
// input, and drops the decrypted key material; see input's algorithm-
// dependent meaning on the Signer interface's own doc comment.
func (s *LocalSigner) Sign(ctx context.Context, keyRef string, input []byte) ([]byte, error) {
	priv, err := s.loadPrivateKey(ctx, keyRef)
	if err != nil {
		return nil, err
	}
	switch key := priv.(type) {
	case ed25519.PrivateKey:
		return ed25519.Sign(key, input), nil
	default:
		return nil, fmt.Errorf("pki: local signer: keyRef %q holds an unsupported private key type %T", keyRef, priv)
	}
}

// Public implements Signer.
func (s *LocalSigner) Public(ctx context.Context, keyRef string) (crypto.PublicKey, error) {
	priv, err := s.loadPrivateKey(ctx, keyRef)
	if err != nil {
		return nil, err
	}
	switch key := priv.(type) {
	case ed25519.PrivateKey:
		return key.Public(), nil
	default:
		return nil, fmt.Errorf("pki: local signer: keyRef %q holds an unsupported private key type %T", keyRef, priv)
	}
}

// Destroy implements Signer: physically deletes the pki_local_keys row for
// keyRef.
func (s *LocalSigner) Destroy(ctx context.Context, keyRef string) error {
	return s.keys.Delete(ctx, keyRef)
}

// loadPrivateKey reads and decrypts the private key for keyRef. The
// decrypted key exists only in this call's stack, for as long as the
// caller's switch statement holds it.
func (s *LocalSigner) loadPrivateKey(ctx context.Context, keyRef string) (crypto.PrivateKey, error) {
	row, err := s.keys.FindByKeyRef(ctx, keyRef)
	if err != nil {
		return nil, err
	}
	priv, err := x509.ParsePKCS8PrivateKey([]byte(row.EncryptedPrivateKey))
	if err != nil {
		return nil, fmt.Errorf("pki: local signer: parse private key for keyRef %q: %w", keyRef, err)
	}
	return priv, nil
}

// compile-time check that *LocalSigner satisfies Signer.
var _ Signer = (*LocalSigner)(nil)
