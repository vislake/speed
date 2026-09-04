package template

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

// validSelectionKeys is the one enumeration of the five legal selection
// keys, mirrored here and in internal/new's validation (new cannot import
// this package's tests and template cannot import new -- the template
// package is new's dependency, not the other way around). The two sides
// cross-check each other through their shared ground truth: a selection
// key new accepts must exist in the embed tree or new's materialization
// fails, and a directory in the embed tree new does not accept is dead
// weight that TestSelectionDirectoriesMatchValidKeys below refuses. The
// selection-key grammar itself is SelectionKey's: sorted modules joined by
// '+', or "none" for the empty set.
var validSelectionKeys = []string{"authn+org+rbac", "authn+rbac", "authn+org", "authn", "none"}

// TestEmbeddedGoFilesStartWithBuildIgnoreLine walks the whole embedded
// project tree and asserts that every .go file starts with BuildIgnoreLine
// -- the compile-containment convention (A2): the skeleton must never
// compile, vet or lint as part of this module. A template edit that adds a
// .go file without the marker fails here, and independently in new's own
// generator guard (internal/new), so the convention is enforced in two
// places as its doc comment promises.
func TestEmbeddedGoFilesStartWithBuildIgnoreLine(t *testing.T) {
	goFiles := 0
	err := fs.WalkDir(Project, ProjectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		goFiles++
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(content), BuildIgnoreLine+"\n") {
			t.Errorf("%s: first line is not the build-ignore marker %q", path, BuildIgnoreLine)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded project tree: %v", err)
	}
	// The shared pair plus one server.go per selection: a drift in either
	// direction (a new template file, a removed one) is a real convention
	// change and this count is the tripwire.
	wantGoFiles := 2 + len(validSelectionKeys)
	if goFiles != wantGoFiles {
		t.Errorf("embedded tree holds %d .go files, want %d", goFiles, wantGoFiles)
	}
}

// TestSelectionDirectoriesMatchValidKeys asserts the embed tree holds
// exactly the five legal selection directories -- no more, no fewer, none
// of them files -- and that each holds exactly the two files a selection
// drives: its go.mod.txt (the go.mod document -- the require set, with the
// seeds pruned by the development-proof tidy run -- stored under the inert
// .txt name because go:embed refuses to descend into a subdirectory
// containing a go.mod, and renamed by new at materialization) and its
// server.go (the import, module and middleware set).
func TestSelectionDirectoriesMatchValidKeys(t *testing.T) {
	entries, err := fs.ReadDir(Project, ProjectRoot+"/selection")
	if err != nil {
		t.Fatalf("read selection directory: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			got[e.Name()] = true
			inner, err := fs.ReadDir(Project, ProjectRoot+"/selection/"+e.Name())
			if err != nil {
				t.Fatalf("read selection %s: %v", e.Name(), err)
			}
			want := map[string]bool{"go.mod.txt": true, "server.go": true}
			for _, f := range inner {
				if f.IsDir() {
					t.Errorf("selection %s holds a nested directory %s, want only go.mod.txt and server.go", e.Name(), f.Name())
					continue
				}
				if !want[f.Name()] {
					t.Errorf("selection %s holds unexpected file %s, want only go.mod.txt and server.go", e.Name(), f.Name())
				}
			}
		} else {
			t.Errorf("project/selection holds a file %s, want only selection directories", e.Name())
		}
	}
	for _, key := range validSelectionKeys {
		if !got[key] {
			t.Errorf("missing selection directory for valid key %s", key)
		}
	}
	for key := range got {
		found := false
		for _, valid := range validSelectionKeys {
			if key == valid {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("selection directory %s is not one of the valid keys %v", key, validSelectionKeys)
		}
	}
}

// TestSharedFilesPresent asserts every file SharedFiles names exists in
// the embed tree -- the shared set materializes verbatim in every
// selection, so a missing entry would fail every `saasctl new` run.
func TestSharedFilesPresent(t *testing.T) {
	for _, name := range SharedFiles {
		content, err := fs.ReadFile(Project, ProjectRoot+"/"+name)
		if err != nil {
			t.Errorf("shared file %s: %v", name, err)
			continue
		}
		if strings.HasSuffix(name, ".go") {
			if !strings.HasPrefix(string(content), BuildIgnoreLine+"\n") {
				t.Errorf("shared .go file %s does not start with the build-ignore marker", name)
			}
		}
	}
}

// TestProjectReadmeNamesTheShippedMaintenanceCommands asserts the generated
// project's README -- a shared file, materialized verbatim into every
// selection -- documents the maintenance commands that are wired today
// (`saasctl upgrade`, `saasctl db migrate`, `saasctl config print`) and
// presents nothing that shipped only later as available now. The README is
// the generated project's first documentation, so a lifecycle claim that
// drifted from the CLI's real surface would mislead every consumer project
// that reads it.
func TestProjectReadmeNamesTheShippedMaintenanceCommands(t *testing.T) {
	content, err := fs.ReadFile(Project, ProjectRoot+"/README.md")
	if err != nil {
		t.Fatalf("read project README: %v", err)
	}
	readme := string(content)
	const section = "## Editing and regenerating"
	start := strings.Index(readme, section)
	if start < 0 {
		t.Fatalf("README has no %q section", section)
	}
	body := readme[start:]
	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}
	// Fold the prose's line wrapping: a command name split across two
	// wrapped lines must still count as present.
	body = strings.Join(strings.Fields(body), " ")
	for _, cmd := range []string{"saasctl upgrade", "saasctl db migrate", "saasctl config print"} {
		if !strings.Contains(body, cmd) {
			t.Errorf("%s section does not name the shipped command %q", section, cmd)
		}
	}
	// The stale wording this pins against presented upgrade, db and config
	// as "later rounds" and claimed config inspects its dynamic
	// configuration; config print renders the bootstrap environment only,
	// and dynamic-configuration print is genuinely still later work.
	for _, stale := range []string{"inspects its dynamic configuration", "`saasctl db` runs", "`saasctl config` inspects"} {
		if strings.Contains(body, stale) {
			t.Errorf("%s section still carries the stale claim %q", section, stale)
		}
	}
}

