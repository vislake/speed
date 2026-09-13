package log

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// archiveNamePattern is the shape this module writes: the base name, a
// timestamp to the second, an optional sequence number, and the extension.
var archiveNamePattern = regexp.MustCompile(`^app-\d{8}-\d{6}(-\d+)?\.log$`)

// newTestFile opens a rotating file with the size in bytes and the clock given
// outright, so that a test need not write a megabyte or wait out a retention
// period. It is closed when the test ends.
func newTestFile(t *testing.T, path string, maxSize int64, maxFiles, maxAgeDays int, now func() time.Time) *rotatingFile {
	t.Helper()
	rf, err := newRotatingFile(path, maxSize, maxFiles, maxAgeDays, now, time.Hour)
	if err != nil {
		t.Fatalf("opening %s failed: %v", path, err)
	}
	t.Cleanup(func() {
		rf.stopBackground()
		rf.release()
	})
	return rf
}

// write puts one record into the file, as a handler would: one call, one
// complete record.
func write(t *testing.T, rf *rotatingFile, record string) {
	t.Helper()
	if _, err := rf.Write([]byte(record)); err != nil {
		t.Fatalf("writing %q failed: %v", record, err)
	}
}

// otherFilesIn lists everything in the directory apart from the named file,
// sorted. It reads the directory itself rather than going through this module's
// own archive parser, so a parser that recognised nothing would not hide a
// missing archive.
func otherFilesIn(t *testing.T, dir, current string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s failed: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() != current {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}

// read returns a file's contents.
func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s failed: %v", path, err)
	}
	return string(content)
}

// TestRotateRenamesCurrentAndRecreatesPath pins what a rotation is: the current
// file is renamed to an archive and the configured path is created anew.
//
// An implementation that instead opened a new timestamped file and carried on
// writing there would leave the configured path holding the records from before
// the rotation, and tail, a collector and the next restart would all follow the
// wrong file.
func TestRotateRenamesCurrentAndRecreatesPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rf := newTestFile(t, path, 32, 0, 0, time.Now)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the active file is not there after the open: %v", err)
	}
	write(t, rf, "the first record\n")
	write(t, rf, "the second record, which does not fit\n")

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the active file is gone after the rotation: %v", err)
	}
	if os.SameFile(before, after) {
		t.Error("the rotation left the original file at the configured path; it has to be renamed away and the path created anew")
	}
	if got := read(t, path); got != "the second record, which does not fit\n" {
		t.Errorf("the active file holds %q, want only the record written after the rotation", got)
	}

	archives := otherFilesIn(t, dir, "app.log")
	if len(archives) != 1 {
		t.Fatalf("the rotation left %v, want exactly one archive", archives)
	}
	if !archiveNamePattern.MatchString(archives[0]) {
		t.Errorf("the archive is named %q, want the base name, a timestamp to the second and the extension", archives[0])
	}
	if got := read(t, filepath.Join(dir, archives[0])); got != "the first record\n" {
		t.Errorf("the archive holds %q, want what the file held before the rotation", got)
	}
}

// TestActiveFilePathIsStableAcrossRotations pins that the path an operator
// configured is always the file being written to, however many rotations have
// happened.
func TestActiveFilePathIsStableAcrossRotations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rf := newTestFile(t, path, 24, 0, 0, time.Now)

	// The first record goes into an empty file, which is not rotated; the
	// loop below is what rotates.
	write(t, rf, "the record that fills the first file\n")
	previous, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the active file is not there after the open: %v", err)
	}
	for i := range 3 {
		write(t, rf, fmt.Sprintf("record number %d, long enough to fill the file\n", i))
		current, err := os.Stat(path)
		if err != nil {
			t.Fatalf("the active file is gone after rotation %d: %v", i, err)
		}
		if os.SameFile(previous, current) {
			t.Errorf("rotation %d left the same file at the configured path", i)
		}
		previous = current
	}
	if got := read(t, path); !strings.Contains(got, "record number 2") {
		t.Errorf("the active file holds %q, want the last record written", got)
	}
	if archives := otherFilesIn(t, dir, "app.log"); len(archives) != 3 {
		t.Errorf("three rotations left %v, want one archive each", archives)
	}
}

