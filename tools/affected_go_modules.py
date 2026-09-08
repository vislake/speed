#!/usr/bin/env python3
"""Compute which Go modules a set of changes affects.

docs/internal/19-dev-workflow.md's planned `task test` semantics are "run
the affected modules' tests", and 18-cicd.md's layered-triggering design
states the same scope rule for CI path filtering: "only run changed
modules and their DOWNSTREAM dependent modules, with the dependency
relation derived from go.work and the workspace's own go.mod files --
never a hand-maintained mapping table". This script is that computation,
in the pure, unit-tested form the Taskfile task (and any future CI caller)
wraps: the diff comes from git, the module set from go.work, the edges
from each module go.mod's require lines, and the scope rule is
"modules whose files changed, plus every module that depends on them,
transitively".

What counts as a changed module

  * A path under a go.work module directory (go/<name>/... or
    examples/reference-app/...) maps to that module -- go.mod/go.sum
    edits included, since a dependency bump or a replace-line change
    alters what the module builds.
  * go.work / go.work.sum changes map to ALL modules: the workspace
    union decides every module's resolved dependency versions.
  * A changed path under go/ or examples/ that no go.work module
    directory covers (a scaffolded module not yet registered, a stray
    file) maps to ALL modules, the safe direction: scope is computed to
    over-run, never to skip something that might be affected.
  * Everything else -- tools/, web/, docs/, .github/, Taskfile.yml and
    other root files -- maps to no Go module.

Downstream closure

A module whose own files changed is in scope; so is every module whose
go.mod requires it, transitively (a pkgcore signature change can break
anyone). Edges are parsed from the require lines of each module's own
go.mod (github.com/vislake/speed/go/<name> prefixes), so adding a module
to go.work plus its go.mod's requires is all it takes for the closure to
cover it -- no mapping table to keep in step. The reference app is an
ordinary member of this graph on the requiring side (its go.mod requires
the go/ modules it composes) and has no dependents of its own.

The ALL fallback

The scope is printed as the single token ALL when it equals the full
module set or cannot be computed: on the base ref itself with a clean
working tree there is no diff to scope on, a missing --base ref cannot
be diffed against, and an empty module set (a change touching nothing
under go/ or examples/) would otherwise make `task test` run nothing.
ALL means the caller runs the full suite -- `task test` on main keeps
today's behaviour byte for byte, and a scoped run can never silently
become a no-op.

Git plumbing is deliberately outside the unit-tested core: the diff is
read with plain `git diff` / `git ls-files` calls (no merge-base
plumbing) so the script behaves identically under any git version.
merge-base is used when the base ref exists and HEAD is not on it, so a
feature branch is diffed against the point it diverged from main, not
against main's tip (a main that moved on is not credited with your
changes).

Usage:
    python3 tools/affected_go_modules.py [--base main] [--root DIR]

Prints one repo-root-relative module directory per line (go/dbkit,
examples/reference-app, ...), or the single token ALL. Exit codes:
0 = scope printed; 1 = git plumbing failed (message on stderr);
2 = usage error. Standard library only, Python >= 3.11.
"""

from __future__ import annotations

import argparse
import pathlib
import re
import subprocess
import sys

# go.work use entries: "./go/dbkit" -- the repo-root-relative module
# directories, the single enumeration source (a module registered in
# go.work but missing from the closure logic below would be a bug; the
# parse reads go.work itself, so no list to drift).
USE_ENTRY = re.compile(r"^\s*\./(\S+)\s*$")

# require lines naming a sibling speed module, in both go.mod shapes:
# the single-line form ("require github.com/vislake/speed/go/dbkit
# v0.0.0-...") and the parenthesized-block form, whose lines start with
# the path directly. A version must follow the path ("v0.0.0-..."),
# which is also what keeps replace lines ("replace
# github.com/.../go/pkgcore => ../pkgcore") and replace-block lines
# ("path vX => target") from matching -- replace entries name the same
# paths but are not dependencies. The prefix is the module path; the
# directory is derived by stripping the go.work-root module-path prefix
# (github.com/vislake/speed/go/X -> go/X). The reference app (module
# github.com/vislake/speed/examples/reference-app) is never the target
# of a require line, so its dir needs no mapping.
REQUIRE_LINE = re.compile(
    r"^\s*(?:require\s+)?github\.com/vislake/speed/"
    r"(go/[a-z0-9-]+)\s+v",
    re.MULTILINE,
)

# Module-path prefixes of the two source trees (go.work's module
# directories live under these), used by the unknown-path rule.
GO_ROOT = "go/"
EXAMPLES_ROOT = "examples/"


def parse_go_work(text: str) -> list[str]:
    """Module directories from a go.work file's use entries, in file
    order, repo-root-relative (go/dbkit, examples/reference-app)."""
    return [
        m.group(1)
        for line in text.splitlines()
        if (m := USE_ENTRY.match(line)) is not None
    ]


def parse_go_mod_edges(text: str) -> set[str]:
    """The sibling speed modules one go.mod requires, as their
    repo-root-relative directories (go/dbkit). require lines name module
    PATHS (github.com/vislake/speed/go/dbkit); the directory is the path
    after the github.com/vislake/speed/ prefix, which is exactly the
    repo-root-relative form go.work use entries carry. Non-speed
    requires (stdlib, third-party) are not edges for this computation."""
    return {m.group(1) for m in REQUIRE_LINE.finditer(text)}


