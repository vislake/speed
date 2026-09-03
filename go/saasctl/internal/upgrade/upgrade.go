// Package upgrade implements the `saasctl upgrade` command: rewriting a
// consumer project's speed module requires to one target version.
//
// Consumer projects carry every github.com/vislake/speed/go/* module at the
// same version -- the lockstep plan releases them all together, so a project
// either moves every speed module or none. `upgrade` performs that
// all-or-nothing rewrite on the project's go.mod and nothing else:
// third-party requires, replace directives, comments and formatting are
// left exactly as the Go toolchain maintains them.
//
// The rewrite is a structured edit through golang.org/x/mod/modfile, the Go
// team's own parser and printer for go.mod files -- this module's single
// third-party dependency, justified here -- rather than a text substitution
// over raw bytes. A go.mod is full of places a version string resembles but
// must not touch (third-party require lines, replace blocks, comments), and
// a hand-rolled text rewrite would corrupt a file's formatting and comments
// the moment the consumer's go.mod had been touched by go mod tidy or any
// other Go tool. Parse/rewrite/Format round-trips every line the toolchain
// itself writes; the only bytes that differ afterwards are the version
// tokens of the speed module requires.
//
// The rewrite is validated offline before anything is written: the result
// parses, every speed require carries the target version, and the replace
// directives are exactly the input's. Nothing here contacts a module proxy
// or a registry -- until M4's first release nothing is published, so the
// target version is a required --version argument, never discovered -- and
// the version is validated with the same release-version form the release
// pipeline itself enforces (internal/version). web/package.json rewrites
// are frontend work and land with the frontend-scaffold round; this package
// rewrites go.mod files only.
package upgrade

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/vislake/speed/go/saasctl/internal/version"
)

// modulePrefix is the import-path prefix shared by every module this
// repository releases. A require whose path starts with it is a speed
// module, rewritten by saasctl upgrade in lockstep with the others; the
// module set is read from the go.mod being rewritten, never hardcoded.
const modulePrefix = "github.com/vislake/speed/go/"

// defaultModPath is the go.mod the command rewrites when no path argument
// is given.
const defaultModPath = "go.mod"

const usage = `Usage: saasctl upgrade [flags] [go.mod]

Upgrade a speed consumer project to a new lockstep release: rewrite every
require of a github.com/vislake/speed/go/* module in the project's go.mod
to --version, and leave everything else -- third-party requires, replace
directives, comments, formatting -- untouched. The go.mod argument names
the project's go.mod file, defaulting to ./go.mod.

Until the first release (M4) nothing is published, so the target version is
never discovered: it is always the required --version flag, in the
v<major>.<minor>.<patch>[-prerelease] form the release pipeline validates.

Flags:

  --version version   The release version to rewrite the speed requires to
                      (required)
  -h, --help          Show this help and exit

Examples:

  saasctl upgrade --version v0.2.0
  saasctl upgrade --version v1.0.0-rc.1 /path/to/project/go.mod

The command is offline: it reads nothing but the go.mod it rewrites. Exit
codes: 0 success or help, 2 usage error, 1 execution error.
`

// Run implements the upgrade command: parse the invocation, rewrite the
// named go.mod in place, and report one line to stdout. The exit-code
// contract mirrors the sibling commands: 0 for success and help, 2 for
// usage errors (--version missing or malformed, too many positional
// arguments), 1 for execution errors. Output writes are best-effort --
// the returned exit code is the whole contract either way -- so each call
// blank-assigns the write error (the repository's errcheck config runs
// with check-blank off).
func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	target := flags.String("version", "", "release version to rewrite the speed requires to")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *target == "" {
		return usageError(stderr, errors.New("--version is required: nothing is published yet, so the target release version is never discovered"))
	}
	if err := version.Validate(*target); err != nil {
		return usageError(stderr, err)
	}
	paths := flags.Args()
	if len(paths) > 1 {
		return usageError(stderr, fmt.Errorf("expected at most one go.mod path, got %d", len(paths)))
	}
	modPath := defaultModPath
	if len(paths) == 1 {
		modPath = paths[0]
	}
	if err := rewriteFile(modPath, *target, stdout); err != nil {
		return reportError(stderr, err)
	}
	return 0
}

// usageError reports a malformed invocation: the error plus the usage text
// on stderr, exit code 2.
func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl upgrade: %v\n\n%s", err, usage)
	return 2
}

// reportError reports a failed execution: one line on stderr, exit code 1.
func reportError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl upgrade: %v\n", err)
	return 1
}

