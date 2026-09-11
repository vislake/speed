package new

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/saasctl/internal/template"
)

// testSpeedRoot creates a fake speed checkout: a directory holding a
// go.work whose use entries name go/pkgcore, the marker ResolveSpeedRoot
// probes for. Its path is what a materialized go.mod's replace directives
// must point at, so every materialization test passes it back as the
// expected speed root.
func testSpeedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	content := "go 1.25.0\n\n// the fake checkout's workspace\nuse ./go/pkgcore\n\nuse ./go/dbkit\n"
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fake go.work: %v", err)
	}
	return root
}

// testRunArgs builds the argv for a materialization run: --speed-root is
// always explicit so no test depends on the working directory or the
// SPEED_ROOT environment, and --with carries the selection's module list --
// except for the full set, which is left to the flag's default (exercising
// the default path), and the empty selection, which needs an explicit
// --with= (the module list of an empty set is not expressible as a value).
func testRunArgs(root, target, key string) []string {
	args := []string{"--speed-root", root}
	switch key {
	case "authn+org+rbac":
		// the default --with value
	case "none":
		args = append(args, "--with=")
	default:
		args = append(args, "--with", strings.ReplaceAll(key, "+", ","))
	}
	return append(args, target)
}

// runNew invokes Run and returns its exit code, stdout and stderr.
func runNew(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// assetPath maps a materialized file's path inside the generated project
// to its embedded template path for a selection key. The go.mod document
// is embedded as go.mod.txt -- go:embed will not descend into a
// subdirectory holding a go.mod -- and renamed at materialization.
func assetPath(key, rel string) string {
	switch rel {
	case "go.mod":
		return "selection/" + key + "/go.mod.txt"
	case "cmd/server/server.go":
		return "selection/" + key + "/server.go"
	default:
		return rel
	}
}

// wantContent renders the expected bytes of one materialized file: the
// embedded asset, stripped of its build-ignore marker (for .go files) and
// token-substituted with the app name and speed root this run used. The
// materialization test asserts byte equality against this, which pins the
// materialized file to its embedded asset byte for byte -- and deliberately
// nothing more. In particular this is NOT a freshness check on the go.mod
// documents: both sides of the comparison are the same asset, so a
// go.mod.txt golden gone stale relative to what the speed modules now
// require (a dependency a module gained since the goldens were last
// tidied) passes this comparison unchanged. Nothing in the offline suite
// detects that staleness, and nothing else automatic does either --
// scaffold-verify's real tidy+build leg (one selection, on a schedule)
// would silently repair a stale require set rather than fail on it.
// Regenerating the five goldens through the real tidy procedure whenever
// a speed module's dependency set changes is the only check that exists.
//
// The substitution on THIS side is strings.NewReplacer applied over the
// same two tokens -- stdlib machinery with the same single-pass semantics
// replaceTokens implements by hand (a replacement value is written out and
// never re-scanned) -- and never a call to replaceTokens itself: an
// expectation rendered by the function under test could not detect a
// defect inside that function (the byte comparison would be constant-true
// against it), so the expected side carries its own substitution of the
// same two tokens. replaceTokens' own byte-exact behavior is pinned
// independently by the hand-pinned goldens in
// TestReplaceTokensSinglePassByteExactGoldens below, whose expected bytes
// are committed literals rather than that function's output.
func wantContent(t *testing.T, key, rel, appName, speedRoot string) []byte {
	t.Helper()
	path := template.ProjectRoot + "/" + assetPath(key, rel)
	content, err := fs.ReadFile(template.Project, path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	if strings.HasSuffix(rel, ".go") {
		stripped, err := template.StripBuildIgnore(content)
		if err != nil {
			t.Fatalf("strip %s: %v", path, err)
		}
		content = stripped
	}
	replacer := strings.NewReplacer(template.TokenAppName, appName, template.TokenSpeedRoot, speedRoot)
	return []byte(replacer.Replace(string(content)))
}

// TestRunMaterializesEverySelection drives the real command (Run, with
// stdout/stderr captured) for each of the five legal selections into a
// fresh temp target, then asserts the written tree is byte-identical to
// the embedded assets with the run's own tokens substituted -- the
// go.mod goldens included -- and that no template token survives
// anywhere. The five selections are enumerated as the canonical keys of
// template.SelectionKey (mirroring internal/template's embed_test.go
// side; the full universe cross-check is TestSelectionUniverseCrossCheck
// below).
//
// The expected tree is derived from template.SharedFiles (plus the two
// selection-driven files), the SAME single source materialize's own asset
// list is built from -- the twin-discipline pin that a shared template
// file added on either side without the other silently never (or wrongly)
// materializes: a file the run wrote that SharedFiles does not name, or a
// SharedFiles entry the run did not write, both fail here in the
// materialized-tree assertions below, in both directions.
func TestRunMaterializesEverySelection(t *testing.T) {
	t.Setenv(speedRootEnv, "") // never let the environment leak in
	root := testSpeedRoot(t)
	keys := []string{"authn+org+rbac", "authn+rbac", "authn+org", "authn", "none"}
	const appName = "probeapp"
	for _, key := range keys {
		key := key
		t.Run(key, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), appName)
			code, stdout, stderr := runNew(t, testRunArgs(root, target, key))
			if code != 0 {
				t.Fatalf("Run = %d, want 0; stderr:\n%s", code, stderr)
			}
			if stderr != "" {
				t.Errorf("Run wrote to stderr on success: %q", stderr)
			}

			// The two selection-driven files plus exactly the shared set:
			// the same derivation materialize itself uses, so the two
			// lists are equal by construction and a drift on either side
			// of the SharedFiles contract fails the tree assertions.
			files := []string{"go.mod", "cmd/server/server.go"}
			files = append(files, template.SharedFiles...)
			for _, rel := range files {
				want := wantContent(t, key, rel, appName, root)
				got, err := os.ReadFile(filepath.Join(target, rel))
				if err != nil {
					t.Errorf("read materialized %s: %v", rel, err)
					continue
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s is not byte-identical to the embedded asset with tokens substituted", rel)
				}
				if strings.Contains(string(got), template.TokenAppName) ||
					strings.Contains(string(got), template.TokenSpeedRoot) {
					t.Errorf("%s still carries a template token", rel)
				}
				if !strings.Contains(stdout, "Wrote "+filepath.Join(target, rel)) {
					t.Errorf("stdout does not report writing %s", rel)
				}
			}

			// The written tree holds exactly these six files -- no go.sum,
			// no stray output.
			got := map[string]bool{}
			err := filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					rel, err := filepath.Rel(target, path)
					if err != nil {
						return err
					}
					got[rel] = true
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk materialized tree: %v", err)
			}
			for _, rel := range files {
				if !got[rel] {
					t.Errorf("materialized tree is missing %s", rel)
				}
				delete(got, rel)
			}
			for extra := range got {
				t.Errorf("materialized tree holds unexpected file %s", extra)
			}

			// The go.mod module line carries the target's base name -- the
			// project's own module path.
			mod, err := os.ReadFile(filepath.Join(target, "go.mod"))
			if err != nil {
				t.Fatalf("read materialized go.mod: %v", err)
			}
			if !strings.HasPrefix(string(mod), "module "+appName+"\n") {
				t.Errorf("go.mod module line is not %q", "module "+appName)
			}
		})
	}
}

