package pkgcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// objectStoreLocalConfig is the "objectstore.local" component's configuration
// schema: Directory names the store root. An empty value falls back to a
// fresh private temporary directory the component owns and removes at Close
// (the throwaway default); a host-supplied directory is the host's data and
// is never removed.
type objectStoreLocalConfig struct {
	Directory string `json:"directory"`
}

// objectStoreLocalComponent is the component descriptor for
// "objectstore.local", the local-directory store a composition configuration
// selects as the "objectstore" module's implementation; it registers itself
// from this file's init. Capabilities are deliberately 0: one component
// covers both the throwaway temporary-directory default and a host-supplied
// persistent directory, and under-declaring is the safe direction -- no
// deployment mode requires the bit, and a host with a persistent directory
// can select its own provider component when it wants the durability bit
// declared. Its New funnels through newLocalObjectStoreFromDirectory: when
// the component created the temporary directory, the returned value carries
// its own Close() error, which the assembly's close stage runs in place of
// a declared Close callback, while a store over a host-supplied directory
// carries no closer and has nothing to release.
var objectStoreLocalComponent = Component{
	Name:         "objectstore.local",
	Module:       "objectstore",
	Provides:     []any{(*ObjectStore)(nil)},
	Capabilities: 0,
	ConfigSchema: (*objectStoreLocalConfig)(nil),
	New: func(_ context.Context, _ *ComponentRegistry, cfg ComponentConfig) (any, error) {
		var c objectStoreLocalConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return newLocalObjectStoreFromDirectory(c.Directory)
	},
}

func init() { MustRegister(objectStoreLocalComponent) }

// localObjectStore is the standalone deployment mode's ObjectStore: a
// directory on the local file system, one file per object, keys mapped onto
// paths below the root. Files are written to a temporary sibling and renamed
// into place, so a PutObject is atomic exactly as the interface promises:
// readers see the old object or the new one, never a torn write, and a failed
// PutObject leaves the previous object behind.
//
// The key, joined onto the root, must also fit the file system's own
// path-length limit. The shared grammar caps keys at 1024 bytes for the S3
// side's sake, but a key that long is only storable locally when the root
// directory leaves room for it below the limit, so a valid key that a deep
// root makes unrepresentable is refused by the file system with an ordinary
// wrapped error, not with ErrInvalidObjectKey: the store reports the sentinel
// for the rules it enforces itself and a plain error for its environment's.
//
// The root directory is private to the store, and the store refuses to
// follow a symbolic link anywhere on a key's path: the grammar already keeps
// keys inside the root, and links are the one way a path below the root
// could still reach outside it, so a link planted in the tree (by a process
// with write access to the root, or by the file system the root lives on) is
// treated as a breach of the store's integrity rather than as routing. An
// object whose path crosses a link is reported as not found, because the key
// is not addressable in the store's tree; putting or deleting through one is
// refused outright, because silently accepting or silently dropping the
// operation would both be wrong. Checking each segment is cheap, and a
// store whose root never contains links is unaffected by the rule.
//
// The store doubles as the test double for code written against ObjectStore:
// it implements the same contract as the S3 store, shares no state between
// instances, and needs no external service, so unit tests run against it
// with nothing else running. It is safe for concurrent use: the struct holds
// only the resolved root, and every operation below it is atomic on the file
// system level.
type localObjectStore struct {
	root string
}

// NewLocalObjectStore returns the standalone deployment mode's ObjectStore,
// storing objects as files below directory. directory is created if it does
// not exist and resolved to its canonical path, so a store whose root is a
// symbolic link stores below the link's target. Objects survive the store:
// the same directory handed back to a later NewLocalObjectStore reads what
// the earlier store wrote, which is how a standalone-mode host keeps
// objects across restarts. An unusable directory (one that cannot be
// created, one that is a file rather than a directory) panics: it is an
// unrecoverable wiring error at startup, the same failure mode the SMTP
// mailer uses for an unusable configuration.
func NewLocalObjectStore(directory string) ObjectStore {
	if directory == "" {
		panic("pkgcore: NewLocalObjectStore requires a non-empty directory")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		panic(fmt.Sprintf("pkgcore: NewLocalObjectStore: %v", err))
	}
	// Newly created store directories are 0o750 (owner and group), tighter
	// than the 0o755 umask default that gosec's G301 calls out: the store is
	// process-private -- nothing outside the owning process reads its tree,
	// and it may hold PII objects. A host-injected directory is never
	// affected, because MkdirAll leaves an existing tree's permissions alone.
	// mkdirErr so the filepath.Abs error above is not shadowed: the error
	// variables of the Abs/EvalSymlinks/Stat chain below are reassignments
	// of the same err, and govet's shadow check flags an if-init err here.
	if mkdirErr := os.MkdirAll(absolute, 0o750); mkdirErr != nil {
		panic(fmt.Sprintf("pkgcore: NewLocalObjectStore: %v", mkdirErr))
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		panic(fmt.Sprintf("pkgcore: NewLocalObjectStore: %v", err))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		panic(fmt.Sprintf("pkgcore: NewLocalObjectStore: %v", err))
	}
	if !info.IsDir() {
		panic(fmt.Sprintf("pkgcore: NewLocalObjectStore: %q is not a directory", directory))
	}
	return &localObjectStore{root: resolved}
}

