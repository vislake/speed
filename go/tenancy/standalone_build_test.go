package tenancy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestModuleBuildsStandaloneOutsideWorkspace guards against a class of bug
// that go.work silently hides: inside the workspace, go.work resolves this
// module's local `replace` targets (pkgcore, dbkit) itself and folds every
// workspace member's require graph together, so `go build`/`go vet` can
// succeed even when this module's OWN go.mod is incomplete -- missing a
// `require` for a `replace`d module, or for an ordinary external
// dependency that happens to be required only by some other workspace
// member. A real consumer of this module has no go.work to hide behind:
// this module's go.mod alone must resolve its dependencies, `replace`
// lines included, so an incomplete go.mod builds green under go.work
// while failing every consumer build that runs GOWORK=off. The remedy
// for such a state is `go mod tidy` run from inside this module's own
// directory; this test exists so a regression of that shape fails
// `go test ./...` (which runs by default, workspace or not) instead of
// waiting for someone to think to try GOWORK=off by hand.
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