// TestReplaceTokensSinglePassByteExactGoldens pins replaceTokens' output
// byte for byte against hand-pinned expected documents -- committed
// literals, never bytes the function itself computed, so a defect inside
// replaceTokens cannot hide behind a self-computed expectation. The goldens
// cover both tokens, repeated and adjacent occurrences, token-free
// passthrough, and the single-pass property that replacement values are
// never re-scanned: an application name or speed-root path whose own text
// carries a token-shaped substring must survive byte-identical. The
// token-shaped-values case pins the single-pass property against the
// two-pass defect: a two-pass
// substitution (app-name pass, then a speed-root pass over the first
// pass's output) re-substitutes the app name's embedded speed-root text
// and corrupts the module line into a spliced path; the single-pass
// substitution this golden pins leaves the app name intact.
func TestReplaceTokensSinglePassByteExactGoldens(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		appName   string
		speedRoot string
		want      string
	}{
		{
			name: "both tokens, repeated occurrences in one document",
			content: "module __APP_NAME__\n\n" +
				"require github.com/vislake/speed/go/pkgcore v0.0.0\n\n" +
				"replace github.com/vislake/speed/go/pkgcore => __SPEED_ROOT__/go/pkgcore\n\n" +
				"replace github.com/vislake/speed/go/dbkit => __SPEED_ROOT__/go/dbkit\n",
			appName:   "smileapp",
			speedRoot: "/home/dev/speed",
			want: "module smileapp\n\n" +
				"require github.com/vislake/speed/go/pkgcore v0.0.0\n\n" +
				"replace github.com/vislake/speed/go/pkgcore => /home/dev/speed/go/pkgcore\n\n" +
				"replace github.com/vislake/speed/go/dbkit => /home/dev/speed/go/dbkit\n",
		},
		{
			name:      "token-free content passes through byte-identical",
			content:   "module example.com/plain\n\ngo 1.25.0\n",
			appName:   "smileapp",
			speedRoot: "/home/dev/speed",
			want:      "module example.com/plain\n\ngo 1.25.0\n",
		},
		{
			name:      "adjacent occurrences of both tokens",
			content:   "__APP_NAME____SPEED_ROOT__ x __SPEED_ROOT____APP_NAME__",
			appName:   "app",
			speedRoot: "/r",
			want:      "app/r x /rapp",
		},
		{
			name: "replacement values containing token-shaped text survive byte-identical",
			content: "module __APP_NAME__\n\n" +
				"replace github.com/vislake/speed/go/pkgcore => __SPEED_ROOT__/go/pkgcore\n",
			appName:   "smile__SPEED_ROOT__app",
			speedRoot: "/work/__APP_NAME__/speed",
			want: "module smile__SPEED_ROOT__app\n\n" +
				"replace github.com/vislake/speed/go/pkgcore => /work/__APP_NAME__/speed/go/pkgcore\n",
		},
		{
			name:      "app name containing the app-name token's own text",
			content:   "module __APP_NAME__\n",
			appName:   "probe__APP_NAME__x",
			speedRoot: "/r",
			want:      "module probe__APP_NAME__x\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(replaceTokens([]byte(c.content), c.appName, c.speedRoot)); got != c.want {
				t.Errorf("replaceTokens output = %q, want the hand-pinned bytes %q", got, c.want)
			}
		})
	}
}