// PutObject implements ObjectStore.PutObject by streaming r into a temporary
// file in the object's directory, syncing it, and renaming it over the
// object's path, so that a concurrent reader sees the old object or the new
// one, never a partial write, and a failed upload leaves no trace behind.
func (s *localObjectStore) PutObject(ctx context.Context, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateObjectKey(key); err != nil {
		return err
	}

	segments := strings.Split(key, "/")
	dir := s.root
	for _, segment := range segments[:len(segments)-1] {
		child := filepath.Join(dir, segment)

		// Resolve the segment as a directory, creating it when it is absent.
		// The Mkdir is raced by concurrent PutObjects of sibling keys: the
		// loser sees EEXIST and must not walk on from the stale parent -- the
		// path it is about to join the remaining segments onto no longer is
		// the tree's parent -- nor assume the EEXIST names the winner's
		// directory, so the loop goes around and inspects what actually
		// appeared under the same rules as the Lstat above: a plain directory
		// is descended into, a symbolic link or an existing object is
		// refused. Going around also covers the winner's directory being
		// removed again before it is inspected (a concurrent DeleteObject can
		// clear the empty directory a racing PutObject just created), in
		// which case the next round's Mkdir simply succeeds.
		for {
			info, err := os.Lstat(child)
			if err == nil {
				switch {
				case info.Mode()&os.ModeSymlink != 0:
					return fmt.Errorf("pkgcore: local object store: refusing to follow the symbolic link at %q", child)
				case !info.IsDir():
					return fmt.Errorf("pkgcore: local object store: the path at %q is an existing object, and a key below it cannot be stored", child)
				}
				dir = child
				break
			}
			if !os.IsNotExist(err) {
				return fmt.Errorf("pkgcore: local object store: %w", err)
			}
			// 0o750 like the store root (see NewLocalObjectStore).
			mkdirErr := os.Mkdir(child, 0o750)
			if mkdirErr == nil {
				dir = child
				break
			}
			if !os.IsExist(mkdirErr) {
				return fmt.Errorf("pkgcore: local object store: %w", mkdirErr)
			}
			// A concurrent PutObject created the directory between the Lstat
			// and the Mkdir; go around and inspect what actually appeared.
		}
	}

	temporary, err := os.CreateTemp(dir, ".pkgcore-object-*")
	if err != nil {
		return fmt.Errorf("pkgcore: local object store: %w", err)
	}
	cleanedUp := false
	defer func() {
		if !cleanedUp {
			// Best-effort cleanup on the error paths: the error the function
			// is about to return is the failure that matters, and there is
			// no caller left to report a cleanup failure to.
			_ = temporary.Close()
			_ = os.Remove(temporary.Name())
		}
	}()
	// CreateTemp creates with mode 0600. Objects are stored as ordinary
	// files, readable the way the file system's own permissions would make
	// them, so the umask-free 0644 stands in for the default file creation
	// mode the caller would otherwise get.
	if err := temporary.Chmod(0o644); err != nil {
		return fmt.Errorf("pkgcore: local object store: %w", err)
	}

	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := r.Read(buffer)
		if n > 0 {
			if _, err := temporary.Write(buffer[:n]); err != nil {
				return fmt.Errorf("pkgcore: local object store: %w", err)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return fmt.Errorf("pkgcore: local object store: %w", readErr)
		}
	}
	// The rename publishes the object; syncing first makes sure the bytes
	// are on disk before the name that points at them is.
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("pkgcore: local object store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("pkgcore: local object store: %w", err)
	}

	// Rename replaces whatever sits at the final path -- the previous object,
	// a symbolic link (replaced, never written through), or nothing -- and
	// does so atomically; it never follows a symbolic link at the final path,
	// so a link planted there is replaced, not written through. One thing
	// rename cannot replace is a directory. A directory at the final path is
	// the trace of longer keys: deleting a deep key removes its object file
	// but leaves the directories that led to it behind, empty once the object
	// is gone. Storing a key only such empty traces stand in the way of is
	// legal -- no object any longer claims the tree below the key -- so the
	// traces are cleared and the rename retried once. The clearing decides
	// whether the put may proceed at all: only empty directories can be
	// removed, so a subtree that still holds a stored object anywhere below
	// it survives and the put is refused as the prefix clash it would be.
	// A trace can also vanish between the failed rename and the clearing (a
	// concurrent delete removing it), which is the same "retry the rename"
	// outcome, and a concurrent put can refill a trace during the clearing,
	// which surfaces as the refusal or the retried rename's error: the two
	// puts race for keys that cannot coexist, so one of them failing is a
	// legal outcome, exactly as it is on the other clash points. File systems
	// disagree on which error a file-to-directory rename is -- EEXIST here,
	// EISDIR on Linux -- so both are treated as the same "a directory sits
	// there" signal.
	finalPath := filepath.Join(dir, segments[len(segments)-1])
	publishErr := os.Rename(temporary.Name(), finalPath)
	if errors.Is(publishErr, syscall.EEXIST) || errors.Is(publishErr, syscall.EISDIR) {
		if err := s.removeEmptyTraces(finalPath); err != nil {
			return err
		}
		publishErr = os.Rename(temporary.Name(), finalPath)
	}
	if publishErr != nil {
		return fmt.Errorf("pkgcore: local object store: %w", publishErr)
	}
	cleanedUp = true
	return nil
}

