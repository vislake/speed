// Package new implements the `saasctl new` command: materializing the
// embedded project skeleton (internal/template) into a new consumer
// project directory. A generated project is a real, working app by
// construction -- the template files themselves were produced by
// materializing, tidying and building them against a real speed checkout
// (internal/template's package doc comment carries the details) -- and
// `saasctl new` is the first half of the product's consumer story: the
// CLI shapes a project, the project's own `go mod tidy` + `go build` (and
// a later `saasctl upgrade`) maintain it.
//
// The --with flag positively selects the business modules the generated
// project wires (default: authn, rbac and org, the minimal combination).
// Only these three are switchable in this build; the required five
// (pkgcore, dbkit, tenancy, config, observability) have no off option.
// Selection is validated with downward closure: rbac and org each need an
// authenticating layer, so selecting either without authn is an error
// naming authn, and unknown names are rejected listing the valid set.
// There is no --without: closing authn is expressed by not listing it,
// and dependents cannot be selected without it.
package new

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/vislake/speed/go/saasctl/internal/template"
)

const usage = `Usage: saasctl new [flags] <target-directory>

Materialize the speed project skeleton into a new consumer project: a
module-path go.mod (require set + local-checkout replace directives), a
cmd/server composition wiring the selected modules with the authn-then-
tenancy middleware chain, and the shared README, .gitignore, config and
lifecycle files. The skeleton's host seams (authn's MembershipReader,
org's SubjectResolver, the config resolver's host map) are left unwired
and fail closed per each module's contract; each generated file names
them as the owner's first task.

Flags:

  --with modules   Comma-separated modules to wire: authn, rbac, org
                   (default "authn,rbac,org"). Selecting rbac or org
                   requires authn -- they need an authenticating layer.
                   Use --with="" for the bare config-only skeleton.
  --speed-root dir Speed checkout the go.mod replace directives point at.
                   Default: the SPEED_ROOT environment variable, then the
                   nearest ancestor directory whose go.work lists
                   go/pkgcore, so a bare run from anywhere under a speed
                   checkout works.
  -h, --help       Show this help.

Next steps after generation:

  cd <target-directory>
  go mod tidy      # resolve the replace directives against the checkout
  go run ./cmd/server
`

// speedRootEnv is the environment variable naming the speed checkout, the
// second tier of ResolveSpeedRoot's precedence ladder.
const speedRootEnv = "SPEED_ROOT"

// switchableModules is the v0.1 switchable universe: the implemented
// modules that participate in the minimal combination. Selection keys are
// canonical renderings of subsets of this list (template.SelectionKey).
// internal/template's embed_test.go enumerates the same five keys on its
// side; a module added here must ship an embedded selection directory
// (and vice versa) or the cross-checks on one side fail.
var switchableModules = []string{"authn", "rbac", "org"}

// defaultWith is the --with default: every switchable module, the minimal
// combination the reference-app wiring exercises.
var defaultWith = strings.Join(switchableModules, ",")

