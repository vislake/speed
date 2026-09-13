package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// fileMode is what a log file is created with. The file carries whatever the
// process logs, which is why it is not group- or world-readable; a collector
// that has to read it is given access through the directory.
const fileMode = 0o600

// archiveStamp is the layout of the timestamp in an archive's name, to the
// second, in local time so that it reads the way an operator reads the clock.
const archiveStamp = "20060102-150405"

// rotatingFile is the file destination's sink: the active file, its rotation
// and the pruning of its archives.
//
// It holds no lock of its own. Every method runs under the destination writer's
// lock, so a rotation has no write in flight and nothing lands in a file that is
// part-way through being renamed.
type rotatingFile struct {
	// path is the active file and never changes. Rotation renames the file
	// out of the way and creates path anew, so tail and an external
	// collector keep up and a restart opens the file that is current rather
	// than one that has already rolled away.
	path string
	// dir, prefix and ext are path taken apart, and an archive is built back
	// out of them.
	dir    string
	prefix string
	ext    string

	// maxSize is the size in bytes a write may not take the file past, 0
	// when rotation by size is off.
	maxSize int64
	// maxFiles is how many archives are kept, 0 when pruning by count is off.
	maxFiles int
	// maxAgeDays is how long an archive is kept, 0 when pruning by age is off.
	maxAgeDays int

	// f is the open active file and size is how much has been written to it.
	// f is nil when a rotation could not create the new file; writes then
	// fail rather than reopen, because this module does not reopen a
	// destination behind its own back.
	f    *os.File
	size int64

	// now is the clock the archive names and the age judgement read. It is a
	// field so that the tests need not wait out a retention period.
	now func() time.Time
	// pruneInterval is how often the background prune runs. It is fixed at
	// an hour in a running process: it only affects how promptly an
	// over-age archive goes, and the retention is counted in days.
	pruneInterval time.Duration
	// lock is the destination writer's lock, which the background prune
	// takes for each pass.
	lock sync.Locker
	// stop ends the background prune and is nil when neither pruning limit
	// is set: with nothing to prune there is no goroutine to run for the
	// life of the process.
	stop chan struct{}
	// done is closed once the background prune has returned, so that closing
	// the writer can wait for it.
	done     chan struct{}
	stopOnce sync.Once
	// pruneFailureReported says a failed prune has already been reported, so
	// that a destination whose directory has gone does not report the same
	// fault on every pass.
	pruneFailureReported bool
}

// backgroundPruneInterval is how often a file destination prunes on its own.
const backgroundPruneInterval = time.Hour

// openRotatingFile opens the active file of a file destination and prunes its
// archives once, so that a process which has been down past the retention
// period does not carry the old archives until its first rotation.
func openRotatingFile(path string, params fileParams) (*rotatingFile, error) {
	return newRotatingFile(path, int64(params.maxSizeMB)<<20, params.maxFiles, params.maxAgeDays,
		time.Now, backgroundPruneInterval)
}

// newRotatingFile is openRotatingFile with the size in bytes, the clock and the
// background interval given outright.
func newRotatingFile(path string, maxSize int64, maxFiles, maxAgeDays int,
	now func() time.Time, pruneInterval time.Duration,
) (*rotatingFile, error) {
	ext := filepath.Ext(path)
	rf := &rotatingFile{
		path:          path,
		dir:           filepath.Dir(path),
		prefix:        filepath.Base(path[:len(path)-len(ext)]),
		ext:           ext,
		maxSize:       maxSize,
		maxFiles:      maxFiles,
		maxAgeDays:    maxAgeDays,
		now:           now,
		pruneInterval: pruneInterval,
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	rf.f = f
	rf.size = info.Size()
	rf.prune()
	return rf, nil
}

// Write appends one complete record, rotating first when it would not fit.
//
// The decision is on the call boundary, so a record is never split over two
// files: half a record in each of two files is a pair of fragments neither of
// which parses on its own.
func (rf *rotatingFile) Write(p []byte) (int, error) {
	if rf.f == nil {
		return 0, fmt.Errorf("log: the active file %s is not open", rf.path)
	}
	if rf.shouldRotate(int64(len(p))) {
		if err := rf.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := rf.f.Write(p)
	rf.size += int64(n)
	return n, err
}

// shouldRotate answers whether the record about to be written has to go into a
// fresh file.
//
// A record longer than the limit all by itself rotates and is then written
// whole, past the limit: not splitting a record comes before not exceeding the
// size, so an oversized record ends up with an archive to itself. An empty
// active file is not rotated, since renaming it away would only leave an empty
// archive behind and the record would still not fit.
func (rf *rotatingFile) shouldRotate(record int64) bool {
	return rf.maxSize > 0 && rf.size > 0 && rf.size+record > rf.maxSize
}

// rotate renames the active file to an archive and creates path anew, then
// prunes: the count limit takes effect the moment an archive appears rather
// than at the next background pass.
func (rf *rotatingFile) rotate() error {
	if err := rf.f.Close(); err != nil {
		return err
	}
	rf.f = nil
	if err := os.Rename(rf.path, rf.archiveName()); err != nil {
		return err
	}
	f, err := os.OpenFile(rf.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	rf.f = f
	rf.size = 0
	rf.prune()
	return nil
}

// archiveName is the name the active file is renamed to: the base name, the
// timestamp to the second, and the extension.
//
// A second rotation inside the same second, or one into a second an earlier run
// of the process already used, takes a sequence number, so an archive is never
// overwritten by the next one.
func (rf *rotatingFile) archiveName() string {
	stamp := rf.now().Format(archiveStamp)
	name := filepath.Join(rf.dir, rf.prefix+"-"+stamp+rf.ext)
	for seq := 1; seq <= maxArchiveSequence; seq++ {
		if _, err := os.Lstat(name); err != nil {
			return name
		}
		name = filepath.Join(rf.dir, rf.prefix+"-"+stamp+"-"+strconv.Itoa(seq)+rf.ext)
	}
	// Past the sequence the second is full to a degree no rotation rate
	// reaches; the nanosecond keeps the name unique rather than losing an
	// archive to an overwrite.
	return filepath.Join(rf.dir, fmt.Sprintf("%s-%s-%d%s", rf.prefix, stamp, rf.now().Nanosecond(), rf.ext))
}

// maxArchiveSequence bounds the search for a free sequence number, so a file
// system that keeps reporting a name as taken cannot spin here.
const maxArchiveSequence = 1000

// release closes the active file. The writer has already been marked closed, so
// no record reaches the descriptor after this.
func (rf *rotatingFile) release() error {
	if rf.f == nil {
		return nil
	}
	err := rf.f.Close()
	rf.f = nil
	return err
}
