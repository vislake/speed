package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Object keys are limited to 1024 bytes to stay inside the S3 object-key
// limit every compatible service enforces, and each '/' -separated segment to
// 255 bytes, the filename limit of the local filesystem backend. The grammar
// is the intersection of what every backend can represent, never the union.
const (
	maxObjectKeyLength     = 1024
	maxObjectSegmentLength = 255
)

// ErrObjectNotFound is returned by ObjectStore.GetObject when the key does
// not exist. Every implementation reports the same sentinel: the local
// backend maps the file system's not-found errors onto it, the S3 backend
// maps the S3 NoSuchKey error onto it, so a caller written against the
// interface never sees a backend's own vocabulary. DeleteObject never returns
// it: removing a key that does not exist is a success.
var ErrObjectNotFound = errors.New("pkgcore: object not found")

// ErrInvalidObjectKey is returned by every ObjectStore operation when the key
// fails the rules all implementations enforce, so a key one backend accepts
// is accepted by all of them. The rules are the grammar both backends can
// represent, deliberately narrower than raw S3's: a non-empty key of at most
// 1024 bytes whose '/' -separated segments are each at most 255 bytes, with
// no NUL byte, no backslash, no leading or trailing '/', no empty segment,
// and no segment that is "." or "..". Nothing about a key is echoed in the
// error text: the caller knows the key, and the error names the rule the key
// broke.
var ErrInvalidObjectKey = errors.New("pkgcore: invalid object key")

// ObjectStore is the object-storage contract shared by every deployment
// mode: a directory on the local file system in the standalone deployment
// mode, an S3-compatible service (MinIO, OSS or AWS S3) in the distributed
// deployment mode.
//
// The interface is deliberately designed against the weakest backend it must
// support, the local directory. Every key must be storable as a path below a
// single root, which is why the key grammar above is what it is; objects are
// streams of bytes with no metadata, no attributes and no server-side
// operations, because the local backend has none of those. There is no
// listing, no stat, no copy and no presigned access on the interface: a store
// carries bytes, and callers that need a catalog keep it themselves. Those
// omissions are the seam's boundary: presigned access, object metadata and
// lifecycle handling are capabilities only the S3-backed backend could
// satisfy, so they belong to go/storage -- the module built on top of this
// contract and its first consumer -- never on the interface itself.
//
// The keyspace is a single tree. A key must not be a proper prefix of another
// stored key (one "/"-separated path cannot hold both a file and a
// directory), so at most one of a key and any key extending it may exist at a
// time. The local backend refuses the PutObject that would create the clash,
// because its file system cannot represent it; an S3-compatible backend could
// accept it, but the overlap is outside the contract either way, and the
// standalone implementation's refusal is what makes the discipline visible
// during development and in tests, where it doubles as the test double.
//
// All three operations stream and honour ctx throughout: a PutObject whose
// context is cancelled stops uploading and returns the context's error, a
// GetObject reader whose context is cancelled fails its reads, and an
// operation that begins on an already-cancelled context does nothing but
// return the context's error. Implementations are safe for concurrent use.
type ObjectStore interface {
	// PutObject stores the whole stream read from r under key, replacing the
	// previous object at that key if there was one. The write is atomic:
	// readers never observe a partial object, and a PutObject that fails
	// leaves the previous object, or the absence of one, untouched. r may be
	// of unknown length; implementations stream it with bounded memory.
	PutObject(ctx context.Context, key string, r io.Reader) error

	// GetObject returns a reader streaming the object stored under key. A key
	// that does not exist is reported with ErrObjectNotFound; a key whose
	// name is invalid is reported with ErrInvalidObjectKey. The caller owns
	// the returned reader and must Close it, whether or not it was read to
	// completion, and a caller that abandons an object mid-read without
	// closing it leaks a descriptor or an open request. The reader yields the
	// object's bytes as they were when the request started: an object
	// overwritten while it is being read does not disturb an open read.
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)

	// DeleteObject removes the object stored under key. Deleting a key that
	// does not exist is a success: DeleteObject is idempotent, so retrying a
	// failed cleanup is always safe.
	DeleteObject(ctx context.Context, key string) error
}

// ValidateObjectKey enforces the rules ErrInvalidObjectKey describes. It is
// shared by every implementation -- the local store below and the S3-backed
// one in the objectstore/s3 subpackage -- so that a key accepted by one
// backend is accepted by all of them, and the checks run before an operation
// touches the backend. Exported because the split-out subpackage cannot
// otherwise share this package's own key grammar; the implementation lives
// in its own package so that importing it is the only way a consumer pays
// for its backend's dependencies.
func ValidateObjectKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: the key is empty", ErrInvalidObjectKey)
	}
	if len(key) > maxObjectKeyLength {
		return fmt.Errorf("%w: the key is longer than %d bytes", ErrInvalidObjectKey, maxObjectKeyLength)
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" {
			return fmt.Errorf("%w: the key must not start or end with %q or contain an empty segment", ErrInvalidObjectKey, "/")
		}
		if segment == "." || segment == ".." {
			return fmt.Errorf("%w: a segment must not be %q", ErrInvalidObjectKey, segment)
		}
		if len(segment) > maxObjectSegmentLength {
			return fmt.Errorf("%w: a segment is longer than %d bytes", ErrInvalidObjectKey, maxObjectSegmentLength)
		}
		for i := 0; i < len(segment); i++ {
			switch segment[i] {
			case 0:
				return fmt.Errorf("%w: the key must not contain a NUL byte", ErrInvalidObjectKey)
			case '\\':
				// A backslash is an ordinary character on POSIX file
				// systems, so the local backend could store it, but it is
				// the path separator on others. The grammar refuses it so
				// that a key is storable everywhere.
				return fmt.Errorf("%w: the key must not contain a backslash", ErrInvalidObjectKey)
			}
		}
	}
	return nil
}
