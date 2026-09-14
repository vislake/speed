package http

import (
	"os/exec"
	"strings"
	"testing"
)

// TestHTTPHasNoThirdPartyDependencies is the executable form of the design's
// surface statement: the registration surface carries net/http's types and
// this package's types and nothing else, so a host that registers a route
// takes on no routing library. It is also what keeps the Spec check to a shape
// check — an OpenAPI parsing library would land here the moment one is
// imported.
//
// The scan is over ./... rather than the root package alone, so a test host
// under internal/ cannot pull one in through the back door either.
func TestHTTPHasNoThirdPartyDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	own := []string{
		"github.com/vislake/speed/pkg/core",
		"github.com/vislake/speed/pkg/config",
		"github.com/vislake/speed/pkg/log",
		"github.com/vislake/speed/pkg/http",
	}
	scanned := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" {
			continue
		}
		scanned++
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
			t.Errorf("http depends on %s, which is outside the standard library", pkg)
		}
	}
	if scanned == 0 {
		t.Fatal("go list -deps listed no package, so the scan above proved nothing")
	}
}