// removeEmptyTraces removes directory and the empty-directory traces below
// it, the remnants a deep key's deletion left behind, so that a later put of
// the directory's key can rename its object file onto the cleared path. The
// whole subtree must consist of directories: the clearing runs only because
// no object may be stored under the key being put, so a subtree holding a
// stored object, a symbolic link or a foreign entry anywhere below is
// refused untouched. Entries are judged by their directory-entry type, never
// by opening them, and every removal is an rmdir, which removes nothing but
// an empty directory: an object that a concurrent put lands mid-clearing is
// never deleted, and the refusal names the directory the put's key would
// become a proper prefix of. A directory that vanishes mid-clearing (a
// concurrent delete removing the same trace) is treated as already cleared,
// DeleteObject's own empty-directory race tolerance.
func (s *localObjectStore) removeEmptyTraces(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("pkgcore: local object store: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("pkgcore: local object store: the path at %q is a directory that still holds objects stored under longer keys, and a key must not be a proper prefix of a stored key", path)
		}
		// Named for the govet shadow check: the ReadDir error above stays
		// live below, for the rmdir of the path itself.
		if subErr := s.removeEmptyTraces(filepath.Join(path, entry.Name())); subErr != nil {
			return subErr
		}
	}
	err = syscall.Rmdir(path)
	if err == nil || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTDIR) {
		return fmt.Errorf("pkgcore: local object store: the path at %q is a directory that still holds objects stored under longer keys, and a key must not be a proper prefix of a stored key", path)
	}
	return fmt.Errorf("pkgcore: local object store: %w", err)
}

// GetObject implements ObjectStore.GetObject by opening the object's file
// and wrapping it in a reader that fails its reads once the context that
// started the request is done. The object is not opened through symbolic
// links, and a key that names a link, a directory or nothing at all is
// reported as not found: none of those is an object in the store's tree.
func (s *localObjectStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateObjectKey(key); err != nil {
		return nil, err
	}

	segments := strings.Split(key, "/")
	dir := s.root
	for _, segment := range segments[:len(segments)-1] {
		child := filepath.Join(dir, segment)
		info, err := os.Lstat(child)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrObjectNotFound
			}
			return nil, fmt.Errorf("pkgcore: local object store: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrObjectNotFound
		}
		dir = child
	}

	path := filepath.Join(dir, segments[len(segments)-1])
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("pkgcore: local object store: %w", err)
	}
	// Only a regular file is an object. A directory at the final path is a
	// trace of longer keys, never an object itself; anything else that is not
	// a regular file is foreign to the store.
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, ErrObjectNotFound
	}
	// O_NOFOLLOW refuses to open the path if it became a symbolic link after
	// the Lstat above looked at it, so the file the reader ends up streaming
	// is the regular file that was just inspected.
	//nolint:gosec // G304: the path is an Lstat-verified regular file under the store root
	// (ValidateObjectKey rejects traversal), and O_NOFOLLOW makes the open
	// refuse exactly the symlink swap gosec worries about.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("pkgcore: local object store: %w", err)
	}
	return &localObjectReader{ctx: ctx, file: file}, nil
}

