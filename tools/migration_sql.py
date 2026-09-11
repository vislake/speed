#!/usr/bin/env python3
"""Shared SQL-text helper for the two migration checkers.

check_migration_parity.py (dual-dialect pair comparison) and
check_migration_cross_module_fks.py (cross-module foreign-key lint) both
read migration files as text and must strip SQL comments before matching
clauses. The stripping rule is one rule, so it lives here once: the two
checkers carried private copies that had already forked (block comments
replaced by a space in both, line comments by an empty string in one and
by a space in the other) -- the exact class of silent divergence the
checkers exist to catch elsewhere.

The canonical rule, both checkers' original documented convention:
`/* ... */` blocks (DOTALL) and `--` line comments are replaced by a
single space, so removing a comment never glues two tokens together.
String literals are not tracked: no migration in this repository embeds
'--' or '/*' inside a literal (recorded in both checkers' docstrings).

Standard library only, Python >= 3.11 (the tools/ floor).
"""

from __future__ import annotations

import re


def strip_sql_comments(text: str) -> str:
    """Remove SQL comments (-- to end of line, /* ... */ blocks) so prose
    explaining a migration is never compared or matched as a clause."""
    text = re.sub(r"/\*.*?\*/", " ", text, flags=re.DOTALL)
    return re.sub(r"--[^\n]*", " ", text)
