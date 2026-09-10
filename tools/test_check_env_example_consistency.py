#!/usr/bin/env python3
"""Unit tests for check_env_example_consistency.py's reading and gating
rules.

Stdlib-only (unittest + tempfile), matching this directory's
conventions. Run directly:

    python3 tools/test_check_env_example_consistency.py

The real tree passes the gate clean, so these planted fixtures are the
rules' living proof, in the same shape as the sibling checker suites:

  * a host declaration and an example documenting exactly those keys
    stay silent -- the positive side, proving the gate is not simply
    firing on everything;
  * a host pin the example documents no entry for fires, naming the
    pin's declaration line in bootstrap.go;
  * an example key the host never declares fires, naming the key's
    line in the example;
  * both key spellings the file uses count -- an active ``KEY=`` line
    and a commented-out example, the indented variant included -- while
    a prose mention of a name and a repeated key are not separate
    entries;
  * bootstrap.go's derivation is read from both declaration forms: the
    env pin (the ``,derive`` option included) and the WithRootKeyEnv
    literal, while a comment that merely names a variable declares
    nothing;
  * a missing side of the comparison (bootstrap.go or the example)
    fires with the missing path named;
  * a final case runs the gate against this repository, so the real
    tree's consistency is asserted here as it is in CI.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_env_example_consistency as m  # noqa: E402

BOOTSTRAP = '''package app

import "example.com/pkgcore/config"

// hostConfig is the loader target. The comment mentioning
// WithRootKeyEnv("APP_DOC_ONLY") declares nothing; the real wiring is
// at the load call below.
type hostConfig struct {
	// DeploymentMode names APP_DEPLOYMENT_MODE.
	DeploymentMode string `config:"env=APP_DEPLOYMENT_MODE"`

	// Port names PORT, the one unprefixed variable.
	Port string `config:"env=PORT"`

	// S3Region names APP_S3_REGION.
	S3Region string `config:"env=APP_S3_REGION"`

	// Cipher_Key names APP_CONFIG_KEY.
	Cipher_Key []byte `config:"env=APP_CONFIG_KEY,derive"`
}

func loadHostConfig() (hostConfig, error) {
	hc := hostConfigDefaults()
	err := config.New(
		config.WithEnvPrefix("APP_"),
		config.WithRootKeyEnv("APP_ROOT_KEY"),
	).Load(&hc)
	return hc, err
}
'''

EXAMPLE = '''# Environment variables the reference app's bootstrap resolves;
# the loader-driven surface of internal/app/bootstrap.go.

# Deployment mode: "standalone" (default) or "distributed".
APP_DEPLOYMENT_MODE=standalone

# HTTP listen port. Defaults to 8080 when unset.
PORT=8080

# The S3 composition; the region matters to AWS S3 and Aliyun OSS:
#   APP_S3_REGION=us-east-1
#
# A prose line that names APP_CONFIG_KEY without an entry is not an
# entry.

# Key material: set APP_ROOT_KEY alone to derive the rest.
APP_ROOT_KEY=REPLACE_WITH_64_HEX_CHARS

APP_CONFIG_KEY=
'''


def make_tree(files: dict[str, str]) -> pathlib.Path:
    root = pathlib.Path(
        tempfile.mkdtemp(prefix="env-example-consistency-")
    )
    for rel, text in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return root


def base_files(bootstrap: str = BOOTSTRAP, example: str = EXAMPLE):
    return {
        m.BOOTSTRAP_REL_PATH: bootstrap,
        m.ENV_EXAMPLE_REL_PATH: example,
    }


class KeySetComparison(unittest.TestCase):
    def test_matching_sets_stay_silent(self):
        # Covers both declaration forms (the pins, the `,derive` option
        # and the WithRootKeyEnv literal) and both example spellings
        # (active entries and the indented commented-out example).
        self.assertEqual(m.scan(make_tree(base_files())), [])

    def test_host_key_missing_from_the_example_fires(self):
        example = EXAMPLE.replace("#   APP_S3_REGION=us-east-1\n", "")
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn(m.BOOTSTRAP_REL_PATH, findings[0])
        self.assertIn("pins APP_S3_REGION", findings[0])
        self.assertIn(m.ENV_EXAMPLE_REL_PATH, findings[0])

    def test_root_key_missing_from_the_example_fires(self):
        # APP_ROOT_KEY is no struct field; only the WithRootKeyEnv
        # literal declares it, and its absence from the example must
        # fire like any pinned variable's.
        example = EXAMPLE.replace(
            "APP_ROOT_KEY=REPLACE_WITH_64_HEX_CHARS\n", ""
        )
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("pins APP_ROOT_KEY", findings[0])

    def test_unknown_example_key_fires(self):
        example = EXAMPLE + "\nAPP_NOT_A_HOST_VARIABLE=1\n"
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn(f"{m.ENV_EXAMPLE_REL_PATH}:", findings[0])
        self.assertIn("documents APP_NOT_A_HOST_VARIABLE", findings[0])
        self.assertIn("never declares", findings[0])

    def test_missing_bootstrap_fires(self):
        files = base_files()
        del files[m.BOOTSTRAP_REL_PATH]
        findings = m.scan(make_tree(files))
        self.assertEqual(len(findings), 1)
        self.assertIn(m.BOOTSTRAP_REL_PATH, findings[0])

    def test_missing_example_fires(self):
        files = base_files()
        del files[m.ENV_EXAMPLE_REL_PATH]
        findings = m.scan(make_tree(files))
        self.assertEqual(len(findings), 1)
        self.assertIn(m.ENV_EXAMPLE_REL_PATH, findings[0])


class ReadingRules(unittest.TestCase):
    def test_prose_mention_does_not_document_a_key(self):
        # Drop the active APP_CONFIG_KEY= entry but keep the prose line
        # naming it: the prose explicitly says it is not an entry, and
        # the pin must still be reported missing.
        example = EXAMPLE.replace("APP_CONFIG_KEY=\n", "")
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("pins APP_CONFIG_KEY", findings[0])

    def test_repeated_example_entries_are_one_key(self):
        # A commented-out example and an active entry for the same key
        # (the file's own shape for the AI-gateway pair) are one
        # documented key, and a repeated unknown key fires once, at its
        # first line.
        example = EXAMPLE + (
            "\n# APP_NOT_A_HOST_VARIABLE=1\nAPP_NOT_A_HOST_VARIABLE=1\n"
        )
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        first_line = example.splitlines().index(
            "# APP_NOT_A_HOST_VARIABLE=1"
        ) + 1
        self.assertIn(
            f"{m.ENV_EXAMPLE_REL_PATH}:{first_line}:", findings[0]
        )

    def test_comment_mention_in_bootstrap_is_not_a_declaration(self):
        example = EXAMPLE + "\nAPP_DOC_ONLY=1\n"
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("documents APP_DOC_ONLY", findings[0])

    def test_commented_unknown_key_fires_too(self):
        # The commented-out spelling is part of the key set in both
        # directions: an unknown key documented only as a commented
        # example (the file's spelling for optional groups) fires the
        # same finding its active spelling does.
        example = EXAMPLE + "\n#   APP_NOT_A_HOST_VARIABLE=1\n"
        findings = m.scan(make_tree(base_files(example=example)))
        self.assertEqual(len(findings), 1)
        self.assertIn("documents APP_NOT_A_HOST_VARIABLE", findings[0])


class RealTree(unittest.TestCase):
    def test_repository_example_matches_the_declared_surface(self):
        root = pathlib.Path(__file__).resolve().parent.parent
        self.assertEqual(m.scan(root), [])


if __name__ == "__main__":
    unittest.main()
