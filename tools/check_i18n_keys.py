#!/usr/bin/env python3
"""zh-CN / en-US locale message-key set consistency checker.

Root CLAUDE.md's internationalization rule: every user-facing text ships in
both zh-CN and en-US resources, and CI checks that the two key sets match
("New text must ship with both zh-CN and en-US resources; CI checks the key
sets match"). docs/internal/18-cicd.md schedules this as a self-written
discipline check diffing the key sets of the two resources; this script is
its local-run counterpart.

What it does: for every locale directory under --root, it parses zh-CN.toml
and en-US.toml (TOML in the go-i18n style used by the repository, e.g.
examples/reference-app/internal/notes/locales/), extracts each file's
message-id set with tomllib, and reports every id present in one file but
not the other. Exit code is nonzero on any mismatch, missing pair member,
or unparsable file.

Message-id semantics (must stay in step with how the repository names
keys): a message id is the key of a key/value pair whose value is not
itself a table -- the leaf key. Quoted keys are TOML-native single keys, so
a quoted dotted key like "notes.text_required" stays whole and *is* the
message id, exactly as handler code references it. Section headers such as
[errors] are organizational grouping, not part of the id; a flat file and a
section-grouped file with the same leaf keys therefore match. Defensive
rule: if one file defines the same leaf id under two different paths (two
sections both containing a key of the same name), pairing across languages
is ambiguous, so that file is reported as an error rather than compared.

Discovery: a locale directory is any directory that contains a file named
zh-CN.toml or en-US.toml; --root itself is searched first, then the tree
below it is walked (skipping .git, node_modules and vendor). Both zh-CN.toml
and en-US.toml must exist in every locale directory; any other *.toml in
the same directory is reported as informational only -- the repository's
pair discipline is exactly zh-CN/en-US, and additional languages, when they
appear, should extend this script's pair table rather than slip past it.

Usage:
    python3 tools/check_i18n_keys.py [--root PATH]

--root defaults to the current directory. Paths in the report are relative
to --root.

Exit codes: 0 = every found pair matches; 1 = at least one mismatch,
missing pair member, or unparsable file; 2 = bad usage (--root is not a
directory). Standard library only; requires Python >= 3.11 (tomllib).
"""

from __future__ import annotations

import argparse
import os
import sys
import tomllib
from typing import Any

PAIR = ("zh-CN.toml", "en-US.toml")
PAIR_STEMS = frozenset(fn[:-5] for fn in PAIR)  # "zh-CN.toml" -> "zh-CN"
PRUNED_DIR_NAMES = frozenset({".git", "node_modules", "vendor"})


def extract_message_ids(table: dict[str, Any]) -> tuple[set[str], dict[str, list[str]]]:
    """Return (leaf message ids, leaf id -> full dotted paths).

    Only non-table values produce message ids (the leaf key). A leaf defined
    under several different paths (e.g. two sections both carrying a key of
    the same name) makes the id ambiguous across languages; the caller
    treats that as an error. Full paths are kept purely to report such
    collisions precisely.
    """
    ids: set[str] = set()
    id_paths: dict[str, list[str]] = {}
    for key, value in table.items():
        if isinstance(value, dict):
            nested, nested_paths = extract_message_ids(value)
            ids |= nested
            for leaf, paths in nested_paths.items():
                id_paths.setdefault(leaf, []).extend(
                    f"{key}.{p}" for p in paths
                )
        else:
            ids.add(key)
            id_paths.setdefault(key, []).append(key)
    return ids, id_paths


def load_ids(path: str) -> tuple[set[str], dict[str, list[str]], str | None]:
    """Parse path with tomllib. Returns (ids, id_paths, error_message)."""
    try:
        with open(path, "rb") as fh:
            data = tomllib.load(fh)
    except OSError as exc:
        return set(), {}, f"cannot read: {exc}"
    except tomllib.TOMLDecodeError as exc:
        return set(), {}, f"invalid TOML: {exc}"
    ids, id_paths = extract_message_ids(data)
    return ids, id_paths, None


