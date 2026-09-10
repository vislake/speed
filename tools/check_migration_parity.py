#!/usr/bin/env python3
"""Dual-dialect migration parity gate.

Every shipped migration set under this repository carries two hand-written
copies of each migration: migrations/sqlite/<name>.sql and
migrations/postgres/<name>.sql (dbkit.MigrationRegistry.Apply reads each
dialect's own subdirectory independently and has no cross-dialect pairing
requirement of its own). The two copies are authored separately, and the
drift between them is invisible to every other gate here: a column added
to one copy only compiles, applies and passes on the dialect its tests
happen to run -- until the other dialect's deployment hits the missing
column in production.

This gate makes the pairing rule and the parity rule machine-checked, with
the exceptions declared in one place:

  * Pairing -- every <name>.sql in one dialect directory must have a
    same-named sibling in the other, unless the file is declared
    dialect-only in tools/migration_parity_exceptions.json (a migration
    whose statement has no counterpart on the other dialect at all; it
    still carries its reason there).
  * Structural parity -- a paired file's SQL must be textually equal to
    its sibling's after comment removal, case folding, whitespace
    collapse and the TYPE_SYNONYMS canonicalization below
    (BLOB/BYTEA, REAL/DOUBLE PRECISION, INTEGER/BIGINT -- the only
    dialect spellings the tree's shipped pairs use for the same column
    type). A pair whose statements genuinely differ in shape (a SQLite
    table-rebuild recipe where PostgreSQL has an ALTER, a trigger written
    as a per-dialect construct) must be declared divergent in the
    exceptions file, with its reason.
  * Registry honesty -- every declared entry must still be live: a
    dialect-only entry whose sibling has since appeared, or a divergent
    entry whose pair now passes, is a stale entry and fails (the registry
    must not silently accumulate dead entries that could mask a future
    real divergence).
  * Structure -- a migration set must carry BOTH dialect directories; a
    set with one is the "forgot the other dialect" shape this gate
    exists to refuse (dbkit treats a missing dialect directory as zero
    migrations for it, so nothing else notices) -- unless the whole set
    is declared dialect-only in the registry (a backend that only exists
    on one dialect, such as pkgcore's PostgreSQL LISTEN/NOTIFY event
    bus, ships exactly one directory by design).

Why a checked parity rule and not a generator: applied migration files are
frozen history. MigrationRegistry.Apply records (module, filename) rows in
schema_migrations and skips an already-applied file by NAME, so editing an
already-applied file's SQL never reaches a database that ran it -- a
generator that rewrote the pairs would be a no-op on every existing
deployment while claiming single-source authorship. And the dialect
differences that legitimately exist are authorial intent, not a mechanical
transform (SQLite has no ALTER COLUMN TYPE or SET NOT NULL, so three
shipped pairs are whole table-rebuild recipes on one side; the audit
append-only trigger is a language-level construct with no shared
spelling), so a generator would have to carry the same per-pair exceptions
this registry carries -- with the extra failure mode of regenerating a
file that must never change. The tooling-side mechanism is therefore the
gate plus the authoring rule it enforces: write both copies, keep them
identical modulo the declared synonyms, and declare anything else here,
with a reason, in the same change that writes it.

Scan scope: every directory named migrations under go/ or examples/ (the
same corpus and ownership model tools/check_migration_cross_module_fks.py
documents), its sqlite/ and postgres/ subdirectories. The dbkit registry
fixtures (go/dbkit/internal/{migrationfixture,testutil}) ARE scanned --
they are the corpus tools/check_repo_isolation.py and friends already
walk; a fixture pair that drifts is the same defect in miniature. Skipped
by the walk: .git/, .claude/ (nested worktree checkouts), node_modules/,
vendor/, __pycache__/.

Usage:
    python3 tools/check_migration_parity.py [--root DIR]

Exit codes: 0 = every pair paired, parity-clean and the registry live;
1 = at least one finding; 2 = usage/infrastructure error (missing or
malformed exceptions registry, unreadable root).
Standard library only, Python >= 3.11.
"""

from __future__ import annotations

import argparse
import difflib
import json
import pathlib
import re
import sys

