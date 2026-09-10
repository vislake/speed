#!/usr/bin/env python3
"""Unit tests for semgrep_fixture_check.py's env-literal reader.

Stdlib-only (unittest), matching this directory's conventions. Run
directly:

    python3 tools/test_semgrep_fixture_check.py

The pure core under test is env_literals_read: which environment-variable
names a Go file counts as reading, for the fixture-liveness expectation
(a rule's positive fixture must exercise an env name some real file still
reads). The suite pins the shapes that expectation's docstring promises --
a direct string argument, a same-file single const, a same-file const
declared in a block -- plus the block-parsing case a paren in a comment
used to break:

  * test_direct_literal_argument -- os.Getenv("APP_X") is the literal.
  * test_const_single_and_block -- a const, however it is declared, is
    resolved to its value.
  * test_comment_paren_does_not_truncate_const_block -- a const block
    whose comment contains a paren must still yield every entry below it;
    the reader used to end the block at that paren and report NOTHING for
    a read that goes through an entry declared after it.
  * test_non_literal_argument_is_not_a_read -- an argument that is
    neither a literal nor a same-file const contributes no name.
  * test_loader_tag_pins_are_reads -- a `config:"env=NAME"` struct tag
    counts as a read in either Go string spelling, with a loader option
    after the name or without.
  * test_non_config_tags_are_not_env_reads -- json tags and non-env
    config tag options contribute no name.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import semgrep_fixture_check  # noqa: E402


class EnvLiteralsReadTest(unittest.TestCase):
    def test_direct_literal_argument(self):
        self.assertEqual(
            semgrep_fixture_check.env_literals_read(
                'package p\n\nfunc f() string { return os.Getenv("APP_DIRECT") }\n'
            ),
            {"APP_DIRECT"},
        )

    def test_const_single_and_block(self):
        self.assertEqual(
            semgrep_fixture_check.env_literals_read(
                "package p\n\n"
                'const singleEnv = "APP_SINGLE"\n\n'
                "const (\n"
                '\tblockEnv = "APP_BLOCK"\n'
                ")\n\n"
                "func f() (string, string) { return os.Getenv(singleEnv), os.LookupEnv(blockEnv) }\n"
            ),
            {"APP_SINGLE", "APP_BLOCK"},
        )

    def test_comment_paren_does_not_truncate_const_block(self):
        # The regression: a paren inside a block comment ended the block
        # early, so blockEnv's value never entered the const map and a read
        # through it reported no env name at all.
        self.assertEqual(
            semgrep_fixture_check.env_literals_read(
                "package p\n\n"
                "const (\n"
                "\t// defaultPort is the port a host listens on when no\n"
                "\t// environment variable names one (see the docs).\n"
                '\tdefaultPort = "8080"\n'
                "\n"
                '\tdeploymentModeEnv = "APP_DEPLOYMENT_MODE"\n'
                ")\n\n"
                "func f() string { return os.Getenv(deploymentModeEnv) }\n"
            ),
            {"APP_DEPLOYMENT_MODE"},
        )

    def test_non_literal_argument_is_not_a_read(self):
        self.assertEqual(
            semgrep_fixture_check.env_literals_read(
                "package p\n\n"
                "func f(name string) string {\n"
                "\tvalue := os.Getenv(name)\n"
                "\treturn value\n"
                "}\n"
            ),
            set(),
        )

    def test_loader_tag_pins_are_reads(self):
        self.assertEqual(
            semgrep_fixture_check.env_literals_read(
                "package p\n\n"
                "type hostConfig struct {\n"
                '\tMode string `config:"env=APP_DEPLOYMENT_MODE"`\n'
                '\tAlt string "config:\\"env=APP_ALT_MODE\\""\n'
                '\tOpt string `config:"env=APP_OPT_MODE,derive"`\n'
                "}\n"
            ),
            {"APP_DEPLOYMENT_MODE", "APP_ALT_MODE", "APP_OPT_MODE"},
        )

    def test_non_config_tags_are_not_env_reads(self):
        self.assertEqual(
            semgrep_fixture_check.env_literals_read(
                "package p\n\n"
                "type hostConfig struct {\n"
                '\tA string `json:"env=APP_JSON"`\n'
                '\tB string `config:"default=8080"`\n'
                "}\n"
            ),
            set(),
        )


if __name__ == "__main__":
    unittest.main()
