#!/usr/bin/env python3
"""Shared test-support helpers for the tools/ unittest suites.

Stdlib-only, like every script in this directory (tools/README.md's
"Running in CI and locally" section); not executable on its own, imported
by the suites that carry a planted-fixture tree:

    from testutil import make_tree

make_tree lived as four byte-identical private copies across
test_check_env_example_consistency.py, test_check_host_composition.py,
test_check_migration_cross_module_fks.py and
test_check_migration_parity.py (differing only in the mkdtemp prefix),
the same fork risk the checkers themselves exist to catch elsewhere. One
copy now; test_check_migration_parity.py wraps it to add the exception
registry its checker reads, which is that suite's own extra fixture, not
a second tree writer.
"""

from __future__ import annotations

import pathlib
import tempfile


def make_tree(
    files: dict[str, str], prefix: str = "tools-fixture-"
) -> pathlib.Path:
    """Materialize {repo-relative path: text} under a fresh temp dir and
    return the temp root. Files never need to compile for the checkers
    (they are textual scanners); the suites pass faithful shapes so the
    tests cannot pass for the wrong reason."""
    root = pathlib.Path(tempfile.mkdtemp(prefix=prefix))
    for rel, text in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return root
