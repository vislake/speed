#!/usr/bin/env python3
"""zh-CN / en-US locale message-key set consistency checker.

Root CLAUDE.md's internationalization rule: every user-facing text ships in
both zh-CN and en-US resources, and CI checks that the two key sets match
("New text must ship with both zh-CN and en-US resources; CI checks the key
sets match"). docs/internal/18-cicd.md schedules this as a self-written
discipline check diffing the key sets of the two resources; this script is
its local-run counterpart.

What it does: for every locale directory under --root, it parses the zh-CN
and en-US pair members it finds and reports every id present in one file
but not the other. Two resource formats are checked:

  - zh-CN.toml and en-US.toml (TOML in the go-i18n style used by the Go
    modules, e.g. examples/reference-app/internal/notes/locales/), whose
    ids are extracted with tomllib, and
  - zh-CN.json and en-US.json (the nested JSON bundles used by the web
    @speed/i18n packages, e.g. web/packages/ui-kit/src/locales/), whose
    ids are extracted with the standard json module.

Exit code is nonzero on any mismatch, missing pair member, or unparsable
file.

TOML message-id semantics (must stay in step with how the repository names
keys, and with the locale file contract of go/pkgcore/i18n): a message id
is a key whose value is not a grouping table. Two kinds of values make a
message id:

  - any non-table value (the plain single-form message), and
  - a table whose keys are all message keys -- zero, one, two, few, many,
    other (the CLDR plural categories), translation, description, id,
    hash, leftdelim and rightdelim, matched case-insensitively, the set
    go-i18n reserves. Such a table is ONE plural message: its categories
    and metadata are forms of that message, not separate ids, and the
    table's own key is the message id. A quoted header such as
    ["notes.over_quota"] with one/other entries and an inline
    "notes.over_quota" = { one = "...", other = "..." } both declare the
    single id notes.over_quota.

Quoted keys are TOML-native single keys, so a quoted dotted key like
"notes.text_required" stays whole and *is* the message id, exactly as
handler code references it. A table with any key outside the message set
is a grouping table ([errors], or an unquoted [notes.over_quota] section
carrying other tables); its message ids are its leaf keys, compared
leaf-wise, so a flat file and a grouped file declaring the same messages
match. Note that go/pkgcore/i18n's AddModule is deliberately stricter than
this script -- it rejects grouping sections outright with
ErrUnsupportedShape and enforces the "<module>." id prefix -- so repository
files must follow the flat contract; the script keeps leaf-level grouping
tolerance only so its id semantics stay stable across both shapes.
Defensive rule: if one file defines the same leaf id under two different
paths (two sections both containing a key of the same name), pairing
across languages is ambiguous, so that file is reported as an error rather
than compared.

JSON message-id semantics: a bundle is a plain record tree whose leaves
are strings (exactly the ResourceBundle shape @speed/i18n's
registerNamespace accepts -- anything else is an error, mirroring its
validation). A message id is the full dotted path from the bundle root to
a leaf, e.g. dataTable.loading for {"dataTable": {"loading": "..."}}:
that is the key t() references and the leaf path
registerNamespace's collectLeafPaths compares across languages. The JSON
shape is singular -- nesting is the only structure -- so the full path is
always unambiguous and no leaf-name ambiguity check applies.

Discovery: a locale directory is any directory that contains a zh-CN or
en-US file in either format (zh-CN.toml, en-US.toml, zh-CN.json or
en-US.json); --root itself is searched first, then the tree below it is
walked (skipping .git, node_modules, vendor and dist). A directory that
holds a pair member must hold its complete pair -- both zh-CN.toml and
en-US.toml for a TOML pair, both zh-CN.json and en-US.json for a JSON
pair. Any other *.toml/*.json in the same directory is reported as
informational only -- the repository's pair discipline is exactly
zh-CN/en-US, and additional languages, when they appear, should extend
this script's pair table rather than slip past it.

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
import json
import os
import sys
import tomllib
from typing import Any, Callable

TOML_PAIR = ("zh-CN.toml", "en-US.toml")
JSON_PAIR = ("zh-CN.json", "en-US.json")
ALL_PAIR_FILES = TOML_PAIR + JSON_PAIR
PRUNED_DIR_NAMES = frozenset({".git", "node_modules", "vendor", "dist"})

# The message keys go-i18n reserves on a plural message table, lower-cased
# to match case-insensitively: the CLDR plural categories zero/one/two/few/
# many/other, the v1 "translation" synonym for other, and the metadata keys
# description, id, hash, leftdelim and rightdelim. A table whose keys all
# fall in this set is one message (its key is the id); any table with a key
# outside it is grouping.
RESERVED_MESSAGE_KEYS = frozenset({
    "zero", "one", "two", "few", "many", "other", "translation",
    "description", "id", "hash", "leftdelim", "rightdelim",
})


def is_message_table(value: Any) -> bool:
    """Whether value is a plural message table, not a grouping table.

    A non-empty table whose keys all belong to RESERVED_MESSAGE_KEYS (case-
    insensitively) declares one message whose categories are its forms;
    every other table -- including one with a non-reserved key -- is
    grouping. Key membership alone decides: value types are go/pkgcore/i18n's
    AddModule concern (ErrUnsupportedShape), not a key-parity one.
    """
    return (
        isinstance(value, dict)
        and bool(value)
        and all(str(key).lower() in RESERVED_MESSAGE_KEYS for key in value)
    )


def extract_message_ids(table: dict[str, Any]) -> tuple[set[str], dict[str, list[str]]]:
    """Return (message ids, id -> full dotted paths) for a TOML document.

    Every key whose value is not a grouping table produces a message id --
    both the plain non-table form and a plural message table (whose own key
    is the id, e.g. "notes.over_quota" from ["notes.over_quota"] with
    one/other forms). Grouping tables are descended into and only their
    leaf ids count, so a flat file and a grouped file declaring the same
    messages compare equal. A leaf defined under several different paths
    (e.g. two sections both carrying a key of the same name) makes the id
    ambiguous across languages; the caller treats that as an error. Full
    paths are kept purely to report such collisions precisely.
    """
    ids: set[str] = set()
    id_paths: dict[str, list[str]] = {}
    for key, value in table.items():
        if is_message_table(value):
            ids.add(key)
            id_paths.setdefault(key, []).append(key)
        elif isinstance(value, dict):
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


def load_toml_ids(path: str) -> tuple[set[str], str | None]:
    """Parse path with tomllib. Returns (ids, error_message)."""
    try:
        with open(path, "rb") as fh:
            data = tomllib.load(fh)
    except OSError as exc:
        return set(), f"cannot read: {exc}"
    except tomllib.TOMLDecodeError as exc:
        return set(), f"invalid TOML: {exc}"
    ids, _ = extract_message_ids(data)
    return ids, None


def collect_json_leaves(
    value: dict[str, Any],
    prefix: str,
    leaves: set[str],
    problems: list[str],
) -> None:
    """Add full dotted leaf paths of a JSON bundle to leaves.

    Mirrors @speed/i18n registerNamespace's collectLeafPaths: leaves must
    be strings and nesting must be plain records; a non-string leaf or an
    array value is an error in that runtime validation, so it is one here
    too -- parity with the enforcement the packages themselves run.
    Problems carry no file prefix; the caller labels them per file.
    """
    for key, child in value.items():
        path = key if prefix == "" else f"{prefix}.{key}"
        if isinstance(child, str):
            leaves.add(path)
        elif isinstance(child, dict):
            collect_json_leaves(child, path, leaves, problems)
        elif isinstance(child, bool):
            problems.append(
                f"leaf '{path}' is a boolean; JSON bundle leaves must be strings"
            )
        elif isinstance(child, (int, float)):
            problems.append(
                f"leaf '{path}' is a number; JSON bundle leaves must be strings"
            )
        elif child is None:
            problems.append(
                f"leaf '{path}' is null; JSON bundle leaves must be strings"
            )
        else:
            problems.append(
                f"leaf '{path}' is an array; JSON bundle leaves must be strings"
            )


def load_json_ids(path: str) -> tuple[set[str], list[str]]:
    """Parse path with the json module. Returns (ids, problems)."""
    try:
        with open(path, "rb") as fh:
            data = json.load(fh)
    except OSError as exc:
        return set(), [f"cannot read: {exc}"]
    except json.JSONDecodeError as exc:
        return set(), [f"invalid JSON: {exc}"]
    if not isinstance(data, dict):
        return set(), ["the document is not a JSON object"]
    leaves: set[str] = set()
    problems: list[str] = []
    collect_json_leaves(data, "", leaves, problems)
    return leaves, problems


def check_one_pair(
    dir_path: str,
    rel_dir: str,
    pair: tuple[str, str],
    load: Callable[[str], tuple[set[str], list[str]]],
) -> tuple[list[str], list[str]]:
    """Check one zh-CN/en-US pair inside a locale directory.

    pair names the two file names (TOML or JSON); load parses one file
    into its message-id set plus per-file problems. Returns (problems,
    notes) with the same contract as check_locale_dir.
    """
    problems: list[str] = []
    notes: list[str] = []
    zh_fn, en_fn = pair
    missing = [fn for fn in pair if not os.path.exists(os.path.join(dir_path, fn))]
    if missing:
        problems.append(
            f"{rel_dir}: missing required locale file(s): {', '.join(missing)}"
        )
        return problems, notes
    files = (os.path.join(dir_path, zh_fn), os.path.join(dir_path, en_fn))
    per_file: list[tuple[set[str], list[str]]] = []
    for path in files:
        ids, file_problems = load(path)
        per_file.append((ids, file_problems))
        for problem in file_problems:
            problems.append(f"{os.path.relpath(path, dir_path)}: {problem}")
    if problems:
        return problems, notes
    zh_ids, en_ids = per_file[0][0], per_file[1][0]
    only_zh = sorted(zh_ids - en_ids)
    only_en = sorted(en_ids - zh_ids)
    # The file names already say which format a pair is, so the prose
    # stays format-neutral and the long-standing TOML wording is intact.
    if only_zh or only_en:
        problems.append(
            f"{rel_dir}: {zh_fn} and {en_fn} key sets differ"
        )
        if only_en:
            problems.append(
                f"  only in {en_fn} (missing zh-CN translation):"
            )
            problems.extend(f"    - {k}" for k in only_en)
        if only_zh:
            problems.append(
                f"  only in {zh_fn} (missing en-US translation):"
            )
            problems.extend(f"    - {k}" for k in only_zh)
    else:
        notes.append(f"{rel_dir}: key sets match ({len(en_ids)} key(s))")
    return problems, notes


def check_locale_dir(dir_path: str, rel_dir: str) -> tuple[list[str], list[str]]:
    """Check one locale directory for every pair format it participates in.

    A directory participates in the TOML pair when it contains either
    TOML member, and in the JSON pair when it contains either JSON member
    -- a directory could in principle ship both formats (Go and web
    resources never share a directory today, but each pair is checked
    independently so the script does not depend on that). Returns
    (problems, notes): problems make the run fail, notes are informational
    (success lines, unpaired extra files).
    """
    problems: list[str] = []
    notes: list[str] = []
    present = set(os.listdir(dir_path))
    for pair in (TOML_PAIR, JSON_PAIR):
        if not any(fn in present for fn in pair):
            continue
        pair_problems, pair_notes = check_one_pair(dir_path, rel_dir, pair, load_ids_for(pair))
        problems.extend(pair_problems)
        notes.extend(pair_notes)
        extension = pair[0].rsplit(".", 1)[1]
        extras = sorted(
            fn for fn in present
            if fn.endswith(f".{extension}") and fn not in pair
        )
        for fn in extras:
            notes.append(
                f"{rel_dir}: note: {fn} is not part of the zh-CN/en-US pair and "
                f"is not checked (only {pair[0]} vs {pair[1]} key sets are)"
            )
    return problems, notes


def load_ids_for(pair: tuple[str, str]) -> Callable[[str], tuple[set[str], list[str]]]:
    """The right loader for a pair: tomllib for TOML, json for JSON."""
    if pair == TOML_PAIR:
        def load_toml(path: str) -> tuple[set[str], list[str]]:
            ids, err = load_toml_ids(path)
            return ids, ([err] if err is not None else [])

        return load_toml
    return load_json_ids


def discover_locale_dirs(root: str) -> list[str]:
    """Return repo-relative paths of directories containing a pair member.

    --root itself is a candidate; everything below it is walked for
    directories containing a zh-CN or en-US file in either format,
    skipping VCS, vendored dependency, node_modules and dist trees.
    """
    found: list[str] = []
    for dir_path, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(
            d for d in dirnames if d not in PRUNED_DIR_NAMES
        )
        if any(fn in ALL_PAIR_FILES for fn in filenames):
            found.append(os.path.relpath(dir_path, root))
    return sorted(found)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Check that zh-CN.toml/en-US.toml and zh-CN.json/en-US.json in "
            "every locale directory carry identical message-key sets, per "
            "root CLAUDE.md's internationalization rule. Locale directories "
            "are discovered below --root by the presence of any pair member."
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
        print("no locale directories found (no zh-CN/en-US pair file under "
              "the given root)")
        return 0

    all_lines: list[str] = []
    bad_pairs = 0
    pair_count = 0
    for rel_dir in locale_dirs:
        dir_path = os.path.join(root, rel_dir)
        problems, notes = check_locale_dir(dir_path, rel_dir)
        if problems:
            bad_pairs += 1
        else:
            pair_count += 1
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
    print(f"OK: {pair_count} locale pair(s) checked, key sets match")
    return 0


if __name__ == "__main__":
    sys.exit(main())
