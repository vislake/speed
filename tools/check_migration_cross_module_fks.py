#!/usr/bin/env python3
"""Migration checker: no cross-module foreign keys.

docs/internal/18-cicd.md's architecture-discipline table row "no
cross-module database foreign keys" (root CLAUDE.md: "Do not create
cross-module foreign keys. Store IDs only -- cross-module FKs make
independently released migrations and cascading deletes unmanageable")
is enforced here over every SQL migration this repository ships: a
REFERENCES target table inside a module's migration files must belong
to that same module -- measured by where the target table is CREATEd.
Today the tree carries no REFERENCES clause at all (the codebase stores
IDs only), so this checker is a future-proofing gate in the same class
as the gorm-automigrate-ban semgrep rule, and its planted-violation
fixtures are what prove it fires.

Ownership model (documented because the checker is a textual scanner,
not a SQL parser -- the same convention check_repo_isolation.py
documents for Go):

  * A migration file is any *.sql under go/ or examples/reference-app
    whose path contains a /migrations/ segment. Its owning module is
    the first path segment after go/ (go/dbkit/audit/migrations owns
    to go/dbkit -- audit is a subpackage of the dbkit module, one
    release unit) or the reference app for the examples tree. The
    dialect subdirectories (sqlite/, postgres/) and the modules'
    internal fixture trees (dbkit's migrationfixture, which models
    base/derived migration modules) are covered by the same rule.
  * Table ownership: a table belongs to every module whose migration
    files CREATE it (CREATE TABLE, IF NOT EXISTS included).
  * A REFERENCES target is a finding when the referencing file's own
    module is not among the target table's owners -- the conservative
    direction when a name is created by several modules: an in-module
    reference to an ambiguous name cannot be proven in-module, so it
    fires rather than silently passing.
  * A REFERENCES target that no migration anywhere creates (a
    pre-existing table a module migrates alongside) is not a finding:
    with no owner in this corpus, no cross-module relationship can be
    established from the migrations alone; code review owns that
    residual, recorded here rather than silently scanned.
  * SQL comments (-- to end of line, /* ... */ blocks) are stripped
    before matching, so prose explaining a reference is not read as
    one. Column-level REFERENCES, table-level FOREIGN KEY ... REFERENCES
    and REFERENCES inside CREATE TABLE all match the same clause.

Corpus: the walk starts at --root and skips .git/, node_modules/ and
vendor/ by basename, plus the exact repo-relative path .claude/worktrees/
-- this repository's git worktrees, complete checkouts whose own
migration trees are another checkout's corpus, never this one's
(gitignored local machine state that never exists in CI; the path is
matched exactly, so a directory merely named worktrees/ is walked).
Before that prune a worktree migration file reached this scan and made
owning_module refuse the path, crashing the whole check.

Usage:
    python3 tools/check_migration_cross_module_fks.py [--root DIR]

Exit codes: 0 = clean; 1 = at least one finding; 2 = usage error.
Standard library only, Python >= 3.11.
"""

from __future__ import annotations

import argparse
import os
import pathlib
import re
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

# The comment-stripping rule shared with check_migration_parity.py: one
# rule, one implementation (migration_sql.py). The two checkers' private
# copies had already forked on the line-comment replacement.
from migration_sql import strip_sql_comments  # noqa: E402

# Directories never descended into, matched by basename.
PRUNED_DIR_NAMES = frozenset({".git", "node_modules", "vendor", "__pycache__"})

# Directories never descended into that must be matched by exact
# repo-relative path rather than basename: see the module docstring's
# Corpus paragraph.
NON_SCANNED_DIR_PATHS = frozenset({".claude/worktrees"})

CREATE_TABLE = re.compile(
    r"\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"
    r"(?:[A-Za-z_][A-Za-z0-9_]*\.)?"
    r"[\"`]?([A-Za-z_][A-Za-z0-9_]*)",
    re.IGNORECASE,
)
REFERENCES = re.compile(
    r"\bREFERENCES\s+(?:[A-Za-z_][A-Za-z0-9_]*\.)?"
    r"[\"`]?([A-Za-z_][A-Za-z0-9_]*)",
    re.IGNORECASE,
)

# go/<module>/... owns to the first path segment; the examples tree
# owns to the reference app as a whole.
GO_ROOT = "go/"


def owning_module(rel_path: str) -> str:
    """The release module that owns a migration file path, given
    repo-root-relative (go/dbkit/audit/migrations/... -> go/dbkit;
    examples/reference-app/... -> examples/reference-app)."""
    parts = rel_path.split("/")
    if parts[0] == "go":
        return "go/" + parts[1]
    if parts[0] == "examples":
        return "examples/reference-app"
    raise ValueError("migration outside the source trees: %s" % rel_path)


def scan_tree(root: pathlib.Path) -> list[tuple[str, str, str]]:
    """Scan every migration file under root. Returns findings as
    (rel_path, line_hint, message)."""
    tables: dict[str, set[str]] = {}
    references: list[tuple[str, str, int]] = []  # (rel_path, table, line_no)
    for dirpath, dirnames, filenames in os.walk(root):
        rel_dir = os.path.relpath(dirpath, root)
        dirnames[:] = sorted(
            d for d in dirnames
            if d not in PRUNED_DIR_NAMES
            and os.path.normpath(os.path.join(rel_dir, d))
            not in NON_SCANNED_DIR_PATHS
        )
        for filename in sorted(filenames):
            if not filename.endswith(".sql"):
                continue
            candidate = pathlib.Path(dirpath) / filename
            rel = candidate.relative_to(root).as_posix()
            if "/migrations/" not in rel:
                continue
            module = owning_module(rel)
            text = strip_sql_comments(candidate.read_text(encoding="utf-8"))
            for line_no, line in enumerate(text.splitlines(), start=1):
                for match in CREATE_TABLE.finditer(line):
                    table = match.group(1).lower()
                    tables.setdefault(table, set()).add(module)
                for match in REFERENCES.finditer(line):
                    references.append(
                        (rel, match.group(1).lower(), line_no)
                    )

    findings: list[tuple[str, str, str]] = []
    for (rel, target, line_no) in references:
        module = owning_module(rel)
        owners = tables.get(target, set())
        if owners and module not in owners:
            findings.append(
                (
                    rel,
                    str(line_no),
                    "REFERENCES %s from %s, but %s is created only by %s "
                    "-- cross-module foreign keys make independently "
                    "released migrations and cascading deletes "
                    "unmanageable; store the ID only"
                    % (
                        target,
                        module,
                        target,
                        ", ".join(sorted(owners)),
                    ),
                )
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
    findings = scan_tree(root)
    for (rel, line, message) in findings:
        print("%s:%s: %s" % (rel, line, message))
    if findings:
        print(
            "check_migration_cross_module_fks: %d finding(s)"
            % len(findings),
            file=sys.stderr,
        )
        return 1
    print("check_migration_cross_module_fks: no cross-module foreign keys")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