// TestRunAppNameContainingTokenTextMaterializesByteIdentical drives the
// two-pass defect through the real command end to end: a target
// directory whose base name -- and therefore the app name every token
// occurrence is substituted with -- contains the speed-root token's own
// text. The materialized go.mod's module line and the generated server's
// error prefixes must carry that name byte-identical: a two-pass
// substitution re-substitutes the embedded speed-root text on its second
// pass and corrupts both. Deterministic: the assertions compare fixed
// literals, never anything derived from temp paths.
func TestRunAppNameContainingTokenTextMaterializesByteIdentical(t *testing.T) {
	t.Setenv(speedRootEnv, "")
	root := testSpeedRoot(t)
	const appName = "probe__SPEED_ROOT__app"
	target := filepath.Join(t.TempDir(), appName)
	code, _, stderr := runNew(t, testRunArgs(root, target, "authn+rbac"))
	if code != 0 {
		t.Fatalf("Run = %d, want 0; stderr:\n%s", code, stderr)
	}
	mod, err := os.ReadFile(filepath.Join(target, "go.mod"))
	if err != nil {
		t.Fatalf("read materialized go.mod: %v", err)
	}
	if want := "module " + appName + "\n"; !strings.HasPrefix(string(mod), want) {
		t.Errorf("go.mod module line = %q..., want %q: the app name must survive substitution byte-identical", mod, want)
	}
	server, err := os.ReadFile(filepath.Join(target, "cmd/server/server.go"))
	if err != nil {
		t.Fatalf("read materialized server.go: %v", err)
	}
	if !strings.Contains(string(server), `"`+appName+": build the authn module:") {
		t.Errorf("server.go's app-name error prefix was re-substituted; %q does not survive byte-identical", appName)
	}
}