// TestSelectionGoModsCarryTokens asserts each selection's go.mod document
// (the go.mod.txt asset, renamed by new when it materializes) is a
// token-carrying template, never a materialized artifact: the module line
// still names TokenAppName, every replace directive still points at
// TokenSpeedRoot (a relative path -- an absolute leaked path would mean a
// development-proof golden was committed without its tokens converted
// back), and no replace ever points outside the speed module graph.
func TestSelectionGoModsCarryTokens(t *testing.T) {
	for _, key := range validSelectionKeys {
		path := ProjectRoot + "/selection/" + key + "/go.mod.txt"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		mod := string(content)
		if !strings.HasPrefix(mod, "module "+TokenAppName+"\n") {
			t.Errorf("%s: module line does not name the app token, first line %q", path, firstLine(mod))
		}
		if !strings.Contains(mod, TokenSpeedRoot) {
			t.Errorf("%s: no replace points at the speed-root token", path)
		}
		if strings.Contains(mod, "=> /") {
			t.Errorf("%s: a replace directive carries an absolute path (tokens not converted back?)", path)
		}
		for _, line := range strings.Split(mod, "\n") {
			if !strings.HasPrefix(line, "replace ") {
				continue
			}
			if !strings.Contains(line, "=> "+TokenSpeedRoot+"/go/") {
				t.Errorf("%s: replace directive %q does not point at the speed module graph", path, line)
			}
		}
	}
}

