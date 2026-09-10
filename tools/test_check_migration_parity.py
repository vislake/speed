#!/usr/bin/env python3
"""Unit tests for check_migration_parity.py's rules.

Stdlib-only (unittest + tempfile), matching this directory's
conventions. Run directly:

    python3 tools/test_check_migration_parity.py

Every rule the gate enforces has a planted fixture here proving it
fires -- the drift this gate exists to catch is by construction absent
from the real tree (the tree passes clean), so the fixtures are the
rules' living proof, in the same shape as
test_check_migration_cross_module_fks.py:

  * an undeclared structural divergence fires; the pair stays silent the
    moment it is declared (with its reason);
  * a pair equal modulo comments, whitespace and the declared type
    synonyms stays silent -- the positive side proving the gate is not
    simply firing on every pair;
  * a file present on one dialect only fires, and stays silent when
    declared dialect-only;
  * a set carrying one dialect directory fires, and stays silent when
    declared a dialect-only set;
  * every stale declaration fires (a divergent pair that now matches, a
    dialect-only file that now has its sibling, a dialect-only set that
    now carries both directories);
  * the registry itself is validated: a missing file, an unknown key and
    an entry without a usable reason each fire.
"""

from __future__ import annotations

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_migration_parity as m  # noqa: E402

SQLITE_CREATE = (
    "-- a comment the postgres copy need not repeat\n"
    "CREATE TABLE widgets (\n"
    "    id      VARCHAR(36) NOT NULL,\n"
    "    payload BLOB,\n"
    "    ratio   REAL NOT NULL,\n"
    "    count   INTEGER NOT NULL,\n"
    "    PRIMARY KEY (id)\n"
    ");\n"
)
POSTGRES_EQUIVALENT = (
    "-- a different comment the sqlite copy need not repeat\n"
    "CREATE TABLE widgets (\n"
    "    id      VARCHAR(36)   NOT NULL,\n"
    "    payload BYTEA,\n"
    "    ratio   DOUBLE PRECISION NOT NULL,\n"
    "    count   BIGINT NOT NULL,\n"
    "    PRIMARY KEY (id)\n"
    ");\n"
)


def make_tree(
    files: dict[str, str],
    registry: dict | None = None,
    write_registry: bool = True,
) -> pathlib.Path:
    """Materialize a fixture repo tree {rel_path: sql_text} under a temp
    dir (plus the exceptions registry, empty by default) and return the
    temp root."""
    root = pathlib.Path(tempfile.mkdtemp(prefix="migration-parity-"))
    for (rel, text) in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    if write_registry:
        if registry is None:
            registry = {
                "dialect_only": [],
                "dialect_only_set": [],
                "divergent": [],
            }
        path = root / m.REGISTRY_REL_PATH
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(registry), encoding="utf-8")
    return root


def pair(sqlite: str, postgres: str, name: str = "0001_create_widgets.sql"):
    return {
        f"go/widgets/migrations/sqlite/{name}": sqlite,
        f"go/widgets/migrations/postgres/{name}": postgres,
    }