// Rewrite returns the go.mod data with every require of a
// github.com/vislake/speed/go/* module set to target, validated against the
// release-version form. It returns the number of require lines whose
// version changed (0 when the file already carries the target everywhere),
// and the byte-identical input when nothing changed. It never touches
// third-party requires, replace directives, comments or formatting -- the
// output differs from the input only in the version tokens of speed module
// requires -- and every changed result passes the offline self-check before
// it is returned.
//
// The module set is derived from the data itself, never hardcoded. An
// error is returned when data does not parse, when no speed module is
// required at all, or when target is not a valid release version.
func Rewrite(data []byte, target string) ([]byte, int, error) {
	if err := version.Validate(target); err != nil {
		return nil, 0, err
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("parse go.mod: %w", err)
	}
	if !hasSpeedRequire(f) {
		return nil, 0, errors.New("no github.com/vislake/speed/go/* requires found; nothing to rewrite")
	}
	replaces := replaceKeys(f)
	changed := 0
	for _, req := range f.Require {
		if !strings.HasPrefix(req.Mod.Path, modulePrefix) || req.Mod.Version == target {
			continue
		}
		if req.Syntax == nil {
			return nil, 0, fmt.Errorf("internal error: require %s has no syntax line to edit", req.Mod.Path)
		}
		// The version is always the last token of a require line, whether
		// it sits inside a require block or on a single-line require; the
		// trailing // indirect comment (if any) lives outside the tokens,
		// so editing the last token alone leaves it in place.
		req.Syntax.Token[len(req.Syntax.Token)-1] = target
		req.Mod.Version = target
		changed++
	}
	if changed == 0 {
		return data, 0, nil
	}
	out, err := f.Format()
	if err != nil {
		return nil, 0, fmt.Errorf("format go.mod: %w", err)
	}
	// Self-check the bytes that would be written, not the in-memory file:
	// a fresh parse proves the result is well-formed go.mod text.
	check, err := modfile.Parse("go.mod", out, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("self-check failed: rewritten go.mod does not parse: %w", err)
	}
	if err := selfCheck(check, target, replaces); err != nil {
		return nil, 0, err
	}
	return out, changed, nil
}

// rewriteFile reads the go.mod at path, rewrites its speed requires in
// place to target, and writes the result back only when something changed.
// The path is the command line's go.mod argument: the operator runs this
// tool on a checkout they own, and rewriting the file they name is the
// command's whole purpose, so gosec's file-inclusion and traversal rules
// have nothing to guard against here.
func rewriteFile(path, target string, stdout io.Writer) error {
	//nolint:gosec // G304: the path is the operator-supplied go.mod argument
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	out, changed, err := Rewrite(data, target)
	if err != nil {
		return err
	}
	if changed == 0 {
		_, _ = fmt.Fprintf(stdout, "The github.com/vislake/speed/go/* requires in %s already carry %s; nothing to rewrite\n", path, target)
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, out, info.Mode().Perm()); err != nil { //nolint:gosec // G703: same rationale as the read above
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(stdout, "Rewrote %d github.com/vislake/speed/go/* require lines to %s in %s\n", changed, target, path)
	return nil
}

// hasSpeedRequire reports whether any require in f names a speed module.
func hasSpeedRequire(f *modfile.File) bool {
	for _, req := range f.Require {
		if strings.HasPrefix(req.Mod.Path, modulePrefix) {
			return true
		}
	}
	return false
}

// A replaceKey is the (old, new) pair of one replace directive.
type replaceKey struct {
	old module.Version
	new module.Version
}

// replaceKeys snapshots a file's replace directives as sorted keys, so two
// files with the same replaces in a different order compare equal.
func replaceKeys(f *modfile.File) []replaceKey {
	keys := make([]replaceKey, 0, len(f.Replace))
	for _, r := range f.Replace {
		keys = append(keys, replaceKey{old: r.Old, new: r.New})
	}
	slices.SortFunc(keys, func(a, b replaceKey) int {
		return strings.Compare(a.old.Path, b.old.Path)
	})
	return keys
}

// selfCheck runs the offline structural checks the upgrade contract
// promises before a rewritten go.mod is written anywhere: every speed
// require carries exactly the target version -- a non-lockstep go.mod is
// the one broken state this tool must never produce -- and the replace
// directives are the input's own. It is callable with hand-crafted files
// (the tests exercise it with a mixed-version go.mod the rewrite itself
// could never produce).
func selfCheck(f *modfile.File, target string, want []replaceKey) error {
	for _, req := range f.Require {
		if !strings.HasPrefix(req.Mod.Path, modulePrefix) {
			continue
		}
		if req.Mod.Version != target {
			return fmt.Errorf("self-check failed: %s is required at %s, not the lockstep version %s", req.Mod.Path, req.Mod.Version, target)
		}
	}
	if got := replaceKeys(f); !slices.Equal(got, want) {
		return fmt.Errorf("self-check failed: replace directives changed across the rewrite")
	}
	return nil
}