// TestSelectionServerGoMatchesSelectionKey cross-checks each selection's
// server.go against its own key: the module constructors it calls, the
// middleware pieces it composes and the Bootstrap argument order are the
// selection's whole meaning (A5), so a template edit that lets the file
// and its directory drift apart fails here even though the file itself --
// build-ignored -- would never fail a compile.
func TestSelectionServerGoMatchesSelectionKey(t *testing.T) {
	expected := []struct {
		key              string
		modules          []string // in Bootstrap argument order
		contains, absent []string
	}{
		{
			key:      "authn+org+rbac",
			modules:  []string{"pkiModule", "authnModule", "orgModule", "configModule", "rbacModule"},
			contains: []string{"authn.NewModule(", "org.NewModule(", "rbac.NewModule(", "pki.NewModule(", "authnPreAuthAllowlist()", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
		},
		{
			key:      "authn+rbac",
			modules:  []string{"pkiModule", "authnModule", "configModule", "rbacModule"},
			contains: []string{"authn.NewModule(", "rbac.NewModule(", "pki.NewModule(", "authnPreAuthAllowlist()", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
			absent:   []string{"org.NewModule("},
		},
		{
			key:      "authn+org",
			modules:  []string{"pkiModule", "authnModule", "orgModule", "configModule"},
			contains: []string{"authn.NewModule(", "org.NewModule(", "pki.NewModule(", "authnPreAuthAllowlist()", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
			absent:   []string{"rbac.NewModule("},
		},
		{
			key:      "authn",
			modules:  []string{"pkiModule", "authnModule", "configModule"},
			contains: []string{"authn.NewModule(", "pki.NewModule(", "authnPreAuthAllowlist()", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
			absent:   []string{"org.NewModule(", "rbac.NewModule("},
		},
		{
			key:      "none",
			modules:  []string{"configModule"},
			contains: []string{"config.NewModule("},
			absent: []string{
				"authn.NewModule(", "org.NewModule(", "rbac.NewModule(",
				"authn.Middleware(", "tenancy.Middleware(", "authn.NewPrincipalResolver()",
				"authnAPIPath", "authnPreAuthAllowlist", "RegisterPIISerializer", "devSigningKeySeed",
			},
		},
	}
	for _, want := range expected {
		path := ProjectRoot + "/selection/" + want.key + "/server.go"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		server := string(content)
		// Bootstrap's module argument list is the composition's skeleton;
		// assert the exact order as one line so a reordering that changes
		// migration-vs-registration semantics cannot pass silently.
		bootstrapLine := "Bootstrap(ctx, " + strings.Join(want.modules, ", ") + ")"
		if !strings.Contains(server, bootstrapLine) {
			t.Errorf("%s: missing Bootstrap call %q", path, bootstrapLine)
		}
		migrationList := "[]pkgcore.Module{" + strings.Join(want.modules, ", ") + "}"
		if !strings.Contains(server, migrationList) {
			t.Errorf("%s: missing migration registration list %q", path, migrationList)
		}
		for _, s := range want.contains {
			if !strings.Contains(server, s) {
				t.Errorf("%s: selection %s should contain %q", path, want.key, s)
			}
		}
		for _, s := range want.absent {
			if strings.Contains(server, s) {
				t.Errorf("%s: selection %s must not contain %q", path, want.key, s)
			}
		}
	}
}

// TestSelectionKey pins SelectionKey's rendering: the canonical sorted
// '+'-joined form of a validated --with set, "none" for the empty set, and
// a caller's input slice left untouched (the caller may reuse it).
func TestSelectionKey(t *testing.T) {
	tests := []struct {
		name string
		with []string
		want string
	}{
		{name: "empty", with: []string{}, want: "none"},
		{name: "already sorted", with: []string{"authn", "rbac", "org"}, want: "authn+org+rbac"},
		{name: "reverse sorted input", with: []string{"org", "rbac", "authn"}, want: "authn+org+rbac"},
		{name: "pair", with: []string{"rbac", "authn"}, want: "authn+rbac"},
		{name: "single", with: []string{"org"}, want: "org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append([]string(nil), tt.with...)
			if got := SelectionKey(tt.with); got != tt.want {
				t.Errorf("SelectionKey(%v) = %q, want %q", tt.with, got, tt.want)
			}
			for i := range input {
				if input[i] != tt.with[i] {
					t.Errorf("SelectionKey mutated its input: element %d changed from %q to %q", i, tt.with[i], input[i])
					break
				}
			}
		})
	}
}

// TestStripBuildIgnoreRoundTrip asserts the template .go layout Strip
// depends on -- marker line, one blank line, then content -- and that
// stripping is byte-exact against that layout: Strip must reproduce the
// template-minus-marker document, which is what new's materialized
// project is compared against, so the two cannot drift. A template edit
// that changes the mandated layout (double blank line, no blank line, a
// marker that moved) fails here first.
func TestStripBuildIgnoreRoundTrip(t *testing.T) {
	prefix := []byte(BuildIgnoreLine + "\n\n")
	err := fs.WalkDir(Project, ProjectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(content, prefix) {
			t.Errorf("%s: does not open with the marker and one blank line", path)
			return nil
		}
		stripped, err := StripBuildIgnore(content)
		if err != nil {
			t.Errorf("%s: StripBuildIgnore: %v", path, err)
			return nil
		}
		if want := bytes.TrimPrefix(content, prefix); !bytes.Equal(stripped, want) {
			t.Errorf("%s: StripBuildIgnore is not byte-identical to template-minus-marker", path)
		}
		if bytes.HasPrefix(stripped, []byte("\n")) {
			t.Errorf("%s: stripped file still starts with a blank line", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded project tree: %v", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