// moduleNamePattern is the grammar for a generated project's module path
// (derived from the target directory's base name): it must start with a
// letter or digit and carry only letters, digits, dots, underscores and
// dashes after that. The ".." and trailing-dot rejections in
// validateModuleName are deliberate and separate -- a base name that is
// ".." (or ends in a dot) is legal to the pattern's letter but names no
// module.
var moduleNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Run executes the new command and returns its process exit code: 0 on
// success (and on -h, which prints help), 2 for usage errors -- bad
// flags, a wrong argument count, a target whose base name cannot serve as
// a module path, or an invalid --with value -- and 1 for execution errors
// (an unresolvable speed root, an existing non-empty target, an I/O
// failure). Diagnostics go to stderr; the list of written files and the
// next-steps hint go to stdout. Writes to the passed-in writers are
// best-effort -- the only realistic failure is a closed pipe, and the
// returned exit code is the whole contract either way -- so every call
// blank-assigns the write error (the repository's errcheck config runs
// with check-blank off, which is what makes the assignment the checked
// shape).
func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	with := flags.String("with", defaultWith, "comma-separated modules to wire (authn, rbac, org)")
	speedRoot := flags.String("speed-root", "", "path of the speed checkout the go.mod replaces point at")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	targets := flags.Args()
	if len(targets) != 1 {
		return usageError(stderr, fmt.Errorf("expected exactly one target directory, got %d", len(targets)))
	}
	target := targets[0]
	// The target's base name becomes the generated module path; a name
	// that cannot serve as one is a usage error, caught here before
	// anything touches the filesystem (materialize validates again, for
	// its own direct callers).
	if _, err := deriveModuleName(target); err != nil {
		return usageError(stderr, err)
	}

	modules, err := parseWith(*with)
	if err != nil {
		return usageError(stderr, err)
	}
	if err = validateSelection(modules); err != nil {
		return usageError(stderr, err)
	}
	key := template.SelectionKey(modules)

	root, err := ResolveSpeedRoot(*speedRoot)
	if err != nil {
		return reportError(stderr, err)
	}
	if err := materialize(target, root, key, stdout); err != nil {
		return reportError(stderr, err)
	}
	return 0
}

// reportError prints one prefixed line to stderr and returns the exit
// code for execution errors.
func reportError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl new: %v\n", err)
	return 1
}

// usageError prints one prefixed line plus the full usage text to stderr
// and returns the exit code for usage errors.
func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl new: %v\n\n", err)
	_, _ = fmt.Fprint(stderr, usage)
	return 2
}

// parseWith splits the --with value into module names. It is purely
// syntactic: empty entries (from a trailing comma, or a bare --with="")
// are skipped and duplicates are rejected. Unknown names and closure
// violations are validateSelection's job, so the two error classes stay
// separately testable.
func parseWith(with string) ([]string, error) {
	var modules []string
	for _, part := range strings.Split(with, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		for _, seen := range modules {
			if seen == name {
				return nil, fmt.Errorf("--with lists %q more than once", name)
			}
		}
		modules = append(modules, name)
	}
	return modules, nil
}

// validateSelection checks the parsed --with set against the switchable
// universe and the downward-closure rule. A module outside the universe
// is rejected listing the valid set; selecting rbac or org without authn
// is rejected naming the selected dependents and authn as implied.
func validateSelection(modules []string) error {
	known := map[string]bool{}
	for _, name := range switchableModules {
		known[name] = true
	}
	selected := map[string]bool{}
	for _, name := range modules {
		if !known[name] {
			valid := make([]string, len(switchableModules))
			copy(valid, switchableModules)
			return fmt.Errorf("--with names unknown module %q (valid: %s)", name, strings.Join(valid, ", "))
		}
		selected[name] = true
	}
	var dependents []string
	if selected["rbac"] {
		dependents = append(dependents, "rbac")
	}
	if selected["org"] {
		dependents = append(dependents, "org")
	}
	if len(dependents) > 0 && !selected["authn"] {
		return fmt.Errorf("--with selects %s without authn: each of them needs an authenticating layer, so authn is implied whenever it is selected with them (add authn, or drop the dependent)",
			strings.Join(dependents, " and "))
	}
	return nil
}

