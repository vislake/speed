package pkgcore

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
// This exact failure shipped once: go/ratelimit was fully implemented,
// tested and documented -- AGENTS.md, doc.go, limiter.go, limiter_test.go,
// example_test.go, a well-formed go.mod with its own `replace` directive to
// pkgcore -- but was never added to go.work's `use` block, so no other
// module in the workspace, including its own documented consumers (authn,
// ai-gateway, sharing, integration -- see docs/internal/01-architecture.md),
// could `go get`/import it without someone first hand-editing go.work.
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
	// go/pkgcore/<this file> -- the repo root is two levels up.
	repoRoot, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root from %s: %v", thisFile, err)
	}

	goWorkPath := filepath.Join(repoRoot, "go.work")
	if _, statErr := os.Stat(goWorkPath); statErr != nil {
		if os.IsNotExist(statErr) {
			t.Skipf("no go.work at %s -- not running inside the speed monorepo checkout", goWorkPath)
		}
		t.Fatalf("stat %s: %v", goWorkPath, statErr)
	}

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
