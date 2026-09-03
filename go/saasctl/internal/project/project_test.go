package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMod writes content to a go.mod inside a fresh temp directory and
// returns its path.
func writeMod(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write go.mod fixture: %v", err)
	}
	return path
}

const validMod = `module example.com/smile/cli-app

go 1.25.0

require (
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/dbkit v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	golang.org/x/mod v0.27.0 // indirect
)

require github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000

replace github.com/vislake/speed/go/config => /some/checkout/go/config
`

// TestReadReturnsModulePathAppNameAndSpeedRequires: a real go.mod -- block
// and single-line requires, third-party requires and a replace directive
// included -- reads back as its module path, the path's final element as
// the app name, and the speed requires in go.mod order.
func TestReadReturnsModulePathAppNameAndSpeedRequires(t *testing.T) {
	ctx, err := Read(writeMod(t, validMod))
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if ctx.ModPath != "example.com/smile/cli-app" {
		t.Errorf("ModPath = %q, want the go.mod's module path", ctx.ModPath)
	}
	if ctx.AppName != "cli-app" {
		t.Errorf("AppName = %q, want the module path's final element", ctx.AppName)
	}
	want := []string{
		"github.com/vislake/speed/go/pkgcore",
		"github.com/vislake/speed/go/dbkit",
		"github.com/vislake/speed/go/authn",
		"github.com/vislake/speed/go/config",
	}
	if len(ctx.Requires) != len(want) {
		t.Fatalf("Requires = %v, want %v", ctx.Requires, want)
	}
	for i := range want {
		if ctx.Requires[i] != want[i] {
			t.Errorf("Requires[%d] = %q, want %q (in go.mod order)", i, ctx.Requires[i], want[i])
		}
	}
}

// TestReadAppNameIsTheModulePathFinalElement: the app name is derived from
// the module path, whatever its depth, matching the name saasctl new
// derived from the target directory at materialization.
func TestReadAppNameIsTheModulePathFinalElement(t *testing.T) {
	for _, tc := range []struct {
		module string
		want   string
	}{
		{module: "cli-app", want: "cli-app"},
		{module: "example.com/smile/cli-app", want: "cli-app"},
		{module: "github.com/vislake/demo/smile/cli-app", want: "cli-app"},
	} {
		ctx, err := Read(writeMod(t, "module "+tc.module+"\n\ngo 1.25.0\n"))
		if err != nil {
			t.Fatalf("Read(%s) failed: %v", tc.module, err)
		}
		if ctx.AppName != tc.want {
			t.Errorf("AppName for module %q = %q, want %q", tc.module, ctx.AppName, tc.want)
		}
	}
}

// TestReadThirdPartyOnlyGoModHasNoSpeedRequires: a go.mod that requires no
// speed module at all parses fine and reports an empty Requires slice --
// the state db migrate refuses with its own distinct message.
func TestReadThirdPartyOnlyGoModHasNoSpeedRequires(t *testing.T) {
	ctx, err := Read(writeMod(t, "module cli-app\n\ngo 1.25.0\n\nrequire golang.org/x/mod v0.27.0\n"))
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if ctx.ModPath != "cli-app" || ctx.AppName != "cli-app" {
		t.Errorf("ModPath/AppName = %q/%q, want cli-app/cli-app", ctx.ModPath, ctx.AppName)
	}
	if len(ctx.Requires) != 0 {
		t.Errorf("Requires = %v, want none", ctx.Requires)
	}
}

// TestReadMissingFileNamesThePath: a go.mod argument that does not exist
// fails with the path in the message, prefixed like the sibling commands'
// file errors.
func TestReadMissingFileNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-go.mod")
	_, err := Read(path)
	if err == nil {
		t.Fatal("Read succeeded on a missing file")
	}
	if !strings.HasPrefix(err.Error(), "read "+path+":") {
		t.Errorf("error = %q, want the read %s: prefix", err, path)
	}
}

// TestReadMalformedGoModFailsToParse: go.mod data the Go toolchain itself
// would refuse is refused with the parse-go.mod prefix, so a consumer
// pointed at the wrong file learns it immediately.
func TestReadMalformedGoModFailsToParse(t *testing.T) {
	_, err := Read(writeMod(t, "module cli-app\n\ngo 1.25.0\n\nrequire ( github.com/vislake/speed/go/config\n"))
	if err == nil {
		t.Fatal("Read accepted a malformed go.mod")
	}
	if !strings.HasPrefix(err.Error(), "parse go.mod:") {
		t.Errorf("error = %q, want the parse go.mod: prefix", err)
	}
}

// TestReadModulelessGoModIsRefused: a file with no module line declares no
// project, and Read refuses it rather than inventing a name.
func TestReadModulelessGoModIsRefused(t *testing.T) {
	_, err := Read(writeMod(t, "go 1.25.0\n"))
	if err == nil {
		t.Fatal("Read accepted a go.mod without a module line")
	}
	if !strings.Contains(err.Error(), "module") {
		t.Errorf("error = %q, want it to name the missing module path", err)
	}
}
