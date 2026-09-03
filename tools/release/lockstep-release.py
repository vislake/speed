#!/usr/bin/env python3
r"""The lockstep release coordinator (M0: offline verification only).

docs/internal/02-repo-and-release.md and docs/internal/18-cicd.md define the
lockstep release: every Go module and every npm package of the repository
shares ONE version number and releases together, and a single command must
be able to publish all of them at that version (the M0 exit condition,
docs/internal/15-roadmap.md). This script is the M0 deliverable for that
command: it derives the full release plan from the tree at runtime and
verifies that the plan is consistent -- OFFLINE, with zero side effects by
default. Real publishing (pushing tags, npm publish, artifacts, the GitHub
Release) is deliberately out of scope until the first real release at M4
(v1.0); see "What this round does not do" below.

Publishable set (derived at runtime, never hand-maintained):

  * Go modules: the go.work "use" block. Every entry under go/ names one
    publishable Go module; each gets its own tag go/<module>/<version>,
    the Go multi-module repository convention. Deriving the set from
    go.work -- rather than from a hand-maintained list -- is what makes a
    new module publishable the moment its go.work use entry lands; the
    script additionally verifies the go/ directory tree against go.work
    both ways, so a module missing from go.work (or a go.work entry whose
    directory carries no go.mod) fails the plan loudly instead of being
    silently skipped.
  * npm packages: every web/packages/* directory with a package.json,
    versioned by the changesets fixed group in web/.changeset/config.json
    (which must cover exactly the packages found -- the npm mirror of the
    go.work drift guard). All current versions must be uniform for the
    plan to be consistent.
  * examples/reference-app is deliberately NOT publishable: it is the
    mandatory first consumer of every module (root CLAUDE.md), consumers
    pin published modules and are never themselves published or tagged,
    and a consumer keeps its replace directives after every release (see
    the replace-cleanup section below). The publishable set is the go/
    modules only; the coordinator prints reference-app's role, never a tag
    for it.

Modes:

  Default (a VERSION argument, no flags): offline verification / dry run.
  Prints the full plan -- every go/ module with the tag it would get,
  every npm package with the version the fixed group would bump it to --
  then the preflight results, and closes with one aggregated line
  ("21 Go modules + 3 packages -> v0.3.0"). Exit 0 means the plan is
  consistent. Nothing is tagged, written, fetched or published.

  --self-test: runs this script's unittest suite offline (temp sandboxes
  only; see test_lockstep_release.py). Proves the verification gates and,
  in a scratch git repository built from the live tree's module metadata,
  that the hatched apply mode creates exactly the expected tag set at one
  version -- nothing extra -- and never touches the real repository.

  --apply: HARD-GATED. Refuses with exit 3 unless --allow-local-tag-
  creation is also passed, and even then creates LOCAL, never-pushed,
  lightweight tags only: the escape hatch exists so the self-tests can
  exercise tag creation against a scratch repository, not so this round
  can publish. See the refusal message and "What this round does not do".

What this round does not do (each item waits for the v1.0 release at M4,
docs/internal/18-cicd.md, release.yml's header):

  * push tags, create the GitHub Release, or touch any publish credential
    (none is wired anywhere in this repository);
  * run the web/ changesets flow (web/.changeset is bootstrapped -- fixed
    group over every package -- but no changeset entry exists and nothing
    version-bumps or publishes);
  * edit any module go.mod or web package.json version field: the tree
    stays in its pre-release transition state (sibling replace lines and
    0.0.0 / zero pseudo-versions) until the first real release at M4;
  * build artifacts (images, goreleaser binaries, speed.yaml, SBOMs) or
    run scaffold-verify.

Preflight checks (all must pass for exit 0):

  1. VERSION matches the release-version form
     ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ -- the leading "v" is
     required. This is the exact contract the CI workflow enforces on its
     version input; the two copies must stay in step (both cite the
     regex).
  2. No tag for this version already exists (git tag -l, per module tag).
  3. The go/ tree is complete against go.work in both directions.
  4. web/ package versions are uniform and the changesets fixed group
     covers exactly the packages that exist.

First-release replace cleanup (docs/internal/18-cicd.md step 3) ships in
this module as PURE FUNCTIONS -- first_release_replace_cleanup and its
error type -- exercised ONLY by the self-tests against the fixtures under
tools/release/testdata/. No operational mode of this script calls them,
and no mode of this script ever reads or edits a live go.mod beyond
existence checks: the live tree keeps its transition state until M4, when
the release round wires the cleanup into the first real release.

Usage:
    python3 tools/release/lockstep-release.py v0.3.0        # offline plan
    python3 tools/release/lockstep-release.py --self-test   # unittest suite
    python3 tools/release/lockstep-release.py --apply v0.3.0
        --allow-local-tag-creation                          # LOCAL tags only

Example, run from the repository root (output shape; the current tree
carries 21 go/ modules and 3 web packages):

    $ python3 tools/release/lockstep-release.py v0.3.0
    Lockstep release plan for v0.3.0
    ================================
    ...
    [ok] ...
    21 Go modules + 3 packages -> v0.3.0
    $ echo $?
    0

Exit codes: 0 = plan consistent (or self-tests passed, or local tags
created); 1 = plan inconsistent (preflight failed, drift, duplicate
version) or self-tests failed; 2 = usage or validation error; 3 = --apply
refused (escape hatch not passed).

Standard library only; requires Python >= 3.11 (shared floor with the
other tools/ scripts) and git on PATH (for tag listing and, in the gated
apply mode, tag creation).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import unittest

# The release-version form: the leading "v" is required. Kept in step with
# the version-input validation in .github/workflows/release.yml -- both
# cite this exact pattern.
VERSION_PATTERN = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$")

# Module path prefix shared by every Go module in the monorepo, and the
# prefix whose go.work entries are publishable (new_module.py names the
# same constant).
MODULE_PATH_PREFIX = "github.com/vislake/speed/go"

# Directory names / paths relative to the repository root.
GO_DIR_NAME = "go"
WEB_DIR_NAME = "web"
WEB_PACKAGES_DIR_NAME = "packages"
REPO_MARKER_FILE = "go.work"  # the root is a go.work workspace, not a module
CHANGESETS_CONFIG_REL = os.path.join(
    WEB_DIR_NAME, ".changeset", "config.json"
)

# The in-repo replace LHS shape: github.com/vislake/speed/go/<dir>. Only
# these are in scope for the first-release cleanup.
IN_REPO_MODULE_RE = re.compile(
    r"^" + re.escape(MODULE_PATH_PREFIX) + r"/([A-Za-z0-9-]+)$"
)


def _n(count: int, word: str) -> str:
    """count + word, pluralized ('1 module', '21 modules')."""
    return f"{count} {word}{'' if count == 1 else 's'}"


class ReleaseError(Exception):
    """A release plan is inconsistent (or the repository cannot produce one).

    Raised by every derivation and preflight step; main() reports it on
    stderr and exits 1. Message is a complete sentence naming what failed
    and why.
    """


def find_repo_root(start: str) -> str:
    """Return the nearest ancestor of start that contains go.work."""
    root = os.path.abspath(start)
    while True:
        if os.path.isfile(os.path.join(root, REPO_MARKER_FILE)):
            return root
        parent = os.path.dirname(root)
        if parent == root:
            raise ReleaseError(
                f"cannot find the repository root (a directory containing "
                f"{REPO_MARKER_FILE}) from {start!r}; run from the repo"
            )
        root = parent


def run_git(repo_root: str, *args: str) -> str:
    """Run git -C repo_root with args; return stdout. ReleaseError on failure."""
    try:
        proc = subprocess.run(
            ["git", "-C", repo_root, *args],
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError as exc:
        raise ReleaseError(f"cannot run git: {exc}")
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout).strip()
        raise ReleaseError(
            f"git {args[0]} failed (exit {proc.returncode})"
            + (f": {detail}" if detail else "")
        )
    return proc.stdout


def list_existing_tags(repo_root: str) -> set[str]:
    """Return every git tag currently in the repository."""
    out = run_git(repo_root, "tag", "-l")
    return set(out.split())


def parse_gowork_uses(go_work_text: str) -> list[str]:
    """Parse a go.work text into its use entries (relative, no ./ prefix).

    Understands the block form (use ( ... )) that gofmt emits, the single
    form (use ./a ./b), comments and stray-parenthesis tolerance. Raises
    ReleaseError with a go.work:LINE context on malformed input.
    """
    entries: list[str] = []
    in_block = False
    for lineno, raw in enumerate(go_work_text.splitlines(), 1):
        s = raw.strip()
        if not s or s.startswith("//"):
            continue
        if in_block:
            if s == ")":
                in_block = False
                continue
            if s.endswith(")"):
                s = s[:-1].strip()
                in_block = False
            if s:
                entries.extend(s.split())
            continue
        if s.startswith("use"):
            rest = s[len("use"):].strip()
            if rest == "(":
                in_block = True
            elif rest.startswith("("):
                rest = rest[1:].strip()
                if rest.endswith(")"):
                    rest = rest[:-1].strip()
                else:
                    in_block = True
                if rest:
                    entries.extend(rest.split())
            elif rest:
                entries.extend(rest.split())
            else:
                raise ReleaseError(
                    f"go.work:{lineno}: 'use' with no module list"
                )
            continue
        if s == ")":
            raise ReleaseError(f"go.work:{lineno}: stray ')' outside a use block")
        # Any other directive (go, toolchain, godebug) is not a use entry.
    if in_block:
        raise ReleaseError("go.work: unterminated use ( block")
    return [
        entry[2:] if entry.startswith("./") else entry
        for entry in entries
    ]


def derive_go_modules(repo_root: str) -> tuple[list[str], list[str]]:
    """Return (publishable go/ modules, consumer modules), sorted.

    Publishable modules are the go.work use entries under go/ whose
    directory carries a go.mod; consumers are every other go.work entry
    (today: examples/reference-app). The go/ tree and go.work are checked
    against each other in both directions, so a module added on either
    side alone fails the plan loudly.
    """
    go_dir = os.path.join(repo_root, GO_DIR_NAME)
    go_work_path = os.path.join(repo_root, REPO_MARKER_FILE)
    try:
        with open(go_work_path, encoding="utf-8") as fh:
            go_work_text = fh.read()
    except OSError as exc:
        raise ReleaseError(f"cannot read {go_work_path}: {exc}")
    entries = parse_gowork_uses(go_work_text)
    if not entries:
        raise ReleaseError(
            f"{REPO_MARKER_FILE} declares no modules in its use block"
        )

    registered_go: set[str] = set()
    registered_consumers: set[str] = set()
    for entry in entries:
        if not entry:
            continue
        if entry == GO_DIR_NAME or entry.startswith(GO_DIR_NAME + "/"):
            rel = entry[len(GO_DIR_NAME):].lstrip("/")
            if not rel or "/" in rel:
                raise ReleaseError(
                    f"go.work registers {entry!r}, which is not a module "
                    f"directory under {GO_DIR_NAME}/ (expected "
                    f"{GO_DIR_NAME}/<module>)"
                )
            registered_go.add(rel)
        else:
            registered_consumers.add(entry)

    try:
        on_disk = sorted(
            d for d in os.listdir(go_dir)
            if os.path.isfile(os.path.join(go_dir, d, "go.mod"))
        )
    except OSError as exc:
        raise ReleaseError(f"cannot list {go_dir}: {exc}")

    # Each module's go.mod must declare the path its directory implies: the
    # plan derives every tag as go/<directory>/<version>, so a go.mod whose
    # module line disagrees would be tagged under a path no Go consumer
    # could ever resolve for it. Checking only that a go.mod FILE exists
    # (as the on_disk scan above does) leaves that all-green.
    for d in on_disk:
        mod_path = os.path.join(go_dir, d, "go.mod")
        try:
            with open(mod_path, encoding="utf-8") as fh:
                mod_text = fh.read()
        except OSError as exc:
            raise ReleaseError(f"cannot read {mod_path}: {exc}")
        declared = None
        for line in mod_text.splitlines():
            m = re.match(r"^module\s+(\S+)\s*$", line.strip())
            if m:
                declared = m.group(1)
                break
        expected = f"{MODULE_PATH_PREFIX}/{d}"
        if declared is None:
            raise ReleaseError(
                f"{GO_DIR_NAME}/{d}/go.mod has no module directive -- the "
                f"release plan cannot confirm that its tag "
                f"{GO_DIR_NAME}/{d}/<version> names the module it declares"
            )
        if declared != expected:
            raise ReleaseError(
                f"{GO_DIR_NAME}/{d}/go.mod declares module {declared!r} but "
                f"its directory implies {expected!r} -- the plan would tag "
                f"it {GO_DIR_NAME}/{d}/<version>, which Go consumers could "
                f"never resolve for {declared!r}; rename the directory or "
                f"fix the module line"
            )

    missing_from_gowork = sorted(set(on_disk) - registered_go)
    if missing_from_gowork:
        raise ReleaseError(
            "go/ module tree is not complete against go.work: "
            + ", ".join(
                f"{GO_DIR_NAME}/{d} has a go.mod but no go.work use entry"
                for d in missing_from_gowork
            )
            + " -- add the missing use entry (a module missing from the "
            "plan would never be tagged, and consumers could not pin it)"
        )
    for rel in sorted(registered_go - set(on_disk)):
        full = os.path.join(go_dir, rel)
        if os.path.isdir(full):
            raise ReleaseError(
                f"go.work registers {GO_DIR_NAME}/{rel} but "
                f"{GO_DIR_NAME}/{rel}/go.mod does not exist"
            )
        raise ReleaseError(
            f"go.work registers {GO_DIR_NAME}/{rel} but the directory does "
            f"not exist"
        )
    consumers: list[str] = []
    for entry in sorted(registered_consumers):
        full = os.path.join(repo_root, entry)
        if not os.path.isfile(os.path.join(full, "go.mod")):
            raise ReleaseError(
                f"go.work registers consumer module {entry!r} but "
                f"{entry}/go.mod does not exist"
            )
        consumers.append(entry)
    return on_disk, consumers


def derive_npm_packages(repo_root: str) -> list[dict[str, str]]:
    """Return the deliverable npm packages: web/packages/* with a package.json.

    Each entry carries {"name", "dir", "version"} and the list is sorted
    by package name. A package.json that cannot be parsed, or that lacks
    name or version, fails the plan.
    """
    packages_root = os.path.join(
        repo_root, WEB_DIR_NAME, WEB_PACKAGES_DIR_NAME
    )
    try:
        dirs = sorted(d for d in os.listdir(packages_root))
    except OSError as exc:
        raise ReleaseError(f"cannot list {packages_root}: {exc}")
    packages: list[dict[str, str]] = []
    for d in dirs:
        pkg_json = os.path.join(packages_root, d, "package.json")
        if not os.path.isfile(pkg_json):
            continue  # not a package directory (web/packages holds packages only)
        try:
            with open(pkg_json, encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, json.JSONDecodeError) as exc:
            raise ReleaseError(f"cannot parse {pkg_json}: {exc}")
        name = data.get("name")
        version = data.get("version")
        if not isinstance(name, str) or not name:
            raise ReleaseError(
                f"{pkg_json} carries no package name; every package "
                f"directory needs one"
            )
        if not isinstance(version, str) or not version:
            raise ReleaseError(
                f"{os.path.join('web/packages', d)} has no version field; "
                f"the npm half of the plan cannot verify it"
            )
        packages.append({"name": name, "dir": d, "version": version})
    if not packages:
        raise ReleaseError(
            f"no npm packages found under {os.path.join(WEB_DIR_NAME, WEB_PACKAGES_DIR_NAME)}"
        )
    packages.sort(key=lambda p: p["name"])
    return packages


def load_changesets_fixed_group(repo_root: str) -> set[str]:
    """Return the package names of the web/ changesets fixed group.

    web/.changeset/config.json must exist (the M0 bootstrap of the npm
    half) and its "fixed" array must be a list of groups; every group must
    list at least two packages (a fixed group of one is a tautology) and
    every listed name must exist as a package. The coordinator's own
    coverage check compares this set against the packages derived at
    runtime -- the mirror of the go.work drift guard.
    """
    config_path = os.path.join(repo_root, CHANGESETS_CONFIG_REL)
    try:
        with open(config_path, encoding="utf-8") as fh:
            data = json.load(fh)
    except OSError as exc:
        raise ReleaseError(
            f"cannot read {CHANGESETS_CONFIG_REL} (the web/ changesets "
            f"bootstrap must exist for the npm half to verify): {exc}"
        )
    except json.JSONDecodeError as exc:
        raise ReleaseError(
            f"cannot parse {CHANGESETS_CONFIG_REL}: {exc}"
        )
    fixed = data.get("fixed")
    if not isinstance(fixed, list) or not fixed:
        raise ReleaseError(
            f"{CHANGESETS_CONFIG_REL} declares no fixed group; every web/ "
            f"package must sit in one (lockstep versioning)"
        )
    names: set[str] = set()
    for group_no, group in enumerate(fixed, 1):
        if not isinstance(group, list) or len(group) < 2:
            raise ReleaseError(
                f"{CHANGESETS_CONFIG_REL}: fixed group #{group_no} must "
                f"list at least two packages"
            )
        for name in group:
            if not isinstance(name, str) or not name:
                raise ReleaseError(
                    f"{CHANGESETS_CONFIG_REL}: fixed group #{group_no} "
                    f"contains a non-package entry {name!r}"
                )
            names.add(name)
    return names


def check_tag_collision(repo_root: str, version: str, go_modules: list[str]) -> None:
    """Fail when any module tag for this version already exists."""
    tags = list_existing_tags(repo_root)
    wanted = [f"{GO_DIR_NAME}/{d}/{version}" for d in go_modules]
    existing = sorted(t for t in wanted if t in tags)
    if existing:
        raise ReleaseError(
            f"version {version} is already released: the tag(s) "
            + ", ".join(existing)
            + " already exist -- pick a new version (lockstep release "
            "versions are used exactly once)"
        )


def check_npm_uniform(npm_packages: list[dict[str, str]]) -> None:
    """Fail when the npm package versions are not all identical."""
    versions = sorted({p["version"] for p in npm_packages})
    if len(versions) > 1:
        detail = "; ".join(f"{p['name']} at {p['version']}" for p in npm_packages)
        raise ReleaseError(
            "web/ package versions are not uniform: " + detail
            + " -- the changesets fixed group bumps every package together, "
            "so every package must sit at one version before a release plan "
            "can name the next one"
        )


def check_changesets_coverage(
    fixed_names: set[str], npm_packages: list[dict[str, str]]
) -> None:
    """Fail when the fixed group and the derived packages disagree."""
    package_names = {p["name"] for p in npm_packages}
    if package_names != fixed_names:
        only_fixed = sorted(fixed_names - package_names)
        only_packages = sorted(package_names - fixed_names)
        parts: list[str] = []
        if only_fixed:
            parts.append(
                "in the fixed group but not found as packages: "
                + ", ".join(only_fixed)
            )
        if only_packages:
            parts.append(
                "found as packages but missing from the fixed group: "
                + ", ".join(only_packages)
            )
        raise ReleaseError(
            "web/.changeset fixed group does not cover exactly the "
            "packages found under web/packages: " + "; ".join(parts)
            + " -- when a package joins or leaves the workspace, update "
            "the fixed group in web/.changeset/config.json in the same "
            "change (the drift guard mirrors the go.work one)"
        )


def print_plan(
    version: str,
    go_modules: list[str],
    consumers: list[str],
    npm_packages: list[dict[str, str]],
    ok_lines: list[str],
    applying: bool,
) -> None:
    """Print the full plan. Only called once every preflight check passed."""
    title = f"Lockstep release plan for {version}"
    print(title)
    print("=" * len(title))
    print()
    print("Publishable set (derived at runtime, never hand-maintained):")
    print(f"  Go modules: go.work use entries under go/ -- "
          f"{_n(len(go_modules), 'publishable module')}, one tag "
          "go/<module>/<version> each")
    if consumers:
        print("  Consumers: " + ", ".join(consumers) + " -- go.work module(s) "
              "outside go/, published never, tagged never (they pin "
              "published modules and keep their replace directives after "
              "every release)")
    print(f"  npm packages: web/packages/* with a package.json -- "
          f"{_n(len(npm_packages), 'package')}, versioned by the "
          "web/.changeset fixed group")
    print()
    print(f"Go modules -> tags (Go multi-module repo convention):")
    for d in go_modules:
        print(f"  {GO_DIR_NAME}/{d} -> {GO_DIR_NAME}/{d}/{version}")
    print()
    print("npm packages -> the web/.changeset fixed group bumps all of them "
          f"to {version[1:]} together (current versions must be uniform):")
    for p in npm_packages:
        print(f"  {p['name']}: {p['version']} -> {version[1:]}")
    print()
    print("Preflight checks:")
    for line in ok_lines:
        print(f"  {line}")
    print()
    if applying:
        print(f"Local application only (--allow-local-tag-creation): creating "
              f"{_n(len(go_modules), 'local, lightweight, never-pushed tag')} "
              "at HEAD in this checkout.")
    else:
        print("Dry run: nothing was tagged, written or published, and no "
              "network was touched. The tree keeps its pre-release "
              "transition state (go.mod replace lines, 0.0.0 and zero "
              "pseudo-versions) until the first real release at M4 (v1.0), "
              "which is when tags are pushed, the web/ packages are bumped "
              "and published, and the first-release replace cleanup runs.")
    print()
    print(f"{len(go_modules)} Go modules + {len(npm_packages)} packages "
          f"-> {version}")


def create_local_tags(
    repo_root: str, version: str, go_modules: list[str]
) -> None:
    """Create one local lightweight tag per module (gated apply mode)."""
    for d in go_modules:
        tag = f"{GO_DIR_NAME}/{d}/{version}"
        # Failure aborts the whole run: a tag that exists mid-way means the
        # preflight collision check raced with something else.
        run_git(repo_root, "tag", tag)
        print(f"  tag {tag}")


def print_apply_not_done() -> None:
    """State plainly what the gated apply mode does not do."""
    print("NOT done by this M0 round -- each item waits for the v1.0 "
          "release at M4 (docs/internal/18-cicd.md, release.yml header):")
    print("  - pushing these tags or creating the GitHub Release; no publish "
          "credential exists anywhere in this repository")
    print("  - the web/ changesets bump and npm publish (web/.changeset is "
          "bootstrapped only)")
    print("  - the first-release go.mod replace cleanup (the engine ships "
          "fixture-tested; the live tree keeps its transition state)")
    print("  - artifacts: multi-arch images, goreleaser binaries, the merged "
          "speed.yaml, SBOMs, scaffold-verify")


# ---------------------------------------------------------------------------
# First-release replace cleanup (docs/internal/18-cicd.md step 3), as pure
# functions. Exercised ONLY by the self-tests against the fixtures under
# tools/release/testdata/ -- no operational mode calls them, and no mode of
# this script ever reads or edits a live go.mod. The release round at M4
# wires them into the first real release, when every module tag exists and
# the transitional sibling replaces can finally be deleted.
# ---------------------------------------------------------------------------


class UncleanableReplaceError(Exception):
    """A publishable module's go.mod carries a replace the first release
    cannot simply delete.

    The transitional form the first release removes is exactly
    "replace github.com/vislake/speed/go/X => ../X" (a sibling-directory
    replace matching the replaced module's own name) in a module whose
    declaration is itself under github.com/vislake/speed/go/. Anything
    else -- a consumer-shaped relative path (../../go/X), a sibling path
    that does not match the LHS directory, a versioned target -- needs a
    human decision before release tooling touches the file, so it fails
    the cleanup instead of being edited away.
    """


def first_release_replace_cleanup(go_mod_text: str) -> tuple[str, tuple[str, ...]]:
    """Return (go_module, drops) for one go.mod's first-release cleanup.

    go_module is the module path the go.mod declares. drops lists, in line
    order, the module paths of the sibling replace directives that the
    first real release of this module deletes. A go.mod declaring a module
    OUTSIDE the publishable prefix -- a consumer such as
    examples/reference-app -- always yields an empty drops tuple: consumers
    are never released and keep their replaces after every release, and the
    cleanup does not police their shape.

    Raises UncleanableReplaceError when a publishable module carries a
    replace directive the first release cannot simply delete.
    """
    module_decl = None
    replace_tokens: list[tuple[str, str, str | None, int]] = []
    block_kind: str | None = None
    for lineno, raw in enumerate(go_mod_text.splitlines(), 1):
        s = raw.strip()
        if not s or s.startswith("//"):
            continue
        # Strip a trailing line comment before any directive regex sees the
        # line, so both parser branches below treat "X => ../X // note" the
        # same as the bare form. Without this the single-line branch matched
        # nothing (silently planning no drop) while the block branch refused
        # the file -- an asymmetry that could have shipped a first release
        # whose go.mod still carried a transitional replace. No legal module
        # path or replace target contains "//".
        s = s.split("//", 1)[0].strip()
        if not s:
            continue
        m = re.match(r"^module\s+(\S+)\s*$", s)
        if m:
            module_decl = m.group(1)
            continue
        if block_kind is not None:
            if s == ")":
                block_kind = None
                continue
            if s.endswith(")"):
                s = s[:-1].strip()
                block_kind = None
            if block_kind == "replace":
                m = re.match(r"^(\S+)\s+=>\s+(\S+)(?:\s+(\S+))?\s*$", s)
                if not m:
                    raise UncleanableReplaceError(
                        f"{lineno}: malformed directive inside a replace "
                        f"block: {raw!r}"
                    )
                replace_tokens.append(
                    (m.group(1), m.group(2), m.group(3), lineno)
                )
            # Any other block (require, exclude, retract) carries no
            # replace directives; its contents are skipped.
            continue
        if s == ")":
            raise UncleanableReplaceError(
                f"{lineno}: stray ')' -- replace blocks open with "
                f"'replace (' and close with ')'"
            )
        m = re.match(r"^(\w+)\s*\(\s*$", s)
        if m:
            block_kind = m.group(1)
            continue
        m = re.match(r"^replace\s+(\S+)\s+=>\s+(\S+)(?:\s+(\S+))?\s*$", s)
        if m:
            replace_tokens.append((m.group(1), m.group(2), m.group(3), lineno))
            continue
        # Any other directive (require, exclude, retract, go, toolchain,
        # godebug) or their block contents: not replace directives.
    if module_decl is None:
        raise UncleanableReplaceError("no module directive found")
    if not module_decl.startswith(MODULE_PATH_PREFIX + "/"):
        # Consumer shape: never released, replaces stay forever, shape not
        # policed.
        return module_decl, ()

    drops: list[str] = []
    for lhs, rhs, target_version, lineno in replace_tokens:
        m = IN_REPO_MODULE_RE.match(lhs)
        if not m:
            continue  # third-party replace: never planned for removal
        module_dir = m.group(1)
        expected = f"../{module_dir}"
        if rhs != expected or target_version is not None:
            raise UncleanableReplaceError(
                f"{lineno}: replace {lhs} => {rhs}"
                + (f" {target_version}" if target_version else "")
                + f" in publishable module {module_decl} is not the "
                f"transitional sibling form ({lhs} => {expected}); a "
                "human must decide how to clean it before release tooling "
                "touches this go.mod"
            )
        drops.append(lhs)
    return module_decl, tuple(drops)


# ---------------------------------------------------------------------------
# --self-test mode: run the unittest suite that lives next to this script.
# ---------------------------------------------------------------------------


def run_self_tests() -> int:
    """Discover and run test_lockstep_release.py from this directory."""
    tests_dir = os.path.dirname(os.path.abspath(__file__))
    if tests_dir not in sys.path:
        sys.path.insert(0, tests_dir)
    suite = unittest.defaultTestLoader.discover(
        start_dir=tests_dir, pattern="test_*.py"
    )
    runner = unittest.TextTestRunner(verbosity=2, stream=sys.stdout)
    result = runner.run(suite)
    return 0 if result.wasSuccessful() else 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="lockstep-release.py",
        description=(
            "Derive and verify the lockstep release plan for one version, "
            "offline (M0). Default mode prints the plan -- every go/ module "
            "with its go/<module>/<version> tag and every web/ package with "
            "the version the changesets fixed group bumps it to -- and "
            "exits 0 only when the plan is consistent. Nothing is tagged, "
            "written or published by default."
        ),
        epilog=(
            "This is the command behind the root Taskfile.yml's release:plan "
            "task and behind .github/workflows/release.yml (its offline "
            "verification steps). Real publishing -- pushing tags, npm "
            "publish, artifacts, the GitHub Release -- is scheduled for the "
            "v1.0 release at M4; until then no mode of this script touches "
            "anything outside the repository's own tree."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "version",
        nargs="?",
        metavar="VERSION",
        help="version to release, in release-version form (v prefix "
        "required): v1.2.0, v1.2.0-rc.1, ... -- validated against "
        + VERSION_PATTERN.pattern,
    )
    parser.add_argument(
        "--apply",
        action="store_true",
        help="attempt to apply the plan locally. REFUSED (exit 3) unless "
        "--allow-local-tag-creation is also passed, and even then creates "
        "only local, lightweight, never-pushed tags -- the escape hatch "
        "exists so the self-tests can exercise tag creation against a "
        "scratch repository",
    )
    parser.add_argument(
        "--allow-local-tag-creation",
        action="store_true",
        help="escape hatch for --apply: allow creating LOCAL git tags in "
        "the current repository. They are never pushed and nothing is "
        "published; use only against a scratch checkout",
    )
    parser.add_argument(
        "--self-test",
        action="store_true",
        help="run this script's unittest suite (test_lockstep_release.py) "
        "and exit 0 only when every test passes",
    )
    args = parser.parse_args(argv)

    if args.self_test:
        if args.version is not None or args.apply or args.allow_local_tag_creation:
            print("error: --self-test takes no other arguments",
                  file=sys.stderr)
            return 2
        return run_self_tests()

    if args.version is None:
        parser.error("VERSION is required: the version to release, in "
                     "release-version form (v prefix required), e.g. v1.2.0 "
                     "or v1.2.0-rc.1")
    version = args.version
    if not VERSION_PATTERN.match(version):
        print(f"error: version {version!r} is not a release-version form: "
              "expected " + VERSION_PATTERN.pattern + " -- the leading 'v' "
              "is required (v1.2.0, v1.2.0-rc.1, ...)", file=sys.stderr)
        return 2

    try:
        repo_root = find_repo_root(os.getcwd())
    except ReleaseError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    if args.apply and not args.allow_local_tag_creation:
        print("error: --apply is refused: nothing in this repository may "
              "publish for real yet. Real publishing -- pushing tags, the "
              "web/ changesets bump and npm publish, artifacts, the GitHub "
              "Release -- is scheduled for the v1.0 release at M4, and "
              ".github/workflows/release.yml wires no publish credential, "
              "so this M0 round cannot publish by construction. Re-run "
              "without --apply for the offline verification plan, or pass "
              "--allow-local-tag-creation to create LOCAL, never-pushed "
              "tags only (deliberate exercise of the tag-creation half "
              "against a scratch checkout).", file=sys.stderr)
        return 3

    ok_lines: list[str] = []
    try:
        ok_lines.append(
            f"[ok] version {version} matches "
            + VERSION_PATTERN.pattern
        )
        go_modules, consumers = derive_go_modules(repo_root)
        ok_lines.append(
            f"[ok] go/ module tree complete against go.work: "
            f"{_n(len(go_modules), 'publishable module')}"
            + (f" and {_n(len(consumers), 'consumer module')}"
               if consumers else "")
        )
        npm_packages = derive_npm_packages(repo_root)
        check_npm_uniform(npm_packages)
        fixed_names = load_changesets_fixed_group(repo_root)
        check_changesets_coverage(fixed_names, npm_packages)
        current = npm_packages[0]["version"]
        ok_lines.append(
            f"[ok] web/ package versions uniform ({current}) and the "
            f"web/.changeset fixed group covers exactly the "
            f"{_n(len(npm_packages), 'package')} found"
        )
        check_tag_collision(repo_root, version, go_modules)
        ok_lines.append(
            f"[ok] no existing tag for version {version} (git tag -l over "
            f"all {len(go_modules)} module tags)"
        )
    except ReleaseError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    print_plan(
        version, go_modules, consumers, npm_packages, ok_lines,
        applying=args.apply,
    )
    if args.apply:
        print(f"creating {len(go_modules)} local tags in {repo_root}:")
        try:
            create_local_tags(repo_root, version, go_modules)
        except ReleaseError as exc:
            print(f"error: {exc}", file=sys.stderr)
            return 1
        print()
        print_apply_not_done()
    return 0


if __name__ == "__main__":
    sys.exit(main())
