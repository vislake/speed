#!/usr/bin/env python3
"""Env-example consistency gate: the reference app host's declared
environment surface and its committed .env.example are one key set.

examples/reference-app/.env.example is the reference app's committed
carrier for the variables its host reads: its own header states that it
documents ConfigFromEnv's loader-driven bootstrap surface, and readers
copy it to .env or apply it through a shell / a process manager. The
host side of that surface is declared in three forms inside
examples/reference-app/internal/app/bootstrap.go:

  * the loader target's env pins -- the hostConfig struct tags its own
    variables with config:"env=NAME" (further loader options after the
    name -- the key materials' derive among them -- do not change the
    variable name), so the tag is each variable's declaration site;
  * APP_ROOT_KEY -- the one variable that is not a struct field at all:
    the loader reads it through the WithRootKeyEnv option loadHostConfig
    wires. The call's argument names the variable either as a string
    literal or as an identifier a same-file const declaration binds to
    one; an identifier with no such constant declares nothing;
  * the embedded platform declaration -- hostConfig embeds the platform
    module's key-material declaration (app.PlatformConfig) tagged
    config:"-", so the loader's walk of the host target skips it and the
    loader loads it as its own target instead: each of its fields reads
    the variable the loader derives from the field's declared key path
    under the host's prefix -- config.EnvName's rule: the prefix, then
    the key uppercased with each level of nesting spelled as a double
    underscore, so config.cipher_key reads APP_CONFIG__CIPHER_KEY under
    APP_. The declared key paths are read
    from the platform module's own declaration list in
    go/app/config.go, the one place they are spelled.

Nothing else in the app's executable code reads the environment (the
app's own unit suite pins that surface, unittest/
bootstrap_direct_reads_test.go), so the pins, that call and the
declaration's derived names are the host's whole declared set.

The example side is read line by line in both spellings the file
actually uses for keys: an active KEY= entry and a commented-out
example (``# APP_X=...`` -- the indented ``#   APP_X=...`` variant
included) both document a key; a prose line that merely names a
variable (no ``=`` at the line's head after the comment marker) does
not.

The gate compares the two sets in both directions and fails on either
difference:

  * a host variable the example documents no entry for -- the drift
    class the file's readers hit first, and the one this gate exists
    for;
  * a key the example documents that the host never declares -- the
    file's declared scope is exactly the host's bootstrap surface, so
    an entry outside it is either a stale line to remove or a variable
    the loader target should pin. This direction is deliberately strict
    with no in-file escape marker: the file carries no non-host key, so
    a marker would be machinery nothing exercises, and a future
    non-host entry that is genuinely wanted is a checker change
    (git-visible, reviewable) rather than a per-line opt-out the gate
    would stop seeing.

Usage:
    python3 tools/check_env_example_consistency.py [--root DIR]

Exit codes: 0 = the host's declared variables and the example's
documented keys are one set; 1 = drift in either direction; 2 =
usage/infrastructure error (unreadable root).
Standard library only, Python >= 3.11.
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

# The host's declaration site, the platform declaration it embeds, and
# the example it must agree with, relative to the repository root.
BOOTSTRAP_REL_PATH = "examples/reference-app/internal/app/bootstrap.go"
PLATFORM_DECL_REL_PATH = "go/app/config.go"
ENV_EXAMPLE_REL_PATH = "examples/reference-app/.env.example"

# One struct-tag literal: config:"env=NAME" with any further loader
# options (",derive") after the name.
CONFIG_TAG = re.compile(r'config:"([^"]*)"')

# The root-key read: WithRootKeyEnv(NAME), the name spelled either as
# a string literal (group 1) or as an identifier (group 2) the file's
# own const declaration binds to the name's literal.
ROOT_KEY_ENV = re.compile(
    r'WithRootKeyEnv\(\s*'
    r'(?:"([A-Za-z_][A-Za-z0-9_]*)"|([A-Za-z_][A-Za-z0-9_]*))'
    r'\s*\)'
)

# One same-file const binding the root-key call may name through its
# argument: const NAME = "STRING", the value an environment variable
# name.
ROOT_KEY_CONST = re.compile(
    r'\bconst\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([A-Za-z_][A-Za-z0-9_]*)"'
)

# The embedded platform declaration: the host target embeds the
# platform's key-material declaration tagged config:"-" (the skip
# option), and the enclosing file spells the environment prefix the
# derived variable names carry as a same-file literal const.
EMBEDDED_PLATFORM = re.compile(r'PlatformConfig\s+`config:"-"`')
ENV_PREFIX_CONST = "envPrefix"

# The platform module's declared key-path list, and one quoted path
# inside it.
PLATFORM_KEY_PATHS = re.compile(
    r"var platformKeyPaths = \[\]string\{(.*?)\}", re.DOTALL
)
PLATFORM_KEY_PATH = re.compile(r'"([A-Za-z_][A-Za-z0-9_.]*)"')

# A documented key: NAME= at the head of a line's content (the value
# may be empty), once any leading whitespace and the comment marker of
# the commented-out example spelling are stripped.
EXAMPLE_KEY = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=")


def declared_host_keys(text: str, platform_text: str) -> dict[str, int]:
    """The variables bootstrap.go's host surface declares, name -> first
    declaration line.

    Three declaration forms are read: a struct tag's env pin
    (config:"env=NAME"; any loader options after the comma-separated
    name are not part of the variable), the WithRootKeyEnv call,
    whose argument is the name as a string literal or an identifier
    the file's own const declaration binds to one (an identifier with
    no such constant in the file declares nothing, and the line
    reported for the resolved identifier form is the constant's
    declaration line), and the embedded platform declaration, whose
    derived names -- one per declared key path, read from
    platform_text -- are reported at the embed's line. Full-line ``//``
    comments are skipped -- a comment may state a variable's name, but
    only the pins and the call site declare one, and the line numbers
    reported for the findings point at code."""
    lines = text.splitlines()
    consts: dict[str, tuple[str, int]] = {}
    for line_no, line in enumerate(lines, start=1):
        if line.strip().startswith("//"):
            continue
        match = ROOT_KEY_CONST.search(line)
        if match and match.group(1) not in consts:
            consts[match.group(1)] = (match.group(2), line_no)

    keys: dict[str, int] = {}
    for line_no, line in enumerate(lines, start=1):
        if line.strip().startswith("//"):
            continue
        for tag in CONFIG_TAG.finditer(line):
            for option in tag.group(1).split(","):
                if option.startswith("env="):
                    name = option[len("env="):].strip()
                    if name and name not in keys:
                        keys[name] = line_no
        match = ROOT_KEY_ENV.search(line)
        if not match:
            continue
        if match.group(1) is not None:
            name, decl_line = match.group(1), line_no
        elif match.group(2) in consts:
            name, decl_line = consts[match.group(2)]
        else:
            continue
        if name not in keys:
            keys[name] = decl_line

    # The embedded platform declaration's derived names: one per declared
    # key path, reported at the embed's line. A declaration the gate cannot
    # read end to end -- no embed, no prefix const, no key-path list --
    # contributes nothing, and the example's entries then read as
    # variables the host never declares, which is the drift the gate is
    # for.
    embed_line = next(
        (
            line_no
            for line_no, line in enumerate(lines, start=1)
            if not line.strip().startswith("//")
            and EMBEDDED_PLATFORM.search(line)
        ),
        None,
    )
    prefix = consts.get(ENV_PREFIX_CONST)
    paths_block = PLATFORM_KEY_PATHS.search(platform_text)
    if embed_line is None or prefix is None or paths_block is None:
        return keys
    for path in PLATFORM_KEY_PATH.findall(paths_block.group(1)):
        # config.EnvName(prefix, key): the prefix, then the key uppercased
        # with each nesting level (config.KeyDelimiter ".") spelled as the
        # environment separator (config.EnvSeparator, a double underscore).
        name = prefix[0] + path.upper().replace(".", "__")
        keys.setdefault(name, embed_line)
    return keys


def documented_example_keys(text: str) -> dict[str, int]:
    """The keys .env.example documents, name -> first documenting line.

    Both key spellings the file uses count: an active ``KEY=`` line and
    a commented-out example (``# APP_X=...``, the indented variant
    included). A prose line that merely names a variable does not."""
    keys: dict[str, int] = {}
    for line_no, line in enumerate(text.splitlines(), start=1):
        entry = line.strip()
        if entry.startswith("#"):
            entry = entry[1:].strip()
        match = EXAMPLE_KEY.match(entry)
        if match and match.group(1) not in keys:
            keys[match.group(1)] = line_no
    return keys


def scan(root: pathlib.Path) -> list[str]:
    findings: list[str] = []
    bootstrap = root / BOOTSTRAP_REL_PATH
    example = root / ENV_EXAMPLE_REL_PATH
    for path, rel, what in (
        (bootstrap, BOOTSTRAP_REL_PATH, "the host's declaration site"),
        (example, ENV_EXAMPLE_REL_PATH, "the committed example"),
    ):
        if not path.is_file():
            findings.append(
                f"{rel}: {what} is missing -- the gate compares the "
                "host's declared environment surface against the "
                "committed example, and both sides are committed "
                "artifacts"
            )
    if findings:
        return findings

    bootstrap_text = bootstrap.read_text(encoding="utf-8")
    # The platform declaration participates only when the host target
    # embeds it; the file is read then, and its absence is a finding.
    platform = root / PLATFORM_DECL_REL_PATH
    platform_text = ""
    if EMBEDDED_PLATFORM.search(bootstrap_text):
        if not platform.is_file():
            findings.append(
                f"{PLATFORM_DECL_REL_PATH}: the platform declaration the "
                f"host embeds ({BOOTSTRAP_REL_PATH} reads its key paths "
                "from here) is missing"
            )
            return findings
        platform_text = platform.read_text(encoding="utf-8")

    host = declared_host_keys(bootstrap_text, platform_text)
    documented = documented_example_keys(example.read_text(encoding="utf-8"))

    for name, line_no in sorted(host.items(), key=lambda item: item[1]):
        if name not in documented:
            findings.append(
                f"{BOOTSTRAP_REL_PATH}:{line_no}: pins {name}, but "
                f"{ENV_EXAMPLE_REL_PATH} documents no such key -- add an "
                "entry there (an active KEY= line or a commented-out "
                "example; the file counts both)"
            )
    for name, line_no in sorted(documented.items(), key=lambda item: item[1]):
        if name not in host:
            findings.append(
                f"{ENV_EXAMPLE_REL_PATH}:{line_no}: documents {name}, "
                "which the host never declares -- remove the entry, or "
                "wire the variable in the loader target so the host "
                f"reads it ({BOOTSTRAP_REL_PATH})"
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
        print(
            f"check_env_example_consistency: --root is not a directory: "
            f"{root}",
            file=sys.stderr,
        )
        return 2
    try:
        findings = scan(root)
    except OSError as exc:
        print(f"check_env_example_consistency: {exc}", file=sys.stderr)
        return 2
    for finding in findings:
        print(finding)
    if findings:
        print(
            "check_env_example_consistency: %d finding(s)" % len(findings),
            file=sys.stderr,
        )
        return 1
    print(
        "check_env_example_consistency: the host's declared variables and "
        f"{ENV_EXAMPLE_REL_PATH}'s documented keys are one set"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
