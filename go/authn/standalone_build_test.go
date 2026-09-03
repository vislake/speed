package authn

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
// member. A real consumer of this module has no go.work to hide behind,
// and neither does the first lockstep release: docs/internal/02-repo-and-release.md
// documents that the transitional `replace` lines above get deleted
// entirely once every module has its first tag, at which point this
// module's go.mod is ALL that resolves its dependencies.
//
// This exact failure shipped once: go.mod carried the two `replace` lines
// above but no matching `require` lines, and go.sum did not exist at all.
// `go build ./...` / `go vet ./...` under GOWORK=off failed with
// "pkgcore is replaced but not required", "dbkit is replaced but not
// required", and "no required module provides package gorm.io/gorm",
// while the ordinary go.work-based build (this repo's default day-to-day
// mode) stayed green throughout and never surfaced any of it. The fix is
// `go mod tidy` run from inside this module's own directory; this test
// exists so a future regression of the same shape fails `go test ./...`
// (which already runs by default, workspace or not) instead of waiting
// for someone to think to try GOWORK=off by hand.
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