def check_locale_dir(dir_path: str, rel_dir: str) -> tuple[list[str], list[str]]:
    """Check one locale directory.

    Returns (problems, notes): problems make the run fail, notes are
    informational (success line, unpaired extra files). Both are printed in
    order so per-directory output stays readable.
    """
    problems: list[str] = []
    notes: list[str] = []
    missing = [fn for fn in PAIR if not os.path.exists(os.path.join(dir_path, fn))]
    if missing:
        problems.append(
            f"{rel_dir}: missing required locale file(s): {', '.join(missing)}"
        )
        return problems, notes
    extras = sorted(
        fn for fn in os.listdir(dir_path)
        if fn.endswith(".toml") and fn not in PAIR
    )
    for fn in extras:
        notes.append(
            f"{rel_dir}: note: {fn} is not part of the zh-CN/en-US pair and "
            f"is not checked (only zh-CN.toml vs en-US.toml key sets are)"
        )
    zh_path = os.path.join(dir_path, PAIR[0])
    en_path = os.path.join(dir_path, PAIR[1])
    per_file: dict[str, tuple[set[str], dict[str, list[str]], str | None]] = {}
    for fn, path in zip(PAIR, (zh_path, en_path)):
        per_file[fn] = load_ids(path)
    for fn in PAIR:
        _, _, err = per_file[fn]
        if err:
            problems.append(f"{os.path.join(rel_dir, fn)}: {err}")
            return problems, notes
    for fn in PAIR:
        _, id_paths, _ = per_file[fn]
        for leaf in sorted(id_paths):
            paths = sorted(id_paths[leaf])
            if len(paths) > 1:
                problems.append(
                    f"{os.path.join(rel_dir, fn)}: message id '{leaf}' is "
                    f"defined under multiple paths: {', '.join(paths)}"
                )
    if problems:
        return problems, notes
    zh_ids = per_file[PAIR[0]][0]
    en_ids = per_file[PAIR[1]][0]
    only_zh = sorted(zh_ids - en_ids)
    only_en = sorted(en_ids - zh_ids)
    if only_zh or only_en:
        problems.append(f"{rel_dir}: zh-CN.toml and en-US.toml key sets differ")
        if only_en:
            problems.append("  only in en-US.toml (missing zh-CN translation):")
            problems.extend(f"    - {k}" for k in only_en)
        if only_zh:
            problems.append("  only in zh-CN.toml (missing en-US translation):")
            problems.extend(f"    - {k}" for k in only_zh)
    else:
        notes.append(f"{rel_dir}: key sets match ({len(en_ids)} key(s))")
    return problems, notes


def discover_locale_dirs(root: str) -> list[str]:
    """Return repo-relative paths of directories containing a pair member.

    --root itself is a candidate; everything below it is walked for
    directories containing zh-CN.toml or en-US.toml, skipping VCS, vendored
    dependency and node_modules trees.
    """
    found: list[str] = []
    for dir_path, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(
            d for d in dirnames if d not in PRUNED_DIR_NAMES
        )
        if any(fn in PAIR for fn in filenames):
            found.append(os.path.relpath(dir_path, root))
    return sorted(found)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Check that zh-CN.toml and en-US.toml in every locale directory "
            "carry identical message-key sets, per root CLAUDE.md's "
            "internationalization rule. Locale directories are discovered "
            "below --root by the presence of either pair member."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="directory to search for locale directories (default: current "
        "directory); report paths are relative to it",
    )
    args = parser.parse_args(argv)
    root = os.path.abspath(args.root)
    if not os.path.isdir(root):
        print(f"error: --root is not a directory: {args.root}", file=sys.stderr)
        return 2

    locale_dirs = discover_locale_dirs(root)
    if not locale_dirs:
        print("no locale directories found (no zh-CN.toml/en-US.toml pair "
              "under the given root)")
        return 0

    all_lines: list[str] = []
    bad_pairs = 0
    for rel_dir in locale_dirs:
        dir_path = os.path.join(root, rel_dir)
        problems, notes = check_locale_dir(dir_path, rel_dir)
        if problems:
            bad_pairs += 1
        all_lines.extend(notes)
        all_lines.extend(problems)

    if all_lines:
        print("\n".join(all_lines))
    if bad_pairs:
        print(
            f"FAILED: {bad_pairs} of {len(locale_dirs)} locale pair(s) have "
            f"problems; every user-facing text must ship with both zh-CN "
            f"and en-US resources with identical key sets"
        )
        return 1
    print(f"OK: all {len(locale_dirs)} locale pair(s) checked, key sets match")
    return 0


if __name__ == "__main__":
    sys.exit(main())