class ParityRules(unittest.TestCase):
    def test_equivalent_pair_stays_silent(self):
        # The positive side: the same logical schema, each dialect's own
        # type spellings, different comments and layout -- clean.
        root = make_tree(pair(SQLITE_CREATE, POSTGRES_EQUIVALENT))
        self.assertEqual(m.scan(root), [])

    def test_undeclared_divergence_fires(self):
        root = make_tree(
            pair(
                SQLITE_CREATE,
                POSTGRES_EQUIVALENT.replace("ratio   DOUBLE PRECISION NOT NULL,\n", ""),
            )
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("go/widgets/migrations/0001_create_widgets.sql", findings[0])
        self.assertIn("differ structurally", findings[0])

    def test_declared_divergence_stays_silent(self):
        root = make_tree(
            pair(
                SQLITE_CREATE,
                POSTGRES_EQUIVALENT.replace("ratio   DOUBLE PRECISION NOT NULL,\n", ""),
            ),
            registry={
                "dialect_only": [],
                "dialect_only_set": [],
                "divergent": [
                    {
                        "path": "go/widgets/migrations/0001_create_widgets.sql",
                        "reason": "fixture: the ratio column is deliberately sqlite-only",
                    }
                ],
            },
        )
        self.assertEqual(m.scan(root), [])

    def test_undeclared_spelling_difference_fires(self):
        # TEXT vs VARCHAR is NOT a declared synonym: the gate must not
        # wave through a spelling it does not understand.
        root = make_tree(
            pair(
                SQLITE_CREATE,
                POSTGRES_EQUIVALENT.replace(
                    "id      VARCHAR(36)   NOT NULL", "id      TEXT NOT NULL"
                ),
            )
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("differ structurally", findings[0])


class PairingRules(unittest.TestCase):
    def test_orphan_file_fires(self):
        root = make_tree(
            {
                "go/widgets/migrations/sqlite/0002_only_sqlite.sql": SQLITE_CREATE,
                "go/widgets/migrations/postgres/0001_create_widgets.sql": POSTGRES_EQUIVALENT,
                "go/widgets/migrations/sqlite/0001_create_widgets.sql": SQLITE_CREATE,
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn(
            "go/widgets/migrations/sqlite/0002_only_sqlite.sql", findings[0]
        )
        self.assertIn("sibling is missing", findings[0])

    def test_declared_dialect_only_stays_silent(self):
        only = "go/widgets/migrations/sqlite/0002_only_sqlite.sql"
        root = make_tree(
            {
                only: SQLITE_CREATE,
                "go/widgets/migrations/postgres/0001_create_widgets.sql": POSTGRES_EQUIVALENT,
                "go/widgets/migrations/sqlite/0001_create_widgets.sql": SQLITE_CREATE,
            },
            registry={
                "dialect_only": [
                    {"path": only, "reason": "fixture: sqlite needs no such change"}
                ],
                "dialect_only_set": [],
                "divergent": [],
            },
        )
        self.assertEqual(m.scan(root), [])

    def test_one_dialect_directory_fires(self):
        root = make_tree(
            {
                "go/pkgcore/thing/postgres/migrations/postgres/0001.sql": SQLITE_CREATE,
            }
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("carries no sqlite/ directory", findings[0])

    def test_declared_dialect_only_set_stays_silent(self):
        root = make_tree(
            {
                "go/pkgcore/thing/postgres/migrations/postgres/0001.sql": SQLITE_CREATE,
            },
            registry={
                "dialect_only": [],
                "dialect_only_set": [
                    {
                        "path": "go/pkgcore/thing/postgres/migrations",
                        "reason": "fixture: the backend is postgres-only",
                    }
                ],
                "divergent": [],
            },
        )
        self.assertEqual(m.scan(root), [])


class RegistryHonesty(unittest.TestCase):
    def test_stale_divergent_entry_fires(self):
        root = make_tree(
            pair(SQLITE_CREATE, POSTGRES_EQUIVALENT),
            registry={
                "dialect_only": [],
                "dialect_only_set": [],
                "divergent": [
                    {
                        "path": "go/widgets/migrations/0001_create_widgets.sql",
                        "reason": "fixture: a divergence that no longer exists",
                    }
                ],
            },
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("no longer", findings[0].replace("now passes", "no longer passes"))

    def test_stale_dialect_only_entry_fires(self):
        only = "go/widgets/migrations/sqlite/0002_only_sqlite.sql"
        root = make_tree(
            {
                only: SQLITE_CREATE,
                "go/widgets/migrations/postgres/0002_only_sqlite.sql": POSTGRES_EQUIVALENT,
            },
            registry={
                "dialect_only": [
                    {"path": only, "reason": "fixture: a sibling now exists"}
                ],
                "dialect_only_set": [],
                "divergent": [],
            },
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("has a same-named sibling", findings[0])

    def test_stale_dialect_only_set_entry_fires(self):
        root = make_tree(
            pair(SQLITE_CREATE, POSTGRES_EQUIVALENT),
            registry={
                "dialect_only": [],
                "dialect_only_set": [
                    {
                        "path": "go/widgets/migrations",
                        "reason": "fixture: the set now carries both dialects",
                    }
                ],
                "divergent": [],
            },
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("carries both dialect directories", findings[0])

    def test_missing_registry_fires(self):
        root = make_tree(pair(SQLITE_CREATE, POSTGRES_EQUIVALENT), write_registry=False)
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("is missing", findings[0])

    def test_unknown_registry_key_fires(self):
        root = make_tree(
            pair(SQLITE_CREATE, POSTGRES_EQUIVALENT),
            registry={
                "dialect_only": [],
                "dialect_only_set": [],
                "divergent": [],
                "whitelist": [],
            },
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("unknown key", findings[0])

    def test_entry_without_reason_fires(self):
        root = make_tree(
            pair(SQLITE_CREATE, POSTGRES_EQUIVALENT),
            registry={
                "dialect_only": [],
                "dialect_only_set": [],
                "divergent": [
                    {
                        "path": "go/widgets/migrations/0001_create_widgets.sql",
                        "reason": "  ",
                    }
                ],
            },
        )
        findings = m.scan(root)
        self.assertEqual(len(findings), 1)
        self.assertIn("reason", findings[0])


if __name__ == "__main__":
    unittest.main()