# The two dialect directory names every migration set carries, and the
# order findings are reported in.
DIALECTS = ("sqlite", "postgres")

# The column-type spellings the shipped pairs use interchangeably across
# the two dialects, canonicalized before comparison. The table is
# dialect-independent on purpose: the tree's real pairs split BLOB/BYTEA,
# REAL/DOUBLE PRECISION and INTEGER/BIGINT in BOTH directions (sqlite
# picks BIGINT where postgres writes INTEGER in several pairs, and vice
# versa), which is exactly the dual-dialect contract's accepted
# equivalence -- the pair declares one logical column type, and each
# dialect spells it with the type its engine actually offers. Adding a
# row here is a deliberate, reviewable widening of what "parity" accepts;
# any other spelling difference (TEXT vs VARCHAR, TIMESTAMP vs DATETIME,
# SMALLINT, NUMERIC) is a finding, so this table stays the complete list
# of accepted equivalences rather than a growing pile of conveniences.
# LONGEST FIRST where one pattern's text contains another's is not an
# issue here (every pattern is an anchored whole word), but
# DOUBLE PRECISION must precede the whitespace collapse below.
TYPE_SYNONYMS = (
    (r"\bBLOB\b", "bytes"),
    (r"\bBYTEA\b", "bytes"),
    (r"\bREAL\b", "float"),
    (r"\bDOUBLE\s+PRECISION\b", "float"),
    (r"\bINTEGER\b", "int"),
    (r"\bBIGINT\b", "int"),
)

# The registry file, relative to the repository root: the one place a
# deliberate exception (a dialect-only migration, a structurally divergent
# pair) is declared, with its reason.
REGISTRY_REL_PATH = "tools/migration_parity_exceptions.json"

# Directory names the walk never descends into.
SKIP_DIRS = {".git", ".claude", "node_modules", "vendor", "__pycache__"}

# Source trees carrying migration sets (mirrors
# check_migration_cross_module_fks.py's source-tree list).
SOURCE_TREES = ("go", "examples")

# Diff lines printed for an undeclared divergence, so the output is
# actionable without becoming a wall of text.
MAX_DIFF_LINES = 14


def strip_sql_comments(text: str) -> str:
    """Remove SQL comments (-- to end of line, /* ... */ blocks) so prose
    explaining a migration is never compared; string literals are not
    tracked, the same convention check_migration_cross_module_fks.py
    documents (no migration here embeds '--' or '/*' inside a literal)."""
    text = re.sub(r"/\*.*?\*/", " ", text, flags=re.DOTALL)
    return re.sub(r"--[^\n]*", "", text)


def canonical_statements(text: str) -> list[str]:
    """The canonical statement list for one migration file: comments
    stripped, lowercased, the declared type synonyms unified, each
    ;-terminated statement's whitespace collapsed. Comparing lists (not
    one blob) keeps the diff output readable when a pair diverges."""
    text = strip_sql_comments(text).lower()
    for pattern, replacement in TYPE_SYNONYMS:
        text = re.sub(pattern, replacement, text, flags=re.IGNORECASE)
    statements = []
    for raw in text.split(";"):
        collapsed = " ".join(raw.split())
        if collapsed:
            statements.append(collapsed + ";")
    return statements


def migration_sets(root: pathlib.Path) -> list[pathlib.Path]:
    """Every migrations/ directory under a source tree, as root-relative
    paths, sorted."""
    found: set[pathlib.Path] = set()
    for tree in SOURCE_TREES:
        base = root / tree
        if not base.is_dir():
            continue
        for candidate in base.rglob("migrations"):
            if not candidate.is_dir():
                continue
            rel = candidate.relative_to(root)
            # The skip set is matched against the root-relative parts: an
            # absolute path carries the checkout's own directories (a
            # worktree under .claude/ would otherwise skip the whole
            # tree), while the rule's intent is about directories INSIDE
            # the scanned corpus.
            if any(part in SKIP_DIRS for part in rel.parts):
                continue
            found.add(rel)
    return sorted(found)


