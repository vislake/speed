// Package upgrade implements the `saasctl upgrade` command: rewriting a
// consumer project's speed module requires to one target version.
//
// Consumer projects carry every github.com/vislake/speed/go/* module at the
// same version -- the lockstep plan releases them all together, so a project
// either moves every speed module or none. `upgrade` performs that
// all-or-nothing rewrite on the project's go.mod and nothing else:
// third-party requires, replace and exclude directives, comments and
// formatting are left exactly as the Go toolchain maintains them.
//
// The rewrite is a structured edit through golang.org/x/mod/modfile, the Go
// team's own parser and printer for go.mod files -- this module's single
// third-party dependency, justified here -- rather than a text substitution
// over raw bytes. A go.mod is full of places a version string resembles
// but must not touch (third-party require lines, replace and exclude
// blocks, comments), and a hand-rolled text rewrite would corrupt a file's
// formatting and comments
// the moment the consumer's go.mod had been touched by go mod tidy or any
// other Go tool. Parse/rewrite/Format round-trips every line the toolchain
// itself writes; the only bytes that differ afterwards are the version
// tokens of the speed module requires.
//
// The rewrite is validated offline before anything is written: the result
// parses, every speed require carries the target version, the replace
// directives are exactly the input's, and neither of the two directives
// that can defeat a require rewrite -- replace and exclude -- stands
// against the target: a module-to-module replace of a speed module wins
// over its require line at build time, so a stale pin would silently build
// the very pre-upgrade version the rewrite just claimed to remove, and an
// exclude of a speed module at the target version makes the go command
// refuse that version, so the module resolves to some other version or the
// build fails (local-directory replaces -- the transition-state and
// local-checkout shape -- carry no version and are never pinned, so they
// are untouched by this rule). Nothing here contacts a module proxy or a
// registry -- until M4's first release nothing is published, so the target
// version is a required --version argument, never discovered -- and the
// version is validated with the same release-version form the release
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
	"path/filepath"
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
and exclude directives, comments, formatting -- untouched. A go.mod whose
replace or exclude directives contradict --version is refused, pin or no
pin on the require lines: a replace that pins a speed module to a module
version other than --version wins over its require line at build time, and
an exclude of a speed module at --version itself makes the go command
refuse that version, so the module resolves elsewhere or the build fails.
Upgrading over either would report a clean lockstep move of a project that
does not actually build the lockstep version. The go.mod argument names
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
// third-party requires, replace and exclude directives, comments or
// formatting -- the output differs from the input only in the version
// tokens of speed module requires -- and every changed result passes the
// offline self-check before it is returned.
//
// A go.mod whose replace or exclude directives defeat the rewritten
// requires is refused (see selfCheck), whether or not the require lines
// themselves changed: a module-to-module replace of a speed module wins
// over its require line at build time, and an exclude of a speed module at
// target itself makes the go command refuse the version every rewritten
// require line claims -- either way a clean upgrade report would be a lie
// about what the project would actually build.
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
	if err = checkVersionDefeatingDirectives(f, target); err != nil {
		return nil, 0, err
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
	if err := writeFileAtomically(path, out, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(stdout, "Rewrote %d github.com/vislake/speed/go/* require lines to %s in %s\n", changed, target, path)
	return nil
}

// writeFileAtomically replaces the file at path with data, preserving its
// mode. The bytes land in a temporary file in the same directory first, are
// flushed to stable storage, and only then is the temporary file renamed
// over the target, so the replacement is atomic on the local filesystem: an
// interrupted write -- a crash, a kill, a full disk -- leaves either the old
// go.mod or the new one behind, never a truncated file, and whatever holds
// the old file open keeps reading it. The Sync before the rename is what
// makes that promise survive a crash rather than a mere error return: a
// rename without a preceding fsync can leave the new name pointing at a
// file whose data was never allocated (delayed allocation), which a crash
// then turns into a zero-length go.mod -- the very truncated state the
// temporary-file dance exists to prevent. The temporary file is removed
// again on any failure before the rename completes. A rename within one
// directory cannot cross filesystems, which is why the temporary file is
// created next to the target rather than in the system temp directory.
func writeFileAtomically(path string, data []byte, mode os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
// the one broken state this tool must never produce -- no replace or
// exclude directive defeats that claim (checkVersionDefeatingDirectives),
// and the replace directives are the input's own. It is callable with
// hand-crafted files (the tests exercise it with a mixed-version go.mod
// the rewrite itself could never produce).
func selfCheck(f *modfile.File, target string, want []replaceKey) error {
	for _, req := range f.Require {
		if !strings.HasPrefix(req.Mod.Path, modulePrefix) {
			continue
		}
		if req.Mod.Version != target {
			return fmt.Errorf("self-check failed: %s is required at %s, not the lockstep version %s", req.Mod.Path, req.Mod.Version, target)
		}
	}
	if err := checkVersionDefeatingDirectives(f, target); err != nil {
		return err
	}
	if got := replaceKeys(f); !slices.Equal(got, want) {
		return fmt.Errorf("self-check failed: replace directives changed across the rewrite")
	}
	return nil
}

// checkVersionDefeatingDirectives refuses a go.mod whose replace or exclude
// directives would defeat the version claim of a rewrite to target. Replace
// and exclude are the complete set of go.mod directives that can make what
// a project actually builds disagree with its require lines, and the rule
// and its whole instance list are pinned together here -- a future third
// directive with the same power must be added to this comment and to a loop
// below at the same time. Each instance defeats the requires in its own
// way:
//
//   - replace: a module-to-module replace -- `replace
//     github.com/vislake/speed/go/authn =>
//     github.com/vislake/speed/go/authn v0.1.0` -- takes precedence over
//     the require line at build time: go resolves the module to whatever
//     the replace names, so a require rewritten to target while a replace
//     still pins the module at an older version builds the old module. The
//     refusal names the pin and the goal version. The check is deliberately
//     scoped to the replace TARGET being a speed module at a concrete
//     version: a directory replace (the transition-state and local-checkout
//     shape, whose right-hand side is a path and carries no version) is the
//     tool's own sanctioned pre-release development form and is never
//     pinned to a version, and a replace of a speed module onto a NON-speed
//     module is a genuine fork the tool cannot version-judge -- both pass.
//
//   - exclude: an exclude of a speed module at target itself -- `exclude
//     github.com/vislake/speed/go/authn v1.0.0` -- tells the go command
//     that version is unusable, so a require line the rewrite just set to
//     target cannot be honored: the module resolves to some other version
//     when the module graph offers one, and the build fails when it does
//     not. Either way the project does not build the lockstep version every
//     require now claims. An exclude of a speed module at any other version
//     forbids nothing the rewritten requires ask for, and an exclude of a
//     NON-speed module is never the rewrite's business -- both pass.
//
// An upgrade that reported clean over either defeating shape would be a lie
// about the version the project actually builds, so each refusal names its
// directive and the pinned version.
func checkVersionDefeatingDirectives(f *modfile.File, target string) error {
	for _, r := range f.Replace {
		if !strings.HasPrefix(r.Old.Path, modulePrefix) {
			continue
		}
		if r.New.Version == "" {
			continue
		}
		if !strings.HasPrefix(r.New.Path, modulePrefix) {
			continue
		}
		if r.New.Version != target {
			return fmt.Errorf("self-check failed: replace directive pins %s to %s at %s, not the lockstep version %s -- remove or update the replace before upgrading", r.Old.Path, r.New.Path, r.New.Version, target)
		}
	}
	for _, e := range f.Exclude {
		if !strings.HasPrefix(e.Mod.Path, modulePrefix) {
			continue
		}
		if e.Mod.Version == target {
			return fmt.Errorf("self-check failed: exclude directive rules out %s at %s, the lockstep version -- remove or update the exclude before upgrading", e.Mod.Path, e.Mod.Version)
		}
	}
	return nil
}
