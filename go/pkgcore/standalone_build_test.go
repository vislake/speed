package pkgcore

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestModuleBuildsStandaloneOutsideWorkspace guards against a class of bug
// that go.work silently hides: inside the workspace, go.work's own `go`
// directive governs toolchain selection for every member module, so
// `go build`/`go vet` can succeed even when this module's OWN go.mod carries
// a `go` line its go.sum does not actually support -- e.g. an un-patched
// `go 1.23` whose content is only valid for the fully-qualified `go 1.23.0`
// form. A real consumer of this module has no go.work to hide behind, and
// pkgcore's go.mod carries no local `replace` lines: it is the dependency
// floor and imports no other speed module, so its go.sum must be
// standalone-clean on its own. The failure surfaces under GOWORK=off as
// "go: updates to go.mod needed; to update it: go mod tidy" from
// `go build`/`go vet`, while the go.work-based build stays green and
// surfaces nothing; the remedy is `go mod tidy` run from inside this
// module's own directory. This test exists so a regression of the same
// shape -- here, or from any other dependency change that needs a
// `go mod tidy` this module's go.mod does not yet reflect -- fails
// `go test ./...` instead of waiting for someone to think to try GOWORK=off
// by hand.
//
// pkgcore is the dependency floor every other module sits on -- go/tenancy's
// own go.mod carries a local `replace` to it -- so a broken standalone build
// here is worse than in a leaf module: every downstream module's own copy of
// this same test assumes pkgcore itself resolves cleanly outside the
// workspace.
func TestModuleBuildsStandaloneOutsideWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to `go build`/`go vet` against the module's full standalone dependency graph; skipped in -short")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) did not report this file's own path")
	}
	moduleDir, err := filepath.Abs(filepath.Dir(thisFile))
	if err != nil {
		t.Fatalf("resolve this module's directory: %v", err)
	}

	for _, args := range [][]string{
		{"build", "./..."},
		{"vet", "./..."},
	} {
		cmd := exec.Command("go", args...)
		cmd.Dir = moduleDir
		// GOWORK=off is the whole point: it is what a real consumer -- and
		// this repo's own post-first-tag release -- actually sees. Appending
		// wins over any GOWORK already in the inherited environment (only
		// the last value for a duplicate key is used; see os/exec's Env doc).
		cmd.Env = append(os.Environ(), "GOWORK=off")

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf(
				"go %s failed with GOWORK=off in %s -- this module's go.mod/go.sum cannot stand on their own outside go.work.\n"+
					"Run `go mod tidy` inside this module's directory and commit the result.\n\nOutput:\n%s",
				strings.Join(args, " "), moduleDir, out)
		}
	}
}