// ResolveSpeedRoot resolves the speed checkout path from the three
// sources, in precedence order: the --speed-root flag value, the
// SPEED_ROOT environment variable, then a walk up the ancestors of the
// working directory for a go.work whose use entries include go/pkgcore
// (the pattern of tools/release/lockstep-release.py's find_repo_root,
// sharpened to the pkgcore use entry so a random unrelated go.work up the
// tree cannot be mistaken for a speed checkout). The resolved path is
// returned absolute, after being validated to actually hold that go.work
// -- a wrong path fails here, before anything is written, instead of
// surfacing later as a go.mod whose replaces point nowhere.
func ResolveSpeedRoot(flagValue string) (string, error) {
	if flagValue != "" {
		return validateSpeedRoot(flagValue, "--speed-root")
	}
	if env := os.Getenv(speedRootEnv); env != "" {
		return validateSpeedRoot(env, speedRootEnv)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve the working directory: %w", err)
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		//nolint:gosec // G304: the file name is the constant "go.work" -- this
		// probe reads one fixed document from an ancestor directory, never a
		// caller-named file, and the walk stops at the first directory whose
		// go.work lists go/pkgcore.
		if content, err := os.ReadFile(filepath.Join(dir, "go.work")); err == nil && goWorkUsesPkgcore(content) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", errors.New("cannot find the speed checkout: pass --speed-root, set SPEED_ROOT, or run from inside a speed checkout (a directory whose go.work lists go/pkgcore)")
}

// validateSpeedRoot absolutizes a --speed-root flag or SPEED_ROOT
// environment value (named by label for error messages) and checks it
// looks like the speed checkout.
func validateSpeedRoot(path, label string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	//nolint:gosec // G304: the file name is the constant "go.work" -- this
	// reads one fixed document from a caller-chosen directory, never a
	// caller-named file, and only to check the pkgcore use entry below.
	content, err := os.ReadFile(filepath.Join(abs, "go.work"))
	if err != nil {
		return "", fmt.Errorf("%s %s does not hold a go.work (is it the speed checkout?): %w", label, abs, err)
	}
	if !goWorkUsesPkgcore(content) {
		return "", fmt.Errorf("%s %s holds a go.work that does not list go/pkgcore (is it the speed checkout?)", label, abs)
	}
	return abs, nil
}

// goWorkUsesPkgcore reports whether the go.work document's use entries
// include go/pkgcore: the marker that makes a directory THE speed
// checkout rather than just any Go workspace. Line-based and comment-
// aware rather than a real parse -- this probe must only answer one
// question about files that are already known to parse, and
// tools/check_toolchain.py's go-work gate keeps the real shape honest.
func goWorkUsesPkgcore(content []byte) bool {
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") || line == "" {
			continue
		}
		if strings.Contains(line, "go/pkgcore") {
			return true
		}
	}
	return false
}

// deriveModuleName derives the module path for the generated go.mod from
// the target directory's base name, validating it at the same time. The
// base name is taken from the lexically cleaned target, so a target
// ending in ".." collapses before the name is read -- "saasctl new .."
// is refused as the invalid base name "..", never silently materialized
// into the parent directory. The path this returns is what the go.mod's
// module line carries; callers that write into the target absolutize it
// separately with filepath.Abs.
func deriveModuleName(target string) (string, error) {
	name := filepath.Base(filepath.Clean(target))
	if err := validateModuleName(name); err != nil {
		return "", err
	}
	return name, nil
}

// validateModuleName checks one name against the grammar for a generated
// project's module path: it must start with a letter or digit and carry
// only letters, digits, dots, underscores and dashes, be neither "."
// nor "..", and not end in a dot. The grammar is deliberately stricter
// than Go's own (go mod init also accepts a leading dot and interior
// runs of dots), and a name that passes it must additionally pass
// module.CheckImportPath -- the validator the go command itself runs on
// a module path -- which is what refuses the reserved Windows device
// names (CON, AUX, PRN, NUL, COM1-9, LPT1-9, case-insensitively) on
// EVERY platform: the grammar alone would accept "aux", and "saasctl new
// aux" would then fully write a project whose go.mod no go tool accepts.
// The x/mod gate makes the "every accepted name is one `go mod init`
// accepts" property true by construction rather than by a one-time
// probe, so a project this command shapes always has a go.mod `go mod
// tidy` will parse. Rejecting before anything is created keeps a bad
// target from leaving a half-written directory behind.
func validateModuleName(name string) error {
	if name == "" {
		return errors.New("the target directory has no base name to derive a module path from")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("target base name %q cannot serve as a module path", name)
	}
	if !moduleNamePattern.MatchString(name) {
		return fmt.Errorf("target base name %q is not a valid module path: it must start with a letter or digit and carry only letters, digits, dots, underscores and dashes", name)
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("target base name %q is not a valid module path: it must not end in a dot", name)
	}
	if err := module.CheckImportPath(name); err != nil {
		return fmt.Errorf("target base name %q is not a valid module path: %w", name, err)
	}
	return nil
}