// DeleteObject implements ObjectStore.DeleteObject by removing the object's
// file. Deleting anything that is not an object is a success: a missing key
// and a symbolic link (the store never follows one, and removal through it
// would be a write) are left alone. A directory is the trace of longer keys,
// never an object, so an empty one is removed while one that still holds
// longer keys' objects stays in place -- deleting it would delete the objects
// below it, and the key being deleted never was an object anyway. This keeps
// DeleteObject idempotent exactly as deleting a key that has no object is
// idempotent on the S3 store.
func (s *localObjectStore) DeleteObject(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateObjectKey(key); err != nil {
		return err
	}

	segments := strings.Split(key, "/")
	dir := s.root
	for _, segment := range segments[:len(segments)-1] {
		child := filepath.Join(dir, segment)
		info, err := os.Lstat(child)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("pkgcore: local object store: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("pkgcore: local object store: refusing to follow the symbolic link at %q", child)
		}
		if !info.IsDir() {
			return nil
		}
		dir = child
	}

	path := filepath.Join(dir, segments[len(segments)-1])
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("pkgcore: local object store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		// dirErr: the Lstat error above stays live below, where the final
		// os.Remove reuses it.
		entries, dirErr := os.ReadDir(path)
		if dirErr != nil {
			if os.IsNotExist(dirErr) {
				return nil
			}
			return fmt.Errorf("pkgcore: local object store: %w", dirErr)
		}
		if len(entries) == 0 {
			// A concurrent PutObject may have just filled the directory. An
			// empty-directory remove that loses that race is still a success:
			// the key being deleted never was an object, so nothing about the
			// outcome of this DeleteObject changes.
			// rmErr: the Lstat error above stays live below, where the final
			// os.Remove reuses it.
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) &&
				!errors.Is(rmErr, syscall.ENOTEMPTY) && !errors.Is(rmErr, syscall.EEXIST) {
				return fmt.Errorf("pkgcore: local object store: %w", rmErr)
			}
		}
		return nil
	}
	err = os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("pkgcore: local object store: %w", err)
}

// localObjectReader streams one object's file. os.File reads do not observe
// a context, so each Read checks the context that started the GetObject
// first: a cancelled context surfaces on the read loop as its error, the
// same failure a distributed-mode reader sees when its request is cut.
type localObjectReader struct {
	ctx  context.Context
	file *os.File
}

func (r *localObjectReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}

func (r *localObjectReader) Close() error {
	return r.file.Close()
}

// newLocalObjectStoreFromDirectory builds "objectstore.local" for directory,
// the single construction path the component funnels through. An empty
// directory falls back to a fresh private temporary directory whose removal
// the returned value owns (see closableObjectStore); a non-empty one is the
// host's own directory and the store never carries a closer for it.
func newLocalObjectStoreFromDirectory(directory string) (ObjectStore, error) {
	if directory == "" {
		created, err := os.MkdirTemp("", "pkgcore-object-store-*")
		if err != nil {
			return nil, fmt.Errorf("pkgcore: builtin objectstore.local seam: %w", err)
		}
		// The temp directory was created by this construction and is owned
		// by it: the returned value's Close() error removes the directory
		// again, so the assembly's Close stage (or a failed construction's
		// rollback, which closes what it built) does not leak the throwaway
		// tree. A store over a host-supplied directory never carries a
		// closer: that directory is the host's data, which nothing here may
		// delete.
		store := NewLocalObjectStore(created)
		return &closableObjectStore{ObjectStore: store, removeRoot: func() error {
			return os.RemoveAll(created)
		}}, nil
	}
	return NewLocalObjectStore(directory), nil
}

// closableObjectStore is the value "objectstore.local" produces when it
// created the store's directory itself: the store itself (whose promoted
// methods satisfy ObjectStore) plus the Close() error method that removes
// the temporary directory the construction created, which the assembly's
// close stage runs since the component descriptor declares no Close
// callback. A host that calls NewLocalObjectStore itself keeps owning its
// directory, exactly as that constructor's own doc comment promises, and
// calls Close on the result only when it wants the throwaway tree gone.
type closableObjectStore struct {
	ObjectStore
	removeRoot func() error
}

// The product's Close() error is the ownership declaration the assembly's
// close stage reads, so the compile-time assertion keeps it from being
// dropped silently.
var _ io.Closer = (*closableObjectStore)(nil)

// Close removes the temporary directory the construction created. Nothing
// may use the store after Close; a host shuts its seams down last.
func (s *closableObjectStore) Close() error {
	if s.removeRoot != nil {
		return s.removeRoot()
	}
	return nil
}