def load_registry(root: pathlib.Path) -> tuple[dict, list[str]]:
    """Read and validate the exceptions registry. Returns (registry,
    findings); the findings cover a missing/malformed file and malformed
    entries, and an empty registry is used when the file is missing so the
    rest of the scan still runs."""
    path = root / REGISTRY_REL_PATH
    findings: list[str] = []
    if not path.is_file():
        return {"dialect_only": [], "dialect_only_set": [], "divergent": []}, [
            f"registry file {REGISTRY_REL_PATH} is missing: every declared "
            "exception lives there"
        ]
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return {"dialect_only": [], "dialect_only_set": [], "divergent": []}, [
            f"registry file {REGISTRY_REL_PATH} is unreadable: {exc}"
        ]
    registry: dict[str, list[dict]] = {
        "dialect_only": [],
        "dialect_only_set": [],
        "divergent": [],
    }
    for key in registry:
        entries = data.get(key)
        if not isinstance(entries, list):
            findings.append(
                f"{REGISTRY_REL_PATH}: {key} must be a JSON list"
            )
            continue
        for entry in entries:
            if not isinstance(entry, dict) or set(entry) != {"path", "reason"}:
                findings.append(
                    f"{REGISTRY_REL_PATH}: every {key} entry must carry "
                    'exactly "path" and "reason"'
                )
                continue
            reason = entry["reason"]
            if not isinstance(reason, str) or not reason.strip() or "\n" in reason:
                findings.append(
                    f"{REGISTRY_REL_PATH}: {entry['path']} needs a "
                    "non-empty single-line reason"
                )
                continue
            registry[key].append(entry)
    extra = set(data) - set(registry)
    for key in sorted(extra):
        findings.append(
            f"{REGISTRY_REL_PATH}: unknown key {key!r} (valid: "
            "dialect_only, dialect_only_set, divergent)"
        )
    # A path may be declared once, in exactly one of the lists.
    seen: dict[str, str] = {}
    for key, entries in registry.items():
        for entry in entries:
            path_value = entry["path"]
            if path_value in seen:
                findings.append(
                    f"{REGISTRY_REL_PATH}: {path_value} is declared twice "
                    f"({seen[path_value]} and {key})"
                )
            seen[path_value] = key
    return registry, findings


