package log

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLogHasNoThirdPartyDependencies is the executable form of the design's
// dependency statement: the handler chain, redaction, context storage, config
// reading, the two formats and the file rotation are all done inside the
// standard library, and this module is imported by every module that logs, so
// a dependency of its own would land in every host's dependency list.
//
// The scan is over ./... rather than the root package alone, so a test host
// under internal/ cannot pull one in through the back door either.
func TestLogHasNoThirdPartyDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	own := []string{
		"github.com/vislake/speed/pkg/core",
		"github.com/vislake/speed/pkg/config",
		"github.com/vislake/speed/pkg/log",
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" {
			continue
		}
		mine := false
		for _, prefix := range own {
			if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
				mine = true
			}
		}
		if mine {
			continue
		}
		first, _, _ := strings.Cut(pkg, "/")
		if strings.Contains(first, ".") {
			t.Errorf("log depends on %s, which is outside the standard library", pkg)
		}
	}
}
