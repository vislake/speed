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
// a `go` line its go.sum does not actually support. A real consumer of this
// module has no go.work to hide behind, and pkgcore's go.mod is standalone
// from day one -- unlike modules that still carry transitional local
// `replace` lines (see docs/internal/02-repo-and-release.md), pkgcore has
// none to begin with, since it is the dependency floor and imports no other
// speed module.
//
// This exact failure shipped once: go.mod carried `go 1.23` (no patch
// version) while go.sum's content was only valid for the fully-qualified
// `go 1.23.0` form. `go build ./...` / `go vet ./...` under GOWORK=off both
// failed with "go: updates to go.mod needed; to update it: go mod tidy",
// while the ordinary go.work-based build (this repo's default day-to-day
// mode, where go.work's own `go` directive papered over the mismatch)
// stayed green throughout and never surfaced any of it. The fix is
// `go mod tidy` run from inside this module's own directory; this test
// exists so a future regression of the same shape -- here, or from any
// other dependency change that needs a `go mod tidy` this module's go.mod
// does not yet reflect -- fails `go test ./...` (which already runs by
// default, workspace or not) instead of waiting for someone to think to
// try GOWORK=off by hand.
//
// pkgcore is the dependency floor every other module sits on -- and is
// already `replace`d by go/tenancy's own go.mod ahead of pkgcore's first
// tag -- so a broken standalone build here is worse than in a leaf module:
// every downstream module's own copy of this same test assumes pkgcore
// itself resolves cleanly outside the workspace.
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
