#!/usr/bin/env python3
"""Fragment-enumeration drift gate for tools/api_fragments.json.

tools/api_fragments.json is the single machine-readable source of truth
for the backend-fragment universe: every regeneration leg derives its
set from it rather than enumerating fragments of its own (Taskfile.yml's
api:gen and api:merge read it through tools/api_fragment_matrix.py, so
a fragment joins those legs by joining the manifest). What the manifest
cannot derive is its own agreement with the tree -- a fragment can be
added to disk and never registered, or registered and then moved -- and
that agreement is what this gate proves.

The manifest carries two lists: `fragments`, the platform-module
fragments the platform regeneration leg covers, and `app_owned`, the
api/ directories that carry the same on-disk fragment signature but
belong to the reference app's own generation flow (the app-owned leg
joins the app's three fragments into the app's own merged document and
SDK; see docs/internal/21-api-contract.md) -- they are deliberately not
platform fragments. This gate reads the manifest, the live tree and the
derivation, and fails (exit 1) on any of these disagreements:

  * a fragment registered in the manifest whose api/ directory does not
    exist on disk with its openapi.yaml and oapi-codegen.yaml;
  * an api/ directory on disk carrying openapi.yaml and
    oapi-codegen.yaml that the manifest does not register and does not
    list under `app_owned` (the tree-scan signature of a backend
    fragment; anything else that looks like one must be registered or
    listed app-owned or moved);
  * an `app_owned` directory missing from the tree or missing its
    openapi.yaml / oapi-codegen.yaml (the app-owned generation leg
    regenerates from those files, so a vanished entry is a stale
    manifest), or a directory listed in both lists;
  * a manifest entry that is malformed in itself: a missing name or
    dir, a dir that is not a repo-relative path ending in `/api`, a
    duplicated name, dir or merge_rank, or a merge_rank that is not a
    positive integer;
  * the derivation decoupled from the manifest: tools/api_fragment_matrix.py,
    executed against the tree, must emit exactly the manifest's
    fragments in manifest order with the `name`/`dir` keys its consumers
    read.

The tree scan skips .git/, .claude/ (nested worktree checkouts),
node_modules/, vendor/ and __pycache__/.

Exit codes: 0 = the manifest, the tree and the derivation agree; 1 =
drift (any disagreement above); 2 = infrastructure error (the manifest
missing or unreadable). Companion planted-drift suite:
tools/test_check_api_fragments.py.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys

MANIFEST_REL = os.path.join("tools", "api_fragments.json")
MATRIX_SCRIPT_REL = os.path.join("tools", "api_fragment_matrix.py")
FRAGMENT_SPEC = "openapi.yaml"
FRAGMENT_CONFIG = "oapi-codegen.yaml"
SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "__pycache__", ".venv"}


def _infra(message: str) -> None:
    """Report an infrastructure failure and exit 2.

    The exit-code contract (module docstring): 2 = infrastructure error
    (the manifest missing or unreadable), and only 1 = drift. SystemExit alone
    cannot carry the distinction -- sys.exit("message") exits 1 -- so
    the message is printed to stderr first and the bare code raised.
    """
    print(message, file=sys.stderr)
    raise SystemExit(2)


def _read_manifest(root: str, problems: list[str]) -> tuple[list[dict], list[str]]:
    """Return (fragments, app_owned dirs) from tools/api_fragments.json."""
    path = os.path.join(root, MANIFEST_REL)
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the fragment manifest this gate reads")
    except json.JSONDecodeError as exc:
        _infra(f"error: {path} is not parsable JSON: {exc}")
    if not isinstance(data, dict):
        _infra(f"error: {path} does not hold a JSON object")
    frags = data.get("fragments")
    if not isinstance(frags, list) or not frags:
        problems.append(f"{path}: 'fragments' must be a non-empty list")
        return [], []
    clean: list[dict] = []
    seen_names: set[str] = set()
    seen_dirs: set[str] = set()
    seen_ranks: dict[int, str] = {}
    for idx, entry in enumerate(frags):
        if not isinstance(entry, dict):
            problems.append(f"{path}: fragments[{idx}] is not an object")
            continue
        name = entry.get("name")
        frag_dir = entry.get("dir")
        if not isinstance(name, str) or not name:
            problems.append(f"{path}: fragments[{idx}] lacks a 'name'")
            continue
        if not isinstance(frag_dir, str) or not frag_dir:
            problems.append(f"{path}: fragment '{name}' lacks a 'dir'")
            continue
        if (
            frag_dir.startswith(("/", "./"))
            or ".." in frag_dir.split("/")
            or not frag_dir.endswith("/api")
        ):
            problems.append(
                f"{path}: fragment '{name}': 'dir' must be a "
                f"repo-relative path ending in '/api', not '{frag_dir}'"
            )
            continue
        if name in seen_names:
            problems.append(f"{path}: duplicate fragment name '{name}'")
            continue
        if frag_dir in seen_dirs:
            problems.append(f"{path}: duplicate fragment dir '{frag_dir}'")
            continue
        rank = entry.get("merge_rank")
        if rank is not None and (
            not isinstance(rank, int) or isinstance(rank, bool) or rank < 1
        ):
            problems.append(
                f"{path}: fragment '{name}': 'merge_rank' must be a "
                f"positive integer when present, not {rank!r}"
            )
            continue
        if rank is not None and rank in seen_ranks:
            problems.append(
                f"{path}: fragment '{name}' and '{seen_ranks[rank]}' "
                f"share merge_rank {rank}"
            )
            continue
        seen_names.add(name)
        seen_dirs.add(frag_dir)
        if rank is not None:
            seen_ranks[rank] = name
        clean.append(
            {
                "name": name,
                "dir": frag_dir,
                "merge_rank": rank,
            }
        )
    app_owned: list[str] = []
    seen_owned: set[str] = set()
    owned = data.get("app_owned")
    if owned is not None and not isinstance(owned, list):
        problems.append(f"{path}: 'app_owned' must be a list of dirs")
    elif owned:
        for entry in owned:
            if not isinstance(entry, str) or not entry:
                problems.append(f"{path}: 'app_owned' entries must be dirs")
                continue
            if (
                entry.startswith(("/", "./"))
                or ".." in entry.split("/")
                or not entry.endswith("/api")
            ):
                problems.append(
                    f"{path}: 'app_owned' entry must be a repo-relative "
                    f"path ending in '/api', not '{entry}'"
                )
                continue
            if entry in seen_owned:
                problems.append(f"{path}: duplicate 'app_owned' entry '{entry}'")
                continue
            if entry in seen_dirs:
                problems.append(
                    f"{path}: '{entry}' is registered as a fragment and "
                    f"listed under 'app_owned'"
                )
                continue
            seen_owned.add(entry)
            app_owned.append(entry)
    return clean, app_owned


def _scan_fragment_dirs(root: str) -> list[str]:
    """Every api/ directory holding openapi.yaml + oapi-codegen.yaml.

    The on-disk signature of a backend fragment the regeneration legs
    exist to regenerate; the manifest must register exactly this set.
    """
    found: list[str] = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        if os.path.basename(dirpath) != "api":
            continue
        if FRAGMENT_SPEC in filenames and FRAGMENT_CONFIG in filenames:
            found.append(os.path.relpath(dirpath, root))
    return found


def _check_derivation(root: str, frags: list[dict], problems: list[str]) -> None:
    """Execute the derivation over the live tree.

    tools/api_fragment_matrix.py is the manifest's own reader, and every
    regeneration leg consumes its array rather than enumerating fragments
    itself (Taskfile.yml's api:gen and api:merge both do). The array must
    therefore be exactly the manifest's fragments, in manifest order,
    with the name/dir keys its consumers read -- a derivation that drops,
    reorders or renames an entry silently regenerates the wrong set."""
    script = os.path.join(root, MATRIX_SCRIPT_REL)
    if not os.path.isfile(script):
        problems.append(
            f"{MATRIX_SCRIPT_REL} is missing -- the derivation every "
            f"regeneration leg reads the manifest through"
        )
        return
    try:
        proc = subprocess.run(
            [sys.executable, script, "--root", root],
            capture_output=True,
            text=True,
            cwd=root,
        )
    except OSError as exc:
        problems.append(f"cannot execute {MATRIX_SCRIPT_REL}: {exc}")
        return
    if proc.returncode != 0:
        problems.append(
            f"{MATRIX_SCRIPT_REL} exited {proc.returncode} against this "
            f"tree: {proc.stderr.strip() or proc.stdout.strip()}"
        )
        return
    try:
        derived = json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        problems.append(
            f"{MATRIX_SCRIPT_REL} output is not one JSON array: {exc}"
        )
        return
    if not isinstance(derived, list):
        problems.append(f"{MATRIX_SCRIPT_REL} output is not a JSON array")
        return
    clean = []
    for entry in derived:
        if not isinstance(entry, dict):
            problems.append(
                f"{MATRIX_SCRIPT_REL} emitted a non-object entry: {entry!r}"
            )
            continue
        extra = sorted(set(entry) - {"name", "dir"})
        if extra:
            problems.append(
                f"{MATRIX_SCRIPT_REL} emitted entry keys its consumers do "
                f"not read: {extra}"
            )
            continue
        clean.append({"name": entry.get("name"), "dir": entry.get("dir")})
    expected = [{"name": f["name"], "dir": f["dir"]} for f in frags]
    if clean != expected:
        problems.append(
            f"{MATRIX_SCRIPT_REL}'s output does not match the manifest's "
            f"fragments in manifest order: expected {expected!r} but got "
            f"{clean!r} -- every manifest fragment must reach the "
            f"regeneration legs"
        )


def check(root: str) -> tuple[list[str], int, int, int]:
    """Check every leg against the manifest; return (problems, fragments, merged, app_owned)."""
    problems: list[str] = []
    frags, app_owned = _read_manifest(root, problems)
    manifest_dirs = {f["dir"] for f in frags}
    app_owned_set = set(app_owned)

    for frag in frags:
        for needed in (FRAGMENT_SPEC, FRAGMENT_CONFIG):
            path = os.path.join(root, frag["dir"], needed)
            if not os.path.isfile(path):
                problems.append(
                    f"fragment '{frag['name']}' ({frag['dir']}): {needed} "
                    f"is missing -- a fragment registered in "
                    f"{MANIFEST_REL} must carry its spec and its generator "
                    f"config"
                )

    for frag_dir in app_owned:
        for needed in (FRAGMENT_SPEC, FRAGMENT_CONFIG):
            path = os.path.join(root, frag_dir, needed)
            if not os.path.isfile(path):
                problems.append(
                    f"app-owned fragment ({frag_dir}): {needed} is "
                    f"missing -- an '{MANIFEST_REL}' app_owned entry must "
                    f"carry the spec and generator config the app-owned "
                    f"generation leg regenerates from"
                )

    for frag_dir in _scan_fragment_dirs(root):
        if frag_dir not in manifest_dirs and frag_dir not in app_owned_set:
            problems.append(
                f"{frag_dir}: looks like an api fragment (it holds "
                f"{FRAGMENT_SPEC} and {FRAGMENT_CONFIG}) but is not "
                f"registered in {MANIFEST_REL} nor listed under its "
                f"'app_owned' entries -- register it there (the "
                f"regeneration legs derive their set from it) or move it "
                f"if it is not one"
            )

    _check_derivation(root, frags, problems)

    merged = [f for f in frags if f["merge_rank"] is not None]
    return problems, len(frags), len(merged), len(app_owned)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Fragment-enumeration drift gate: tools/api_fragments.json is "
            "the single source of truth for the backend-fragment universe "
            "(its app_owned entries name the reference app's own fragment "
            "directories, which the platform legs deliberately do not "
            "wire), and this gate fails when the manifest, the tree and "
            "the derivation every regeneration leg reads disagree. "
            "Exit 0 clean, 1 drift, 2 infrastructure error."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root (default: the current directory)",
    )
    args = parser.parse_args(argv)
    problems, n_frags, n_merged, n_app_owned = check(args.root)
    if problems:
        for problem in problems:
            print(f"api-fragments: drift    {problem}")
        print(
            "api-fragments: fix tools/api_fragments.json and the tree "
            "together -- see the module docstring of "
            "tools/check_api_fragments.py for the checked invariants"
        )
        return 1
    print(
        "api-fragments: ok    "
        f"{n_frags} fragments in {MANIFEST_REL} match the tree and the "
        f"derivation (merged: {n_merged}, app-owned: {n_app_owned})"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