// TestRunSpeedRootBreakingGoModGrammarIsRefusedBeforeAnythingCreated
// pins the P1 fix for a speed checkout whose PATH the go.mod grammar
// cannot carry: the replace directives embed the resolved speed-root path
// verbatim, and the go.mod grammar is not every filesystem path's grammar
// -- a space cuts the replace right-hand side (modfile then reads the
// remainder as a version), parentheses derail the directive's shape -- so
// materializing against such a checkout ships a go.mod no go tool accepts,
// while `saasctl new` had exited 0 over it. The produced go.mod document
// is therefore handed to the go command's own parser (modfile.Parse)
// before anything is created, and a document that does not parse is
// refused with an execution error naming the checkout path. The check
// sits on the ARTIFACT, so it covers every resolution tier: the flag tier
// (--speed-root) is exercised by the subtests' named roots, and the
// discovery tier by the third subtest, which runs from inside a spaced
// checkout with no flag and no SPEED_ROOT -- the tier whose resolved path
// must receive the same validation. Every subtest asserts the refusal
// lands before the target directory exists.
func TestRunSpeedRootBreakingGoModGrammarIsRefusedBeforeAnythingCreated(t *testing.T) {
	t.Setenv(speedRootEnv, "")
	// Roots whose paths are legal on the filesystem yet break the go.mod
	// grammar once substituted into a replace directive: a space (modfile
	// reads the post-space remainder as a version) and parentheses (the
	// directive's shape no longer parses).
	for _, breaking := range []string{"speed dev", "speed(dev)"} {
		breaking := breaking
		t.Run("flag tier: "+breaking, func(t *testing.T) {
			root := t.TempDir()
			checkout := filepath.Join(root, breaking)
			if err := os.MkdirAll(checkout, 0o755); err != nil {
				t.Fatalf("create the checkout: %v", err)
			}
			if err := os.WriteFile(filepath.Join(checkout, "go.work"),
				[]byte("go 1.25.0\n\nuse ./go/pkgcore\n"), 0o644); err != nil {
				t.Fatalf("write the checkout's go.work: %v", err)
			}
			target := filepath.Join(t.TempDir(), "probeapp")
			code, stdout, stderr := runNew(t, testRunArgs(checkout, target, "none"))
			if code != 1 {
				t.Fatalf("Run = %d, want 1 (a speed-root path breaking the produced go.mod must refuse the run); stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, checkout) {
				t.Errorf("stderr %q does not name the breaking checkout path %q as the cause", stderr, checkout)
			}
			if strings.Contains(stdout, "Wrote") {
				t.Error("refused run reported writing files")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Errorf("refused run created the target directory (stat err = %v); the refusal must land before anything is created", err)
			}
		})
	}
	t.Run("discovery tier: spaced checkout resolved from inside it", func(t *testing.T) {
		root := t.TempDir()
		checkout := filepath.Join(root, "speed dev")
		if err := os.MkdirAll(filepath.Join(checkout, "consumer", "apps"), 0o755); err != nil {
			t.Fatalf("create the checkout tree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(checkout, "go.work"),
			[]byte("go 1.25.0\n\nuse ./go/pkgcore\n"), 0o644); err != nil {
			t.Fatalf("write the checkout's go.work: %v", err)
		}
		// The auto-discovery shape: no --speed-root and no SPEED_ROOT, the
		// working directory somewhere inside the checkout -- the invocation
		// shape whose resolved path must receive the same validation.
		t.Chdir(filepath.Join(checkout, "consumer", "apps"))
		target := filepath.Join(t.TempDir(), "probeapp")
		code, stdout, stderr := runNew(t, []string{"--speed-root", "", target})
		if code != 1 {
			t.Fatalf("Run = %d, want 1 (the discovered checkout's path breaks the produced go.mod; stderr):\n%s", code, stderr)
		}
		if !strings.Contains(stderr, checkout) {
			t.Errorf("stderr %q does not name the discovered checkout path %q as the cause", stderr, checkout)
		}
		if strings.Contains(stdout, "Wrote") {
			t.Error("refused run reported writing files")
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("refused run created the target directory (stat err = %v)", err)
		}
	})
}

// TestRunEmptyTargetDirectoryAcceptedAndSecondRunRefused pins the
// empty-directory carve-out (a prepared mount point or checkout
// directory is a fine target) and the not-empty refusal on the second
// run: a rerun over an existing materialization fails with exit code 1
// and leaves the first materialization byte-for-byte untouched.
func TestRunEmptyTargetDirectoryAcceptedAndSecondRunRefused(t *testing.T) {
	t.Setenv(speedRootEnv, "")
	root := testSpeedRoot(t)
	target := filepath.Join(t.TempDir(), "apptwo")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("pre-create empty target: %v", err)
	}
	code, _, stderr := runNew(t, testRunArgs(root, target, "none"))
	if code != 0 {
		t.Fatalf("first Run over an empty directory = %d, want 0; stderr:\n%s", code, stderr)
	}
	serverBefore, err := os.ReadFile(filepath.Join(target, "cmd/server/server.go"))
	if err != nil {
		t.Fatalf("read materialized server.go: %v", err)
	}

	code, _, stderr = runNew(t, testRunArgs(root, target, "none"))
	if code != 1 {
		t.Fatalf("second Run = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not empty") {
		t.Errorf("stderr does not explain the refusal: %q", stderr)
	}
	serverAfter, err := os.ReadFile(filepath.Join(target, "cmd/server/server.go"))
	if err != nil {
		t.Fatalf("read server.go after refused rerun: %v", err)
	}
	if !bytes.Equal(serverBefore, serverAfter) {
		t.Error("refused rerun modified the existing materialization")
	}
}

// TestRunExistingNonEmptyTargetRefused asserts a pre-existing non-empty
// target is refused with exit code 1 before anything is written into it.
func TestRunExistingNonEmptyTargetRefused(t *testing.T) {
	t.Setenv(speedRootEnv, "")
	root := testSpeedRoot(t)
	target := filepath.Join(t.TempDir(), "occupied")
	marker := filepath.Join(target, "marker.txt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.WriteFile(marker, []byte("mine"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	code, stdout, stderr := runNew(t, testRunArgs(root, target, "none"))
	if code != 1 {
		t.Fatalf("Run = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not empty") {
		t.Errorf("stderr does not explain the refusal: %q", stderr)
	}
	if strings.Contains(stdout, "Wrote") {
		t.Error("refused run reported writing files")
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker file disappeared: %v", err)
	}
	if string(content) != "mine" {
		t.Error("refused run modified the existing tree")
	}
}

// TestRollbackTargetRemovesFilesAndDirectoriesItCreated pins the
// rollback promise of a failed materialization against the shape a
// mid-write failure actually leaves: materialize wrote some of its files
// and the directories cmd/ and cmd/server/ that hold them, then failed.
// rollbackTarget must restore the tree to its pre-run state -- files AND
// the directories created with them -- so the target's non-empty check
// cannot refuse the next attempt over an empty-dir half skeleton. The
// failure point itself is not constructible in a unit test (materialize
// only ever writes into an empty or absent target, and its write steps
// cannot be made to fail on demand without a fault-injection seam), so
// the test fabricates exactly the half-written tree and drives the
// rollback helper over the same written-file list materialize would have
// accumulated.
func TestRollbackTargetRemovesFilesAndDirectoriesItCreated(t *testing.T) {
	root := t.TempDir()
	// The pre-run state: an existing empty target (the accepted shape).
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("temp dir not empty: %v", err)
	}
	// The half-written state a mid-write failure leaves: some files
	// written, the directories materialize created with them.
	for _, rel := range []string{
		".gitignore", "README.md",
		"cmd/server/main.go", "cmd/server/config.go", "cmd/server/server.go", "go.mod",
	} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("fabricate %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o600); err != nil {
			t.Fatalf("fabricate %s: %v", rel, err)
		}
	}
	written := []string{".gitignore", "README.md", "cmd/server/main.go", "cmd/server/config.go", "go.mod", "cmd/server/server.go"}

	rollbackTarget(root, written)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read target after rollback: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("target holds %v after rollback; a failed run must leave no half skeleton -- files and the directories created with them both removed", names)
	}
	// The target itself survives as an empty directory: an empty target
	// is a legal, reusable state for the next attempt.
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Errorf("target itself was removed by the rollback (err=%v); only the run's own files and directories belong to the rollback", err)
	}
}

