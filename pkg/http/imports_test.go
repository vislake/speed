package http

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestRootPackageImportClosureHasNoSubpackage pins what a program that imports
// this package alone links: the root package and nothing else from this
// module.
//
// This is the half of "the root package registers nothing" that no registry
// read can see coming. Registration lives in an implementation subpackage,
// whose init runs in every program that links it, so a transitive import of
// one from here would hand a host that asked for no entry point an entry point
// it cannot see.
func TestRootPackageImportClosureHasNoSubpackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	const root = "github.com/vislake/speed/pkg/http"
	var subpackages []string
	seenRoot := false
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		switch {
		case pkg == root:
			seenRoot = true
		case strings.HasPrefix(pkg, root+"/"):
			subpackages = append(subpackages, pkg)
		}
	}
	if !seenRoot {
		t.Fatalf("go list -deps did not list %s, so the scan read nothing", root)
	}
	if len(subpackages) > 0 {
		t.Errorf("a program importing only this package also links %v, whose init registers an entry "+
			"point there; a host gets one by importing the implementation subpackage it wants",
			subpackages)
	}
}

// registryReport is what testdata/rootimport reports about the process it runs
// in.
type registryReport struct {
	Modules []string `json:"modules"`
	Routers []string `json:"routers"`
}

// TestAProgramImportingOnlyTheRootPackageRegistersNothing starts a program
// whose only import from this module is the root package, and reads the
// registry that program's own process holds.
//
// The test binary's registry cannot answer this: importing pkg/http/stdmux in
// any test file of this package — which is how a host imports an entry point,
// and what the examples here do — registers that module in the binary, and a
// criterion reading the process registry there cannot tell that registration
// apart from one this package performed itself. The program puts the
// observation back on the host the statement is about.
func TestAProgramImportingOnlyTheRootPackageRegistersNothing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run := exec.Command("go", "run", "./testdata/rootimport")
	run.Stdout = &stdout
	run.Stderr = &stderr
	if err := run.Run(); err != nil {
		t.Fatalf("running the program whose only import from this module is the root package: %v\n%s",
			err, stderr.String())
	}

	var report registryReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("the program reported %q, which is not the JSON this criterion reads: %v",
			stdout.String(), err)
	}
	if len(report.Modules) == 0 {
		t.Fatalf("the program read an empty registry (%q), so its report is no observation of what "+
			"importing this package registers", stdout.String())
	}
	if len(report.Routers) > 0 {
		t.Errorf("the program's registry holds %v, and those deliver Router: a program whose only "+
			"import from this module is the root package got an entry point. The registry it read "+
			"was %v", report.Routers, report.Modules)
	}
	// Every module an implementation subpackage registers carries this release
	// unit's name: "http" is the unit and "http.stdmux" is one of its
	// subpackages, the release unit's name plus the subpackage's. A program
	// that imported no subpackage holds no module named either way.
	for _, name := range report.Modules {
		if name == "http" || strings.HasPrefix(name, "http.") {
			t.Errorf("the program's registry holds a module named %q, which belongs to this release "+
				"unit and which importing this package alone cannot have registered. The registry it "+
				"read was %v", name, report.Modules)
		}
	}
}
