package config

import (
	"os/exec"
	"strings"
	"testing"
)

// TestConfigRootHasNoThirdPartyDependencies is the executable form of the rule
// that the root package stays on the standard library and pkg/core: collection,
// the layers and the config data are all within reach of it, and a dependency
// here would land on every host that declares configuration. Third-party
// dependencies belong to the transport and format subpackages, which is why
// this case lists the root package alone.
func TestConfigRootHasNoThirdPartyDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" || strings.HasPrefix(pkg, "github.com/vislake/speed/pkg/core") ||
			strings.HasPrefix(pkg, "github.com/vislake/speed/pkg/config") {
			continue
		}
		first, _, _ := strings.Cut(pkg, "/")
		if strings.Contains(first, ".") {
			t.Errorf("the config root package depends on %s, which is neither the standard library nor pkg/core", pkg)
		}
	}
}