// TestRunInvalidTargetNameIsUsageError drives names that cannot serve as
// a module path through the whole command: each must exit 2 (a usage
// error), print the usage text, and never create the target directory.
// The reserved Windows device names (aux and its family) belong in the
// list because the module-path validator refuses them on every platform,
// not only on Windows (see validateModuleName's doc comment).
func TestRunInvalidTargetNameIsUsageError(t *testing.T) {
	t.Setenv(speedRootEnv, "")
	root := testSpeedRoot(t)
	badNames := []string{"..", ".", "", "my app", "app.", "-app", ".hidden", "aux", "CON", "com1"}
	for _, name := range badNames {
		name := name
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			target := name
			if name != "" && name != "." && name != ".." && !strings.HasPrefix(name, "/") {
				target = filepath.Join(t.TempDir(), name)
			}
			args := testRunArgs(root, target, "none")
			code, stdout, stderr := runNew(t, args)
			if code != 2 {
				t.Errorf("Run = %d, want 2; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, "module path") {
				t.Errorf("stderr does not name the module-path problem: %q", stderr)
			}
			if !strings.Contains(stderr, "Usage: saasctl new") {
				t.Error("usage error does not print the usage text")
			}
			if strings.Contains(stdout, "Wrote") {
				t.Error("usage error still reported writing files")
			}
		})
	}
}

// TestRunInvalidWithValuesAreUsageErrors drives invalid --with values
// through the whole command: unknown names are refused listing the valid
// set, selecting rbac or org without authn is refused naming authn as
// implied, and duplicates are refused. All exit 2.
func TestRunInvalidWithValuesAreUsageErrors(t *testing.T) {
	t.Setenv(speedRootEnv, "")
	root := testSpeedRoot(t)
	tests := []struct {
		name     string
		with     string
		wantText string
	}{
		{name: "unknown module", with: "jobs", wantText: "authn, rbac, org"},
		{name: "unknown next to valid", with: "authn,jobs", wantText: "jobs"},
		{name: "rbac without authn", with: "rbac", wantText: "authn"},
		{name: "org without authn", with: "org", wantText: "authn"},
		{name: "both dependents without authn", with: "rbac,org", wantText: "rbac and org"},
		{name: "duplicate", with: "authn,authn", wantText: "more than once"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "withprobe")
			args := []string{"--speed-root", root, "--with", tt.with, target}
			code, stdout, stderr := runNew(t, args)
			if code != 2 {
				t.Errorf("Run = %d, want 2; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tt.wantText) {
				t.Errorf("stderr %q does not contain %q", stderr, tt.wantText)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Error("usage error still created the target")
			}
			if strings.Contains(stdout, "Wrote") {
				t.Error("usage error still reported writing files")
			}
		})
	}
}