def scan(root: pathlib.Path) -> list[str]:
    """Walk every migration set under root and return the findings."""
    registry, findings = load_registry(root)

    dialect_only = {e["path"]: e["reason"] for e in registry["dialect_only"]}
    dialect_only_set = {
        e["path"]: e["reason"] for e in registry["dialect_only_set"]
    }
    divergent = {e["path"]: e["reason"] for e in registry["divergent"]}
    live_dialect_only: set[str] = set()
    live_dialect_only_set: set[str] = set()
    live_divergent: set[str] = set()

    for rel_set in migration_sets(root):
        set_dir = root / rel_set
        missing_dirs = [d for d in DIALECTS if not (set_dir / d).is_dir()]
        if missing_dirs:
            if rel_set.as_posix() in dialect_only_set:
                live_dialect_only_set.add(rel_set.as_posix())
                continue
            findings.append(
                f"{rel_set.as_posix()}: carries no "
                f"{', '.join(missing_dirs)}/ directory -- a migration set "
                "must ship both dialects; dbkit treats a missing dialect "
                "directory as zero migrations for it, so only this gate "
                "notices. A set that is single-dialect by design is "
                f"declared dialect-only in {REGISTRY_REL_PATH}"
            )
            continue

        names: set[str] = set()
        for dialect in DIALECTS:
            for sql_file in sorted((set_dir / dialect).glob("*.sql")):
                names.add(sql_file.name)

        for name in sorted(names):
            pair_rel = (rel_set / name).as_posix()
            per_dialect = {
                d: (set_dir / d / name) for d in DIALECTS
            }
            present = {
                d: p for d, p in per_dialect.items() if p.is_file()
            }
            if len(present) == 1:
                dialect, path_only = next(iter(present.items()))
                only_rel = path_only.relative_to(root).as_posix()
                if only_rel in dialect_only:
                    live_dialect_only.add(only_rel)
                else:
                    other = DIALECTS[0] if dialect == DIALECTS[1] else DIALECTS[1]
                    findings.append(
                        f"{only_rel}: exists only in {dialect}/ -- its "
                        f"{other}/ sibling is missing; write it, or declare "
                        f"the migration dialect-only in {REGISTRY_REL_PATH} "
                        "with its reason"
                    )
                continue
            if len(present) == 0:
                continue

            statements = {
                d: canonical_statements(per_dialect[d].read_text(encoding="utf-8"))
                for d in DIALECTS
            }
            if statements["sqlite"] == statements["postgres"]:
                continue
            if pair_rel in divergent:
                live_divergent.add(pair_rel)
                continue
            diff = list(
                difflib.unified_diff(
                    statements["sqlite"],
                    statements["postgres"],
                    fromfile=f"{rel_set.as_posix()}/sqlite/{name}",
                    tofile=f"{rel_set.as_posix()}/postgres/{name}",
                    lineterm="",
                )
            )
            shown = "\n      ".join(diff[:MAX_DIFF_LINES])
            if len(diff) > MAX_DIFF_LINES:
                shown += "\n      ... (diff truncated)"
            findings.append(
                f"{pair_rel}: the two dialect copies differ structurally "
                "after comment removal, case folding, whitespace collapse "
                "and the declared type synonyms; keep them identical, or "
                f"declare the pair divergent in {REGISTRY_REL_PATH} with "
                f"its reason. Diff (sqlite -> postgres):\n      {shown}"
            )

    # Registry honesty: every declared entry must still be live.
    for set_rel, reason in sorted(dialect_only_set.items()):
        if set_rel in live_dialect_only_set:
            continue
        set_path = root / set_rel
        if set_path.is_dir():
            findings.append(
                f"{REGISTRY_REL_PATH}: {set_rel} is declared a dialect-only "
                "set but now carries both dialect directories -- the entry "
                f"is stale; remove it (reason on file: {reason})"
            )
        else:
            findings.append(
                f"{REGISTRY_REL_PATH}: {set_rel} is declared a dialect-only "
                "set but no such directory exists -- the entry is stale or "
                "misspelled; remove or fix it"
            )
    for only_rel, reason in sorted(dialect_only.items()):
        if only_rel in live_dialect_only:
            continue
        if (root / only_rel).is_file():
            findings.append(
                f"{REGISTRY_REL_PATH}: {only_rel} is declared dialect-only "
                "but now has a same-named sibling in the other dialect -- "
                "the entry is stale; remove it (reason on file: "
                f"{reason})"
            )
        else:
            findings.append(
                f"{REGISTRY_REL_PATH}: {only_rel} is declared dialect-only "
                "but no such file exists -- the entry is stale or "
                "misspelled; remove or fix it"
            )
    for pair_rel, reason in sorted(divergent.items()):
        if pair_rel in live_divergent:
            continue
        sqlite_rel = pair_rel.replace("/migrations/", "/migrations/sqlite/")
        postgres_rel = pair_rel.replace(
            "/migrations/", "/migrations/postgres/"
        )
        if (root / sqlite_rel).is_file() and (root / postgres_rel).is_file():
            findings.append(
                f"{REGISTRY_REL_PATH}: {pair_rel} is declared divergent but "
                "the pair now passes the parity rule -- the entry is stale; "
                f"remove it (reason on file: {reason})"
            )
        else:
            findings.append(
                f"{REGISTRY_REL_PATH}: {pair_rel} is declared divergent but "
                "the pair's files are not both present -- the entry is "
                "stale or misspelled; remove or fix it"
            )
    return findings


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--root",
        default=None,
        help="repository root (default: the tools/ directory's parent)",
    )
    args = parser.parse_args(argv)
    root = (
        pathlib.Path(args.root).resolve()
        if args.root
        else pathlib.Path(__file__).resolve().parent.parent
    )
    if not root.is_dir():
        print(f"check_migration_parity: --root is not a directory: {root}",
              file=sys.stderr)
        return 2
    try:
        findings = scan(root)
    except OSError as exc:
        print(f"check_migration_parity: {exc}", file=sys.stderr)
        return 2
    for finding in findings:
        print(finding)
    if findings:
        print(
            "check_migration_parity: %d finding(s)" % len(findings),
            file=sys.stderr,
        )
        return 1
    print("check_migration_parity: every dialect pair paired and parity-clean")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
