package log

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pruning has three occasions, and all three are needed.
//
// Opening the destination covers a process that was down for longer than the
// retention period. A rotation covers the count limit, which would otherwise
// only take effect at the next background pass. The background pass covers
// everything else: a configuration with rotation switched off never rotates, so
// hanging the pruning off rotation alone would never delete anything, and a
// long-running process would otherwise wait for its next start to clear the
// archives of this run.

// archiveFile is one archive of a file destination, with the time its name
// carries. The age is judged by that timestamp and not by the file system's
// modification time: backup, copy and sync tools rewrite the modification time,
// while the name was written by this module and matches what is inside.
type archiveFile struct {
	name  string
	stamp time.Time
	seq   int
}

// prune deletes the archives that are over either limit. The condition is an
// or: what survives is both within the newest few and within the retention
// period. The caller holds the destination's lock, which is also the lock the
// writes and the rotation run under, so nothing is written into a file while it
// is being deleted, and the active file is never a candidate.
func (rf *rotatingFile) prune() {
	if rf.maxFiles <= 0 && rf.maxAgeDays <= 0 {
		return
	}
	entries, err := os.ReadDir(rf.dir)
	if err != nil {
		rf.notePruneFailure("log: the archives of %s could not be listed: %v", rf.path, err)
		return
	}
	current := filepath.Base(rf.path)
	archives := make([]archiveFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == current {
			continue
		}
		if a, ok := rf.parseArchive(entry.Name()); ok {
			archives = append(archives, a)
		}
	}
	// Newest first, so that the count limit is a prefix of the slice.
	slices.SortFunc(archives, func(a, b archiveFile) int {
		if !a.stamp.Equal(b.stamp) {
			return b.stamp.Compare(a.stamp)
		}
		return b.seq - a.seq
	})
	cutoff := rf.now().AddDate(0, 0, -rf.maxAgeDays)
	failed := false
	for i, a := range archives {
		overCount := rf.maxFiles > 0 && i >= rf.maxFiles
		overAge := rf.maxAgeDays > 0 && a.stamp.Before(cutoff)
		if !overCount && !overAge {
			continue
		}
		if err := os.Remove(a.name); err != nil && !os.IsNotExist(err) {
			rf.notePruneFailure("log: the archive %s could not be deleted: %v", a.name, err)
			failed = true
		}
	}
	if !failed {
		rf.pruneFailureReported = false
	}
}

// notePruneFailure reports the first failure of a run of them. A destination
// whose directory has gone takes the same fault on every pass, and a line each
// would flood standard error the way a repeated write failure would.
func (rf *rotatingFile) notePruneFailure(format string, args ...any) {
	if rf.pruneFailureReported {
		return
	}
	rf.pruneFailureReported = true
	report(format, args...)
}

// parseArchive reads an archive's name, and reports that a file is not one when
// the name does not carry the shape this module writes. Nothing else in the
// directory is touched: this module does not delete files it did not produce.
func (rf *rotatingFile) parseArchive(base string) (archiveFile, bool) {
	prefix := rf.prefix + "-"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, rf.ext) {
		return archiveFile{}, false
	}
	middle := base[len(prefix) : len(base)-len(rf.ext)]
	parts := strings.Split(middle, "-")
	if len(parts) != 2 && len(parts) != 3 {
		return archiveFile{}, false
	}
	stamp, err := time.ParseInLocation(archiveStamp, parts[0]+"-"+parts[1], time.Local)
	if err != nil {
		return archiveFile{}, false
	}
	seq := 0
	if len(parts) == 3 {
		if seq, err = strconv.Atoi(parts[2]); err != nil || seq < 0 {
			return archiveFile{}, false
		}
	}
	return archiveFile{name: filepath.Join(rf.dir, base), stamp: stamp, seq: seq}, true
}

// startBackground starts the periodic pruning, taking mu for each pass so that
// it is mutually exclusive with the writes and the rotation.
//
// With both limits switched off there is nothing to delete, and no goroutine is
// started at all: one that runs for the life of the process to find nothing on
// every pass is a cost with no return.
func (rf *rotatingFile) startBackground(mu sync.Locker) {
	if rf.maxFiles <= 0 && rf.maxAgeDays <= 0 {
		return
	}
	rf.lock = mu
	rf.stop = make(chan struct{})
	rf.done = make(chan struct{})
	go rf.backgroundPrune()
}

// backgroundPrune prunes on every tick until the writer stops it.
func (rf *rotatingFile) backgroundPrune() {
	defer close(rf.done)

	ticker := time.NewTicker(rf.pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rf.stop:
			return
		case <-ticker.C:
			rf.lock.Lock()
			rf.prune()
			rf.lock.Unlock()
		}
	}
}

// stopBackground ends the periodic pruning and waits for the pass in flight to
// return. Waiting is the point: a pass that outlived the writer would take the
// lock of a closed destination and delete files after the process thought it
// was done with them, and nothing on the shutdown path would be waiting for it.
func (rf *rotatingFile) stopBackground() {
	if rf.stop == nil {
		return
	}
	rf.stopOnce.Do(func() { close(rf.stop) })
	<-rf.done
}