// TestParseWith pins the syntactic split: empty entries are skipped (a
// trailing comma, a bare --with=""), entries are trimmed, and duplicates
// are rejected. Unknown names and closure violations are
// validateSelection's job, so the two error classes stay separate.
func TestParseWith(t *testing.T) {
	tests := []struct {
		name    string
		with    string
		want    []string
		wantErr bool
	}{
		{name: "empty string", with: ""},
		{name: "only commas", with: ",,,"},
		{name: "single", with: "authn", want: []string{"authn"}},
		{name: "full set", with: "authn,rbac,org", want: []string{"authn", "rbac", "org"}},
		{name: "trailing comma", with: "authn,", want: []string{"authn"}},
		{name: "leading comma", with: ",authn", want: []string{"authn"}},
		{name: "spaces trimmed", with: "authn, rbac , org", want: []string{"authn", "rbac", "org"}},
		{name: "duplicate", with: "authn,authn", wantErr: true},
		{name: "duplicate after gap", with: "authn,rbac,authn", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWith(tt.with)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseWith(%q) = %v, want error", tt.with, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWith(%q): %v", tt.with, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseWith(%q) = %v, want %v", tt.with, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseWith(%q) = %v, want %v", tt.with, got, tt.want)
					break
				}
			}
		})
	}
}

// TestValidateSelection pins the downward-closure decision table over
// the switchable universe directly (Run-level behavior is covered by
// TestRunInvalidWithValuesAreUsageErrors): selecting rbac or org without
// authn is an error naming the selected dependents and authn as implied;
// unknown names are rejected listing the valid set; every legal subset of
// the universe passes.
func TestValidateSelection(t *testing.T) {
	valid := [][]string{
		{},
		{"authn"},
		{"authn", "rbac"},
		{"authn", "org"},
		{"authn", "rbac", "org"},
	}
	for _, mods := range valid {
		if err := validateSelection(mods); err != nil {
			t.Errorf("validateSelection(%v) = %v, want nil", mods, err)
		}
	}

	// Closure violations -- a dependent selected without authn -- are
	// refused naming the selected dependents and authn as implied; the
	// error never needs to list the whole universe, because the fix is
	// adding authn, not choosing among modules.
	closure := []struct {
		mods     []string
		wantText string
	}{
		{mods: []string{"rbac"}, wantText: "rbac"},
		{mods: []string{"org"}, wantText: "org"},
		{mods: []string{"rbac", "org"}, wantText: "rbac and org"},
	}
	for _, tt := range closure {
		err := validateSelection(tt.mods)
		if err == nil {
			t.Errorf("validateSelection(%v) = nil, want error", tt.mods)
			continue
		}
		if !strings.Contains(err.Error(), "authn") {
			t.Errorf("validateSelection(%v) error %q does not name authn as implied", tt.mods, err)
		}
		if !strings.Contains(err.Error(), tt.wantText) {
			t.Errorf("validateSelection(%v) error %q does not mention %q", tt.mods, err, tt.wantText)
		}
	}

	// Unknown names are refused listing the valid universe, so the error
	// both names the offender and shows what would have been legal.
	unknown := []struct {
		mods     []string
		wantText string
	}{
		{mods: []string{"jobs"}, wantText: "jobs"},
		{mods: []string{"authn", "metering"}, wantText: "metering"},
	}
	for _, tt := range unknown {
		err := validateSelection(tt.mods)
		if err == nil {
			t.Errorf("validateSelection(%v) = nil, want error", tt.mods)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantText) {
			t.Errorf("validateSelection(%v) error %q does not mention %q", tt.mods, err, tt.wantText)
		}
		if !strings.Contains(err.Error(), "valid: authn, rbac, org") {
			t.Errorf("validateSelection(%v) error %q does not list the valid universe", tt.mods, err)
		}
	}
}

// TestValidateModuleNameAndDeriveModuleName pins the name grammar against
// real go tooling behavior: every accepted name is one `go mod init`
// accepts (the x/mod gate inside validateModuleName makes the claim true
// by construction -- module.CheckImportPath is the validator the go
// command itself runs), and the rejections -- leading dot, leading dash,
// space, trailing dot, "." and "..", and the reserved Windows device
// names (aux, CON, com1, ...), which x/mod refuses on EVERY platform --
// keep a generated project's module path looking like a name. The
// windows-name half of the list is what the plain grammar could never
// see: "aux" matches the pattern yet is not a module path any go tool
// accepts -- a `saasctl new aux` run would otherwise exit 0 with a fully
// written, unbuildable project; the command-level refusal is
// TestRunInvalidTargetNameIsUsageError. deriveModuleName takes the base
// name from the lexically cleaned target, so a trailing ".." in the
// target can never silently redirect the materialization into a parent
// directory.
func TestValidateModuleNameAndDeriveModuleName(t *testing.T) {
	accepted := []string{"myapp", "1app", "a..b", "app-name", "a_b", "App1"}
	for _, name := range accepted {
		if err := validateModuleName(name); err != nil {
			t.Errorf("validateModuleName(%q) = %v, want nil (go mod init accepts it)", name, err)
		}
	}
	rejected := []string{
		"", ".", "..", "my app", "app.", "-app", ".hidden", "/",
		"aux", "AUX", "con", "CON", "nul", "NUL", "prn", "PRN",
		"com1", "COM9", "lpt1", "LPT9",
	}
	for _, name := range rejected {
		if err := validateModuleName(name); err == nil {
			t.Errorf("validateModuleName(%q) = nil, want error", name)
		}
	}

	derive := []struct {
		target string
		want   string
	}{
		{target: "/x/y/myapp", want: "myapp"},
		{target: "myapp", want: "myapp"},
		{target: "myapp/", want: "myapp"},
		{target: "some/dir/deep/app-name", want: "app-name"},
		{target: "/x/y/..", want: "x"}, // lexical clean collapses y/.. before the base name is read
	}
	for _, tt := range derive {
		got, err := deriveModuleName(tt.target)
		if err != nil {
			t.Errorf("deriveModuleName(%q): %v", tt.target, err)
			continue
		}
		if got != tt.want {
			t.Errorf("deriveModuleName(%q) = %q, want %q", tt.target, got, tt.want)
		}
	}
	if _, err := deriveModuleName(".."); err == nil {
		t.Error("deriveModuleName(\"..\") = nil error, want refusal of the parent directory")
	}
	if _, err := deriveModuleName("."); err == nil {
		t.Error("deriveModuleName(\".\") = nil error, want refusal")
	}
}

