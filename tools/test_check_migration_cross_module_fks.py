#!/usr/bin/env python3
"""Unit tests for check_migration_cross_module_fks.py's rules.

Stdlib-only (unittest + tempfile), matching this directory's
conventions. Run directly:

    python3 tools/test_check_migration_cross_module_fks.py

The real tree carries no REFERENCES clause at all (this checker is a
future-proofing gate, in the gorm-automigrate-ban class), so the
planted-fixture suites below are the rules' only living proof:

  * A REFERENCES target created by a DIFFERENT module fires
    (test_cross_module_reference_fires).
  * A REFERENCES target created by the referencing file's own module
    stays silent (test_in_module_reference_stays_silent).
  * A REFERENCES target no migration creates anywhere stays silent --
    with no owner in the corpus no cross-module relationship can be
    established (test_unowned_target_stays_silent).
  * A REFERENCES mention inside a comment stays silent -- comments are
    stripped before scanning (test_comment_reference_stays_silent).
  * Subpackage migrations own to their module (audit tables are go/
    dbkit's, so a dbkit file referencing them is in-module;
    test_subpackage_ownership).
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_migration_cross_module_fks as m  # noqa: E402

CREATE_AUDIT = (
    "-- create audit_events\n"
    "CREATE TABLE audit_events (\n"
    "    id TEXT PRIMARY KEY\n"
    ");\n"
)
CREATE_USERS = (
    "CREATE TABLE IF NOT EXISTS users (\n"
    "    id TEXT PRIMARY KEY\n"
    ");\n"
)


def make_tree(files: dict[str, str]) -> pathlib.Path:
    """Materialize a fixture repo tree {rel_path: sql_text} under a
    temp dir and return the temp root."""
    root = pathlib.Path(tempfile.mkdtemp(prefix="fk-fixture-"))
    for (rel, text) in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return root


class FixtureRules(unittest.TestCase):
    def test_cross_module_reference_fires(self):
        root = make_tree(
            {
                "go/dbkit/audit/migrations/sqlite/0001.sql": CREATE_AUDIT,
                "go/authn/migrations/sqlite/0001.sql": (
                    "-- authn file\n"
                    "CREATE TABLE sessions (\n"
                    "    id TEXT PRIMARY KEY,\n"
                    "    audit_event_id TEXT REFERENCES audit_events(id)\n"
                    ");\n"
                ),
            }
        )
        findings = m.scan_tree(root)
        self.assertEqual(len(findings), 1)
        rel, line, message = findings[0]
        self.assertEqual(rel, "go/authn/migrations/sqlite/0001.sql")
        self.assertIn("audit_events", message)
        self.assertIn("go/dbkit", message)

    def test_in_module_reference_stays_silent(self):
        root = make_tree(
            {
                "go/authn/migrations/sqlite/0001.sql": CREATE_USERS,
                "go/authn/migrations/sqlite/0002.sql": (
                    "CREATE TABLE sessions (\n"
                    "    id TEXT PRIMARY KEY,\n"
                    "    user_id TEXT REFERENCES users(id)\n"
                    ");\n"
                ),
            }
        )
        self.assertEqual(m.scan_tree(root), [])

    def test_unowned_target_stays_silent(self):
        root = make_tree(
            {
                "go/authn/migrations/sqlite/0001.sql": (
                    "-- this database pre-exists outside the corpus\n"
                    "CREATE TABLE sessions (\n"
                    "    id TEXT PRIMARY KEY,\n"
                    "    legacy_id TEXT REFERENCES legacy_accounts(id)\n"
                    ");\n"
                ),
            }
        )
        self.assertEqual(m.scan_tree(root), [])

    def test_comment_reference_stays_silent(self):
        root = make_tree(
            {
                "go/dbkit/audit/migrations/sqlite/0001.sql": CREATE_AUDIT,
                "go/authn/migrations/sqlite/0001.sql": (
                    "-- note: sessions REFERENCES audit_events by design\n"
                    "CREATE TABLE sessions (\n"
                    "    id TEXT PRIMARY KEY\n"
                    ");\n"
                ),
            }
        )
        self.assertEqual(m.scan_tree(root), [])

    def test_subpackage_ownership(self):
        # audit tables live under go/dbkit/audit, a subpackage of the
        # dbkit module: a dbkit migration referencing them is in-module.
        root = make_tree(
            {
                "go/dbkit/audit/migrations/sqlite/0001.sql": CREATE_AUDIT,
                "go/dbkit/migrations/sqlite/0001.sql": (
                    "CREATE TABLE audit_backfill (\n"
                    "    id TEXT PRIMARY KEY,\n"
                    "    event_id TEXT REFERENCES audit_events(id)\n"
                    ");\n"
                ),
            }
        )
        self.assertEqual(m.scan_tree(root), [])

    def test_table_level_foreign_key_fires(self):
        root = make_tree(
            {
                "go/dbkit/audit/migrations/sqlite/0001.sql": CREATE_AUDIT,
                "go/org/migrations/sqlite/0001.sql": (
                    "CREATE TABLE org_nodes (\n"
                    "    id TEXT PRIMARY KEY,\n"
                    "    FOREIGN KEY (audit_event_id) "
                    "REFERENCES audit_events(id)\n"
                    ");\n"
                ),
            }
        )
        self.assertEqual(len(m.scan_tree(root)), 1)


class CommentStripping(unittest.TestCase):
    def test_block_comments_stripped(self):
        text = "/* REFERENCES audit_events(id) */\nCREATE TABLE x (id TEXT);\n"
        self.assertNotIn("REFERENCES", m.strip_sql_comments(text))

    def test_line_comments_stripped(self):
        text = "CREATE TABLE x (id TEXT); -- REFERENCES audit_events\n"
        cleaned = m.strip_sql_comments(text)
        self.assertNotIn("REFERENCES", cleaned)


if __name__ == "__main__":
    unittest.main()
