// This suite lives in package unittest — this module's dedicated unit-test
// directory for unit-tier checks with no single source file as their target
// (the backend coding standard's testing-layout rule); a repo-shape check is
// such a suite. It tests the repository's workspace wiring, not pkgcore's
// own symbols, so it runs black-box from outside package pkgcore.
package unittest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGoWorkUseBlock_ListsEveryModuleDirectory guards against a class of bug
// invisible to every other check in the repo: a new Go module added under
// go/ or examples/, with a complete and well-formed go.mod, but never added
// to go.work's own `use` block. Inside the workspace this makes every
// command a contributor is told to run from the new module's own directory
// -- `go build ./...`, `go vet ./...`, `go test ./...` -- fail identically
// with "pattern ./...: directory prefix . does not contain modules listed
// in go.work or their selected dependencies", while the repo-root wildcard
// build (`go build github.com/vislake/speed/go/... github.com/vislake/speed/examples/...`)
// still exits 0: it silently never resolves into the missing module at all,
// so a green root build is not evidence the new module is wired in.
//
// The failure shape is not hypothetical: any module in this state -- fully
// implemented, tested and documented (go.mod, doc.go, source, tests), but
// absent from go.work's `use` block -- cannot be `go get`/imported by any
// other workspace member, and nothing else fails until a hand edit of
// go.work or this check.
//
// The check runs from pkgcore rather than from whichever module goes
// missing, deliberately: a module absent from go.work's use block cannot
// resolve `./...` at all (see above), so it can never run a test of its own
// that would catch this -- the check needs a home guaranteed to already be
// correctly wired. pkgcore is the dependency floor: if it were not
// registered in go.work, nothing in the workspace would build, so anchoring
// the check here guarantees the check itself always runs.
//
// Skipped outside the monorepo checkout (e.g. a standalone `go get` of this
// module alone, with no sibling go.work): this is a property of this
// repository's own workspace wiring, not of pkgcore's public API, and must
// not fail for a downstream consumer that only ever sees this module in
// isolation.
func TestGoWorkUseBlock_ListsEveryModuleDirectory(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) did not report this file's own path")
	}
	// The check lives in go/pkgcore/unittest/. The monorepo checkout's
	// go.work anchors at the repo root, which is found by walking up from
	// this module's root until a go.work appears (see repoRootAboveGoWork);
	// walking rather than counting fixed levels keeps the anchor correct
	// wherever the unittest directory sits inside the module.
	moduleRoot := moduleRootOf(t, thisFile)
	repoRoot, found := repoRootAboveGoWork(t, moduleRoot)
	if !found {
		t.Skipf("no go.work above %s -- not running inside the speed monorepo checkout", moduleRoot)
	}
	goWorkPath := filepath.Join(repoRoot, "go.work")

	// Shelling out to `go work edit -json` (mirroring standalone_build_test.go's
	// own use of os/exec against the go tool) parses go.work exactly the way
	// the toolchain itself does -- comments, single-line vs factored-block
	// `use` forms, and formatting all included -- rather than re-implementing
	// go.work's grammar by hand for a check this narrow.
	cmd := exec.Command("go", "work", "edit", "-json", goWorkPath)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go work edit -json %s: %v", goWorkPath, err)
	}

	var parsed struct {
		Use []struct {
			DiskPath string
		}
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse `go work edit -json` output: %v\n%s", err, out)
	}

	used := make(map[string]bool, len(parsed.Use))
	for _, u := range parsed.Use {
		used[filepath.Clean(u.DiskPath)] = true
	}

	var missing []string
	for _, parent := range []string{"go", "examples"} {
		entries, err := os.ReadDir(filepath.Join(repoRoot, parent))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Join(repoRoot, parent), err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			modDir := filepath.Join(parent, entry.Name())
			if _, err := os.Stat(filepath.Join(repoRoot, modDir, "go.mod")); err != nil {
				continue // not a Go module directory
			}
			if !used[filepath.Clean(modDir)] {
				missing = append(missing, "./"+filepath.ToSlash(modDir))
			}
		}
	}

	if len(missing) > 0 {
		t.Fatalf(
			"found go.mod in these directories with no entry in go.work's `use` block: %s\n"+
				"add each with `go work use <dir>` -- until fixed, `go build`/`go vet`/`go test ./...` "+
				"all fail from inside the module itself with \"pattern ./...: directory prefix . does not "+
				"contain modules listed in go.work or their selected dependencies\", even though the "+
				"repo-root wildcard build stays green by silently never resolving into it",
			strings.Join(missing, ", "),
		)
	}
}

// repoRootAboveGoWork walks upward from dir -- the module root the check
// anchors on -- until it finds a directory containing go.work, and returns
// that directory. The second result reports whether a go.work was found
// before the filesystem root: false means this test is not running inside
// the speed monorepo checkout (e.g. a standalone `go get` of this module
// alone), and the caller skips, exactly as this test always did when no
// go.work sat where the repo root should be.
func repoRootAboveGoWork(t *testing.T, dir string) (string, bool) {
	t.Helper()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, true
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", filepath.Join(dir, "go.work"), err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
