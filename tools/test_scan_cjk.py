#!/usr/bin/env python3
"""Unit tests for scan_cjk.py's resource-directory and fixture carve-outs.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section). Run directly:

    python3 tools/test_scan_cjk.py

Regression coverage: resource directories are judged by CONTENT, never
by directory BASENAME (locales, locale, i18n, translations) -- a
directory is a resource directory only when it really holds a zh-CN.* /
en-US.* file, the same discovery rule check_i18n_keys.py applies -- so
i18n-named source trees are scanned like any other tree:
go/pkgcore/i18n and web/packages/i18n are real Go/TS source trees and
are scanned (test_i18n_named_source_dir_is_scanned) while genuine
locale bundles stay exempt (test_locale_bundle_directory_stays_exempt).
A second carve-out covers the one fixture-in-comment shape the Go
toolchain forces: a godoc Example's expected output is spelled as
comments after an "Output:" marker, and CI compiles and runs every
Example, so an Example demonstrating zh-CN rendering carries Chinese
text in comment syntax
(test_godoc_output_block_is_exempt_but_other_comments_are_not).
A third carve-out is machine-local state: the .claude/worktrees/ subtree
holds this repository's git worktrees -- complete checkouts, each
carrying its own docs/internal/ Chinese content -- and is pruned by its
exact repo-relative path, so a worktree's planted Chinese file is not
reported while a directory merely NAMED worktrees/ and the rest of
.claude/ (.claude/skills/** included) stay scanned
(test_repository_worktree_subtree_is_pruned,
test_claude_skills_tree_is_still_scanned,
test_worktrees_named_dir_outside_claude_is_still_scanned).

Fixture text below is spelled with \\u escapes so this test file itself
stays ASCII: tools/ are scanned as full text by the very scanner under
test, and only the files it WRITES to its temporary trees may carry CJK.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import scan_cjk as m  # noqa: E402

# CJK fixture fragments, spelled with \u escapes for the reason in the
# module docstring. _ZH_COMMENT is a plain Chinese comment (always a
# violation); the other fragments are zh-CN bundle text, README prose and
# godoc Example expected output (violations only outside their carve-outs).
_ZH_CN_TOML = '\u4f60\u597d,{{.Name}}!\n'  # a zh-CN bundle line
_ZH_JSON = '{"welcome": "\u6b22\u8fce"}\n'  # a zh-CN.json bundle line
_ZH_NOTE = '\u5171 1 \u6761\u5907\u6ce8\u3002\n'  # godoc Example output
_ZH_COMMENT = '\u4e2d\u6587\u6ce8\u91ca'  # a plain Chinese comment
_ZH_PROSE = '\u540e\u4e00\u6bb5\u6ce8\u91ca'  # a later, separate comment
_ZH_README = 'bilingual catalogs \u4e2d\u6587\u8bf4\u660e\n'  # README prose
_ZH_INTERNAL = '\u5185\u90e8\u8bbe\u8ba1\u6587\u6863\n'  # docs/internal doc text


class ScanRootTests(unittest.TestCase):
    def _scan(self, files: dict[str, str]) -> int:
        with tempfile.TemporaryDirectory() as td:
            for rel, content in files.items():
                path = pathlib.Path(td) / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(content, encoding="utf-8")
            return m.scan_root(td)

    def test_i18n_named_source_dir_is_scanned(self):
        # A directory NAMED i18n holding no zh-CN.*/en-US.* file is a
        # plain source tree (the shape of go/pkgcore/i18n and
        # web/packages/i18n): a CJK comment planted in its Go source is a
        # violation. Fails before the content-based resource-dir rule.
        code = (
            "package i18n\n\n"
            "// ErrExample reports a missing key.\n"
            "// " + _ZH_COMMENT + " here is a violation.\n"
            "var ErrExample = errors.New(\"x\")\n"
        )
        self.assertEqual(
            self._scan({"pkg/i18n/catalog.go": code}), 1
        )

    def test_locale_bundle_directory_stays_exempt(self):
        # A directory that really holds zh-CN/en-US pair files is a
        # resource directory whatever it is called: its bundle text and
        # everything beside it stay exempt, and the surrounding source
        # tree still scans clean.
        files = {
            "mod/locales/zh-CN.toml": '"mod.greeting" = "' + _ZH_CN_TOML,
            "mod/locales/en-US.toml": '"mod.greeting" = "Hello, {{.Name}}!"\n',
            "mod/locales/README.md": _ZH_README,
            "mod/catalog.go": "package mod\n",
        }
        self.assertEqual(self._scan(files), 0)

    def test_flat_locale_dir_under_i18n_named_parent_is_exempt(self):
        # web/packages/i18n's own test fixtures nest a real locale
        # directory (test-utils/locales/welcome/zh-CN.json) under a tree
        # whose PARENT directories are named locales/i18n but hold no pair
        # files themselves: exemption must follow the content, so only the
        # directory actually holding the pair file is pruned.
        files = {
            "web/i18n/src/catalog.ts": "export const k = 1;\n",
            "web/i18n/test-utils/locales/welcome/zh-CN.json": _ZH_JSON,
            "web/i18n/test-utils/locales/welcome/en-US.json":
                '{"welcome": "Welcome"}\n',
        }
        self.assertEqual(self._scan(files), 0)

    def test_godoc_output_block_is_exempt_but_other_comments_are_not(self):
        # A godoc Example's expected output is comment text by toolchain
        # requirement; an Example demonstrating zh-CN rendering therefore
        # spells Chinese output under an "Output:" marker (the
        # go/pkgcore/i18n/example_test.go shape). Those lines are exempt;
        # a CJK comment elsewhere in the same file is still a violation.
        example = (
            "package i18n\n\n"
            "func Example() {\n"
            "\t// Output:\n"
            "\t// Note text must not be empty.\n"
            "\t// " + _ZH_NOTE +
            "}\n"
        )
        self.assertEqual(
            self._scan({"pkg/i18n/example_test.go": example}), 0
        )
        violating = (
            "package i18n\n\n"
            "func Example() {\n"
            "\t// Output:\n"
            "\t// " + _ZH_NOTE +
            "}\n\n"
            "// " + _ZH_COMMENT + " outside the Output block is a violation.\n"
            "var _ = 0\n"
        )
        self.assertEqual(
            self._scan({"pkg/i18n/example_test.go": violating}), 1
        )

    def test_output_marker_without_contiguous_block_exempts_nothing(self):
        # The Output-block exemption applies only to the contiguous comment
        # run after the marker: a blank line ends the run, so a later CJK
        # comment is a violation.
        code = (
            "package i18n\n\n"
            "func Example() {\n"
            "\t// Output:\n"
            "\t// " + _ZH_NOTE +
            "\n"
            "\t// " + _ZH_PROSE + " is its own comment, not expected output.\n"
            "}\n"
        )
        self.assertEqual(self._scan({"pkg/i18n/x_test.go": code}), 1)

    def test_repository_worktree_subtree_is_pruned(self):
        # A git worktree of this repository under .claude/worktrees/ is a
        # complete checkout carrying its own docs/internal/ Chinese
        # content under a path the root-relative carve-out cannot reach,
        # and it is gitignored machine-local state that never exists in
        # CI. The whole subtree is pruned by its exact repo-relative
        # path, so this planted worktree docs/internal file must not be
        # reported. Fails before the exact-path prune: the nested Chinese
        # text is then scanned as a plain text file and flagged.
        files = {
            ".claude/worktrees/w/docs/internal/zh.md": _ZH_INTERNAL,
        }
        self.assertEqual(self._scan(files), 0)

    def test_claude_skills_tree_is_still_scanned(self):
        # The prune stops at .claude/worktrees/: the rest of .claude/ --
        # .claude/skills/** in particular -- is scanned source under the
        # same rules as the rest of the tree, so a CJK file there is a
        # violation.
        files = {
            ".claude/skills/example/SKILL.md": _ZH_README,
        }
        self.assertEqual(self._scan(files), 1)

    def test_worktrees_named_dir_outside_claude_is_still_scanned(self):
        # The prune matches the exact repo-relative path
        # ".claude/worktrees", never the basename: a directory merely
        # NAMED worktrees/ elsewhere in the tree is ordinary source, so
        # CJK content in it stays a violation.
        files = {
            "src/worktrees/zh.md": _ZH_README,
        }
        self.assertEqual(self._scan(files), 1)


if __name__ == "__main__":
    unittest.main()