// TestResolveSpeedRoot pins the precedence ladder -- flag, then SPEED_ROOT
// environment, then the ancestor go.work probe -- and the validation each
// tier applies: the resolved root must hold a go.work whose use entries
// list go/pkgcore, or resolution fails naming the tier that produced the
// bad path. Working-directory probes use t.Chdir, so these tests must not
// run in parallel with anything that assumes a working directory.
func TestResolveSpeedRoot(t *testing.T) {
	root := testSpeedRoot(t)
	plainDir := t.TempDir()
	noPkgcoreRoot := t.TempDir()
	noPkgcoreContent := "go 1.25.0\n\nuse ./go/other\n"
	if err := os.WriteFile(filepath.Join(noPkgcoreRoot, "go.work"), []byte(noPkgcoreContent), 0o644); err != nil {
		t.Fatalf("write go.work without pkgcore: %v", err)
	}

	t.Run("flag tier accepts a real root", func(t *testing.T) {
		got, err := ResolveSpeedRoot(root)
		if err != nil {
			t.Fatalf("ResolveSpeedRoot: %v", err)
		}
		if want := mustAbs(t, root); got != want {
			t.Errorf("ResolveSpeedRoot = %q, want %q", got, want)
		}
	})
	t.Run("flag tier refuses a missing root", func(t *testing.T) {
		_, err := ResolveSpeedRoot(filepath.Join(plainDir, "nope"))
		if err == nil || !strings.Contains(err.Error(), "--speed-root") {
			t.Errorf("error = %v, want it to name --speed-root", err)
		}
	})
	t.Run("flag tier refuses a root without go.work", func(t *testing.T) {
		_, err := ResolveSpeedRoot(plainDir)
		if err == nil || !strings.Contains(err.Error(), "go.work") {
			t.Errorf("error = %v, want it to mention go.work", err)
		}
	})
	t.Run("flag tier refuses a go.work without pkgcore", func(t *testing.T) {
		_, err := ResolveSpeedRoot(noPkgcoreRoot)
		if err == nil || !strings.Contains(err.Error(), "go/pkgcore") {
			t.Errorf("error = %v, want it to mention go/pkgcore", err)
		}
	})

	t.Run("env tier resolves when flag is absent", func(t *testing.T) {
		t.Setenv(speedRootEnv, root)
		got, err := ResolveSpeedRoot("")
		if err != nil {
			t.Fatalf("ResolveSpeedRoot: %v", err)
		}
		if want := mustAbs(t, root); got != want {
			t.Errorf("ResolveSpeedRoot = %q, want %q", got, want)
		}
	})
	t.Run("flag outranks env", func(t *testing.T) {
		t.Setenv(speedRootEnv, noPkgcoreRoot)
		got, err := ResolveSpeedRoot(root)
		if err != nil {
			t.Fatalf("ResolveSpeedRoot: %v", err)
		}
		if want := mustAbs(t, root); got != want {
			t.Errorf("ResolveSpeedRoot = %q, want %q (flag must outrank SPEED_ROOT)", got, want)
		}
	})
	t.Run("env tier errors name SPEED_ROOT", func(t *testing.T) {
		t.Setenv(speedRootEnv, noPkgcoreRoot)
		_, err := ResolveSpeedRoot("")
		if err == nil || !strings.Contains(err.Error(), speedRootEnv) {
			t.Errorf("error = %v, want it to name %s", err, speedRootEnv)
		}
	})

	t.Run("probe tier finds an ancestor go.work", func(t *testing.T) {
		t.Setenv(speedRootEnv, "")
		deep := filepath.Join(root, "consumer", "apps", "one")
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatalf("create nested dirs: %v", err)
		}
		t.Chdir(deep)
		got, err := ResolveSpeedRoot("")
		if err != nil {
			t.Fatalf("ResolveSpeedRoot: %v", err)
		}
		if want := mustAbs(t, root); got != want {
			t.Errorf("ResolveSpeedRoot = %q, want %q", got, want)
		}
	})
	t.Run("probe tier errors when nothing matches", func(t *testing.T) {
		t.Setenv(speedRootEnv, "")
		nowhere := filepath.Join(t.TempDir(), "consumer")
		if err := os.MkdirAll(nowhere, 0o755); err != nil {
			t.Fatalf("create dir: %v", err)
		}
		t.Chdir(nowhere)
		_, err := ResolveSpeedRoot("")
		if err == nil {
			t.Fatal("ResolveSpeedRoot = nil error, want failure naming the resolution sources")
		}
		for _, source := range []string{"--speed-root", speedRootEnv} {
			if !strings.Contains(err.Error(), source) {
				t.Errorf("error %q does not name %s as a remedy", err, source)
			}
		}
	})
}