// materialize writes the embedded skeleton for one selection into target:
// the selection's go.mod and server.go plus every shared file (the shared
// set IS template.SharedFiles -- this function's list is derived from it,
// never a second enumeration), each with the build-ignore marker stripped
// (from .go files) and the app-name and speed-root tokens substituted. The
// whole file list is fixed and relative -- it is never derived from a walk
// of the target, so a hostile or stale target tree cannot smuggle paths
// into the write -- and every path joins onto the absolutized target. An
// existing target is refused unless it is an empty directory; on a write
// failure every file this call created is removed again and every
// directory it created with them, so a failed run never leaves a
// half-skeleton -- not even empty directories -- that would block the next
// attempt.
//
// The one produced file an external tool consumes is the go.mod, so its
// produced bytes are handed to the go command's own parser (modfile.Parse
// -- the same delegate-to-authority self-check `upgrade` applies to its
// rewritten output) BEFORE anything touches the filesystem:
// the replace directives embed the resolved speed-root path verbatim, the
// go.mod grammar is not every filesystem path's grammar (a space cuts a
// replace right-hand side, parentheses derail the directive's shape), and
// a document that does not parse is refused with an execution error naming
// the checkout path instead of shipping a skeleton no `go mod tidy`
// accepts. Sitting the check on the produced document rather than on the
// speed-root input is what lets it cover every resolution tier (the
// --speed-root flag, SPEED_ROOT and the ancestor-go.work discovery)
// without any tier enumerating the grammar's breaking characters. The
// bytes the gate accepts are the bytes the write loop lands, so the
// parsed document and the delivered one cannot drift apart.
func materialize(target, speedRoot, selectionKey string, stdout io.Writer) error {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve target directory: %w", err)
	}
	appName, err := deriveModuleName(target)
	if err != nil {
		return err
	}

	// Pre-render the selection's go.mod document -- the embedded asset with
	// this run's app name and speed root substituted -- and refuse a
	// document the go command cannot parse before the target directory even
	// exists. The document is embedded as go.mod.txt (see the asset list
	// below for why the .txt name is inert); the loop that writes the tree
	// reuses these very bytes for the go.mod asset.
	goModTemplate := "selection/" + selectionKey + "/go.mod.txt"
	rawGoMod, err := fs.ReadFile(template.Project, template.ProjectRoot+"/"+goModTemplate)
	if err != nil {
		return fmt.Errorf("read embedded template %s: %w", goModTemplate, err)
	}
	goModContent := replaceTokens(rawGoMod, appName, speedRoot)
	if _, parseErr := modfile.Parse("go.mod", goModContent, nil); parseErr != nil {
		return fmt.Errorf("the go.mod this run would produce does not parse after substituting the speed checkout path %q into its replace directives: %w; the path is embedded verbatim in the produced document, and a path the go.mod grammar cannot carry (a space or parentheses are the usual breakers) ships a project no go tool accepts -- relocate the checkout or pass --speed-root naming a path the grammar accepts; nothing has been created", speedRoot, parseErr)
	}

	// Refuse an existing non-empty target up front: the skeleton writes
	// fixed file names, and silently mingling them with a consumer's
	// existing tree would be the one irreversible thing this command does.
	// An existing EMPTY directory is accepted and filled, so a prepared
	// mount point or checkout directory works.
	entries, statErr := os.ReadDir(absTarget)
	switch {
	case statErr == nil:
		if len(entries) > 0 {
			return fmt.Errorf("target %s already exists and is not empty; move it aside or choose another directory", absTarget)
		}
	case errors.Is(statErr, os.ErrNotExist):
		// 0o750 (owner and group), the same owner-private mode pkgcore's
		// local object store gives the trees it creates -- the rationale is
		// in its comment there. The scaffold directory is where the
		// consumer's keys and the database file live, and sharing it
		// happens through git, whose checkouts carry their own modes. An existing empty
		// target keeps its creator's mode: MkdirAll leaves an existing
		// directory's permissions alone.
		if mkdirErr := os.MkdirAll(absTarget, 0o750); mkdirErr != nil {
			return fmt.Errorf("create target %s: %w", absTarget, mkdirErr)
		}
	default:
		return fmt.Errorf("inspect target %s: %w", absTarget, statErr)
	}

	type asset struct {
		templatePath string // inside template.ProjectRoot
		targetPath   string // relative to absTarget
	}
	// The shared half of the list is template.SharedFiles itself, never a
	// second hand-copied enumeration: a shared template file added to the
	// embed tree without a SharedFiles entry -- or added to a hard-coded
	// list here without a SharedFiles entry -- would silently never (or
	// wrongly) materialize while every test stayed green. Building the
	// list from the single exported source makes the two identical by
	// construction, and the materialization tests assert the written tree
	// against the same source in both directions, so neither side can
	// drift.
	assets := make([]asset, 0, len(template.SharedFiles)+2)
	for _, name := range template.SharedFiles {
		assets = append(assets, asset{templatePath: name, targetPath: name})
	}
	assets = append(assets,
		// The go.mod document is embedded as go.mod.txt: go:embed's
		// directory scan refuses to descend into a subdirectory containing
		// a file named go.mod (such a directory reads as a nested module
		// root), so the document carries the inert .txt name through the
		// embed and is renamed here.
		asset{templatePath: goModTemplate, targetPath: "go.mod"},
		asset{templatePath: "selection/" + selectionKey + "/server.go", targetPath: "cmd/server/server.go"},
	)

	var written []string
	removeAll := func() {
		rollbackTarget(absTarget, written)
	}
	for _, a := range assets {
		// The go.mod asset is written from the pre-rendered, parse-gated
		// bytes above; every other asset is rendered here, after the same
		// strip-and-substitute steps.
		var content []byte
		if a.templatePath == goModTemplate {
			content = goModContent
		} else {
			content, err = fs.ReadFile(template.Project, template.ProjectRoot+"/"+a.templatePath)
			if err != nil {
				removeAll()
				return fmt.Errorf("read embedded template %s: %w", a.templatePath, err)
			}
			if strings.HasSuffix(a.templatePath, ".go") {
				stripped, err := template.StripBuildIgnore(content)
				if err != nil {
					removeAll()
					return fmt.Errorf("template %s: %w", a.templatePath, err)
				}
				content = stripped
			}
			content = replaceTokens(content, appName, speedRoot)
		}

		full := filepath.Join(absTarget, a.targetPath)
		// Directories 0o750 and files 0o600, the owner-private modes of the
		// target creation above (and of the file-creating code elsewhere in
		// the repository): a generated tree is private to the invoking user
		// until they share it, and this scaffold will soon hold the
		// project's config keys and database file. gosec's G301/G306
		// ceilings are met without an exemption.
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			removeAll()
			return fmt.Errorf("create directory for %s: %w", a.targetPath, err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			removeAll()
			return fmt.Errorf("write %s: %w", a.targetPath, err)
		}
		written = append(written, a.targetPath)
		_, _ = fmt.Fprintf(stdout, "Wrote %s\n", filepath.Join(absTarget, a.targetPath))
	}

	_, _ = fmt.Fprintf(stdout, "\nGenerated %s wiring modules: %s\n", absTarget, selectionKey)
	_, _ = fmt.Fprintf(stdout, "Next: cd %s && go mod tidy && go run ./cmd/server\n", absTarget)
	return nil
}

