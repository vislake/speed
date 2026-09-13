package core

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCoreHasNoThirdPartyDependencies is the executable form of the first
// architecture invariant: core is imported by every module, so any dependency
// of its own would land in every host's dependency list. A standard library
// path has no domain in its first segment; anything else does.
func TestCoreHasNoThirdPartyDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" || strings.HasPrefix(pkg, "github.com/vislake/speed/pkg/core") {
			continue
		}
		first, _, _ := strings.Cut(pkg, "/")
		if strings.Contains(first, ".") {
			t.Errorf("core depends on %s, which is outside the standard library", pkg)
		}
	}
}