// TestGoWorkUsesPkgcore pins the probe's comment-awareness: use lines
// count, comments and blank lines do not.
func TestGoWorkUsesPkgcore(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "plain use line", content: "use ./go/pkgcore\n", want: true},
		{name: "pkgcore in a comment only", content: "// use ./go/pkgcore\nuse ./go/other\n", want: false},
		{name: "blank and comments only", content: "// a comment\n\n// another\n", want: false},
		{name: "empty", content: "", want: false},
		{name: "use line in a block", content: "go 1.25.0\n\nuse (\n\t./go/pkgcore\n\t./go/dbkit\n)\n", want: true},
		{name: "unrelated module", content: "use ./go/observability\n", want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := goWorkUsesPkgcore([]byte(tt.content)); got != tt.want {
				t.Errorf("goWorkUsesPkgcore(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// TestSelectionUniverseCrossCheck is new's side of the cross-check with
// internal/template: it enumerates the whole selection universe (every
// subset of the switchable modules), asserts validateSelection's verdict
// for each -- legal exactly when the subset contains authn or is empty --
// and asserts the embed tree carries exactly the five selection
// directories the legal subsets render to. A module added to
// switchableModules without an embedded selection directory fails here;
// an embedded directory no selection renders to fails on template's own
// side. new cannot import template's tests and template cannot import
// new, so each side asserts its half against its own enumeration of the
// shared ground truth.
func TestSelectionUniverseCrossCheck(t *testing.T) {
	// Every subset of switchableModules, as a bitmask over
	// {authn, rbac, org}.
	var validKeys []string
	invalid := 0
	for mask := 0; mask < 1<<len(switchableModules); mask++ {
		var subset []string
		hasAuthn := false
		for i, name := range switchableModules {
			if mask&(1<<i) != 0 {
				subset = append(subset, name)
				if name == "authn" {
					hasAuthn = true
				}
			}
		}
		key := template.SelectionKey(subset)
		if hasAuthn || len(subset) == 0 {
			validKeys = append(validKeys, key)
			if err := validateSelection(subset); err != nil {
				t.Errorf("validateSelection(%v) = %v, want nil", subset, err)
			}
			// The legal subset's directory must exist in the embed tree,
			// or materializing it would fail.
			if _, err := fs.Stat(template.Project, template.ProjectRoot+"/selection/"+key); err != nil {
				t.Errorf("valid selection %s has no embedded directory: %v", key, err)
			}
		} else {
			invalid++
			if err := validateSelection(subset); err == nil {
				t.Errorf("validateSelection(%v) = nil, want error naming authn", subset)
			} else if !strings.Contains(err.Error(), "authn") {
				t.Errorf("validateSelection(%v) error %q does not name authn", subset, err)
			}
		}
	}
	if len(validKeys) != 5 {
		t.Errorf("expected exactly five legal selections, got %v", validKeys)
	}
	if invalid != 3 {
		t.Errorf("expected exactly three closure-invalid subsets, got %d", invalid)
	}

	// And no selection directory exists that no legal subset renders to.
	entries, err := fs.ReadDir(template.Project, template.ProjectRoot+"/selection")
	if err != nil {
		t.Fatalf("read embedded selection directory: %v", err)
	}
	legal := map[string]bool{}
	for _, key := range validKeys {
		legal[key] = true
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !legal[e.Name()] {
			t.Errorf("embedded selection directory %s is not reachable from any legal --with set", e.Name())
		}
	}
}

// mustAbs absolutizes a path in tests.
func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q): %v", path, err)
	}
	return abs
}