// rollbackTarget undoes a failed materialization: it removes every file in
// written (relative to absTarget), then every directory the run created
// with them. The file half restores the tree's content; the directory half
// matters because a rollback that stopped at files would leave a half
// skeleton -- the target's own non-empty check refuses every later attempt
// over a tree that holds only empty directories, so a failed run would
// block the next one even though nothing real survived. Every directory
// below absTarget is safe to sweep because materialize only ever writes
// into a target that was empty (or absent) when the run started, so no
// directory under it predates the run; the sweep runs deepest-first so a
// parent only empties after its children are gone, absTarget itself is
// never removed (an empty target is a legal, reusable state), and a Remove
// that fails because a directory unexpectedly still holds something is
// ignored rather than masking the original error.
func rollbackTarget(absTarget string, written []string) {
	for i := len(written) - 1; i >= 0; i-- {
		_ = os.Remove(filepath.Join(absTarget, written[i]))
	}
	dirs := map[string]bool{}
	for _, rel := range written {
		for dir := filepath.Dir(rel); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			dirs[dir] = true
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	slices.SortFunc(ordered, func(a, b string) int {
		return strings.Count(b, string(filepath.Separator)) - strings.Count(a, string(filepath.Separator))
	})
	for _, dir := range ordered {
		_ = os.Remove(filepath.Join(absTarget, dir))
	}
}

// replaceTokens substitutes the two materialization tokens everywhere a
// template carries them -- TokenAppName (the module path, from the target
// directory's base name, used in the go.mod module line and in the
// generated server's error prefixes and dev key IDs) and TokenSpeedRoot
// (the resolved speed checkout, used by the go.mod replace directives) --
// in ONE left-to-right scan: each token is replaced as the scan reaches
// it, and the replacement values are written to the output and never
// re-scanned, so an application name or speed-root path whose own text
// carries a token-shaped substring survives byte-identical. Two sequential
// ReplaceAll passes would re-scan the first pass's output and substitute
// the second token into the first pass's own replacement values, corrupting
// the app name whenever it contains the speed-root token's text; the
// single-pass property is pinned byte-exact by the hand-pinned goldens in
// new_test.go (TestReplaceTokensSinglePassByteExactGoldens), whose
// expected bytes are committed literals, and end to end by
// TestRunAppNameContainingTokenTextMaterializesByteIdentical. The
// materialized-tree tests pin materialize's wiring -- every asset stripped
// and substituted once, nothing else written -- with their expectations
// substituted independently of this function (wantContent), so they cannot
// by themselves vouch for this function's behavior.
func replaceTokens(content []byte, appName, speedRoot string) []byte {
	rest := string(content)
	var out strings.Builder
	out.Grow(len(rest) + len(appName) + len(speedRoot))
	for rest != "" {
		appAt := strings.Index(rest, template.TokenAppName)
		rootAt := strings.Index(rest, template.TokenSpeedRoot)
		// The two tokens can never match at the same position (their bytes
		// diverge at the third character), so when both are present the
		// earlier occurrence is unambiguous.
		switch {
		case appAt < 0 && rootAt < 0:
			out.WriteString(rest)
			return []byte(out.String())
		case rootAt < 0 || (appAt >= 0 && appAt < rootAt):
			out.WriteString(rest[:appAt])
			out.WriteString(appName)
			rest = rest[appAt+len(template.TokenAppName):]
		default:
			out.WriteString(rest[:rootAt])
			out.WriteString(speedRoot)
			rest = rest[rootAt+len(template.TokenSpeedRoot):]
		}
	}
	return []byte(out.String())
}