def paths_to_modules(
    changed_paths: list[str],
    module_dirs: list[str],
    edges_by_module: dict[str, set[str]],
) -> set[str]:
    """Map changed repo-root-relative paths to the module set they
    affect: the module owning the path (longest go.work directory
    prefix), ALL for workspace-level or unknown source-tree paths, and
    then the transitive downstream closure of that set.

    ALL is represented by the sentinel string "ALL" in the returned
    set -- callers must treat its presence as the whole workspace, not
    as a directory name.
    """
    affected: set[str] = set()
    module_prefixes = sorted(module_dirs, key=len, reverse=True)

    def owning_module(path: str) -> str | None:
        for prefix in module_prefixes:
            if path == prefix or path.startswith(prefix + "/"):
                return prefix
        return None

    for path in changed_paths:
        owner = owning_module(path)
        if owner is not None:
            affected.add(owner)
        elif path in ("go.work", "go.work.sum"):
            affected.add("ALL")
        elif path.startswith(GO_ROOT) or path.startswith(EXAMPLES_ROOT):
            # Under a source tree but no registered module owns it: a
            # module directory that exists with a go.mod but is not in
            # go.work yet, or a stray file. Scope to everything rather
            # than risk skipping the very module being scaffolded.
            affected.add("ALL")
        # Anything else (tools/, web/, docs/, .github/, root files)
        # maps to no Go module.

    if "ALL" in affected or not affected:
        return {"ALL"}

    # Downstream closure over the parsed require edges, transitive.
    result = set(affected)
    frontier = list(affected)
    while frontier:
        module = frontier.pop()
        for dependent, deps in edges_by_module.items():
            if module in deps and dependent not in result:
                result.add(dependent)
                frontier.append(dependent)
    return result


def git_output(args: list[str], root: pathlib.Path) -> str:
    """One git plumbing call, captured. Raises CalledProcessError when
    git itself fails (missing repo, corrupt index) -- the caller turns
    that into exit 1, never a guessed scope."""
    proc = subprocess.run(
        ["git", *args],
        cwd=root,
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        raise subprocess.CalledProcessError(
            proc.returncode, args, output=proc.stdout, stderr=proc.stderr
        )
    return proc.stdout


def find_repo_root(start: pathlib.Path) -> pathlib.Path:
    """Walk up to the first directory carrying go.work -- the same
    detection new_module.py uses."""
    current = start.resolve()
    while True:
        if (current / "go.work").is_file():
            return current
        parent = current.parent
        if parent == current:
            raise FileNotFoundError(
                "no go.work found walking up from %s" % start
            )
        current = parent


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--base",
        default="main",
        help="git ref to diff against (default: main); a missing ref "
        "falls back to the full workspace",
    )
    parser.add_argument(
        "--root",
        default=None,
        help="repository root (default: walk up from the current "
        "directory to the first go.work)",
    )
    args = parser.parse_args(argv)

    try:
        root = (
            pathlib.Path(args.root).resolve()
            if args.root
            else find_repo_root(pathlib.Path.cwd())
        )
    except FileNotFoundError as err:
        print(str(err), file=sys.stderr)
        return 2

    go_work = (root / "go.work")
    if not go_work.is_file():
        print("no go.work at %s" % root, file=sys.stderr)
        return 2
    module_dirs = parse_go_work(go_work.read_text(encoding="utf-8"))
    if not module_dirs:
        print("go.work carries no use entries", file=sys.stderr)
        return 2

    # Edges: every module go.mod's requires of sibling modules.
    edges_by_module: dict[str, set[str]] = {}
    for module_dir in module_dirs:
        go_mod = root / module_dir / "go.mod"
        if not go_mod.is_file():
            # A go.work entry without a go.mod (should not happen; the
            # release coordinator's completeness check refuses it) --
            # treat as edgeless so the rest of the run proceeds.
            edges_by_module[module_dir] = set()
            continue
        edges_by_module[module_dir] = parse_go_mod_edges(
            go_mod.read_text(encoding="utf-8")
        )

    # Changed paths: committed diff against the base, the working-tree
    # diff (staged + unstaged), and untracked files. merge-base keeps a
    # feature branch scoped to its own commits when main has moved on.
    try:
        if args.base:
            # rev-parse --verify exits nonzero for a missing ref, so the
            # existence probe is a direct subprocess call, not git_output.
            probe = subprocess.run(
                ["git", "rev-parse", "--verify", "--quiet", args.base],
                cwd=root,
                capture_output=True,
                text=True,
                check=False,
            )
            if probe.returncode != 0:
                print(
                    "affected_go_modules: base ref %r not found -- "
                    "cannot scope the diff, falling back to ALL"
                    % args.base,
                    file=sys.stderr,
                )
                print("ALL")
                return 0
            base = args.base
            merge_base = git_output(
                ["merge-base", "HEAD", base], root
            ).strip()
            if merge_base:
                base = merge_base
            changed = git_output(
                ["diff", "--name-only", base, "HEAD"], root
            ).splitlines()
        else:
            changed = []
        changed += git_output(["diff", "--name-only"], root).splitlines()
        changed += git_output(
            ["ls-files", "--others", "--exclude-standard"], root
        ).splitlines()
    except subprocess.CalledProcessError as err:
        cmd = " ".join(err.cmd)
        detail = (err.stderr or err.stdout or "").strip()
        print(
            "affected_go_modules: git plumbing failed (%s)%s"
            % (cmd, ": " + detail if detail else ""),
            file=sys.stderr,
        )
        return 1

    affected = paths_to_modules(changed, module_dirs, edges_by_module)
    if affected == {"ALL"}:
        print("ALL")
        return 0
    for module_dir in sorted(affected):
        print(module_dir)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