// TestRecordIsNeverSplitAcrossFiles pins that the rotation decision is taken on
// the call boundary. Half a record in each of two files is a pair of fragments
// neither of which parses on its own.
func TestRecordIsNeverSplitAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rf := newTestFile(t, path, 200, 0, 0, time.Now)

	for i := range 20 {
		record, err := json.Marshal(map[string]any{
			"level": "INFO",
			"msg":   strings.Repeat("a", i*7%40),
			"seq":   i,
		})
		if err != nil {
			t.Fatalf("building record %d failed: %v", i, err)
		}
		write(t, rf, string(record)+"\n")
	}

	files := append([]string{"app.log"}, otherFilesIn(t, dir, "app.log")...)
	if len(files) < 3 {
		t.Fatalf("the run produced %v, want several rotations to have happened", files)
	}
	seen := 0
	for _, name := range files {
		for lineNumber, line := range strings.Split(strings.TrimSuffix(read(t, filepath.Join(dir, name)), "\n"), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Errorf("line %d of %s does not parse on its own: %v\n%s", lineNumber+1, name, err, line)
			}
			seen++
		}
	}
	if seen != 20 {
		t.Errorf("the files hold %d records between them, want the 20 that were written", seen)
	}
}

// TestOversizedRecordGetsItsOwnArchive pins the precedence: a record longer
// than the limit rotates first and is then written whole, past the limit. Not
// splitting a record comes before not exceeding the size.
func TestOversizedRecordGetsItsOwnArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rf := newTestFile(t, path, 64, 0, 0, time.Now)

	oversized := strings.Repeat("x", 200) + "\n"
	write(t, rf, "a small record\n")
	write(t, rf, oversized)

	if got := read(t, path); got != oversized {
		t.Errorf("the active file holds %d bytes, want the oversized record alone", len(got))
	}
	if int64(len(read(t, path))) <= rf.maxSize {
		t.Error("the oversized record was not written whole")
	}

	// One more record pushes the oversized one into an archive of its own.
	write(t, rf, "the next record\n")
	archives := otherFilesIn(t, dir, "app.log")
	if len(archives) != 2 {
		t.Fatalf("the run left %v, want one archive for the small record and one for the oversized record", archives)
	}
	alone := false
	for _, name := range archives {
		if read(t, filepath.Join(dir, name)) == oversized {
			alone = true
		}
	}
	if !alone {
		t.Errorf("no archive among %v holds the oversized record on its own", archives)
	}
}

// TestEmptyActiveFileIsNotRotated pins that an empty file is not rotated away
// for a record that would not fit it anyway: the archive would be empty, and it
// would count against the retention limit while holding nothing.
func TestEmptyActiveFileIsNotRotated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rf := newTestFile(t, path, 64, 0, 0, time.Now)

	oversized := strings.Repeat("x", 200) + "\n"
	write(t, rf, oversized)

	if archives := otherFilesIn(t, dir, "app.log"); len(archives) != 0 {
		t.Errorf("writing to an empty file left %v, want no archive", archives)
	}
	if got := read(t, path); got != oversized {
		t.Errorf("the active file holds %d bytes, want the record whole", len(got))
	}
}

// TestSecondRotationInSameSecondGetsSequence pins that an archive is never
// overwritten by the next one. The timestamp is only accurate to the second,
// and a busy destination rotates more often than that.
func TestSecondRotationInSameSecondGetsSequence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	frozen := time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)
	rf := newTestFile(t, path, 24, 0, 0, func() time.Time { return frozen })

	write(t, rf, "the first record\n")
	write(t, rf, "the second record, long enough\n")
	write(t, rf, "the third record, long enough\n")

	archives := otherFilesIn(t, dir, "app.log")
	if len(archives) != 2 {
		t.Fatalf("two rotations inside one second left %v, want two archives", archives)
	}
	for _, want := range []string{"app-20260913-120000.log", "app-20260913-120000-1.log"} {
		if !slices.Contains(archives, want) {
			t.Errorf("the archives are %v, want one named %q; the second rotation of a second takes a sequence number",
				archives, want)
		}
	}
	if first, second := read(t, filepath.Join(dir, archives[0])), read(t, filepath.Join(dir, archives[1])); first == second {
		t.Error("the two archives hold the same records, so one rotation overwrote the other")
	}
}

// TestRotationDisabledWhenMaxSizeZero pins that 0 switches rotation off. It is
// the configured meaning of an explicit 0, and the default of 100 MB is only
// applied when the host wrote nothing.
func TestRotationDisabledWhenMaxSizeZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rf, err := openRotatingFile(path, fileParams{maxSizeMB: 0})
	if err != nil {
		t.Fatalf("opening the file destination failed: %v", err)
	}
	t.Cleanup(func() {
		rf.stopBackground()
		rf.release()
	})
	if rf.maxSize != 0 {
		t.Fatalf("max-size-mb 0 became a limit of %d bytes, want rotation switched off", rf.maxSize)
	}

	record := strings.Repeat("y", 1023) + "\n"
	for range 1024 {
		write(t, rf, record)
	}
	if archives := otherFilesIn(t, dir, "app.log"); len(archives) != 0 {
		t.Errorf("writing a megabyte with rotation off left %v, want no archive", archives)
	}
	if got := int64(len(read(t, path))); got != int64(len(record))*1024 {
		t.Errorf("the active file holds %d bytes, want everything that was written", got)
	}
}
