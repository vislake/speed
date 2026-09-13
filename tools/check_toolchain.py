#!/usr/bin/env python3
"""Toolchain drift gate for the root .mise.toml.

The developer toolchain is pinned with mise; the root .mise.toml carries
the versions. One of those versions is a MIRROR of an authoritative
source elsewhere in the repository -- the file a tool of its own already
reads -- and this gate fails when the mirror drifts from its source:

  go            go.work's `go` directive

The other tools .mise.toml pins are pinned there and nowhere else, so
they have no source to drift from and this gate does not check them.

Bump the authoritative source and the mirror together; this gate exists
because they are separate files and will drift without it.

Exit codes: 0 = every mirror equals its source; 1 = drift (a mirror
differs from its source, or a report printed below that starts with
MISMATCH); 2 = infrastructure error (a source file missing or
unparsable, or an expected tool absent from .mise.toml). Paths in the
report are relative to --root.
"""

from __future__ import annotations

import argparse
import os
import re
import sys
import tomllib


def _infra(message: str) -> None:
    """Report an infrastructure failure and exit 2.

    The exit-code contract (module docstring): 2 = infrastructure error
    (a source file missing or unparsable, a tool absent from .mise.toml),
    and only 1 = drift. SystemExit alone cannot carry the distinction --
    sys.exit("message") exits 1 -- so the message is printed to stderr
    first and the bare code raised.
    """
    print(message, file=sys.stderr)
    raise SystemExit(2)


def _read_mise_tools(root: str) -> dict[str, str]:
    path = os.path.join(root, ".mise.toml")
    try:
        with open(path, "rb") as fh:
            data = tomllib.load(fh)
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the mise config this gate guards")
    except tomllib.TOMLDecodeError as exc:
        _infra(f"error: {path} is not parsable TOML: {exc}")
    tools = data.get("tools")
    if not isinstance(tools, dict):
        _infra(f"error: {path} has no [tools] table")
    return {str(k): str(v) for k, v in tools.items()}


def _read_go_version(root: str) -> str:
    path = os.path.join(root, "go.work")
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
    except FileNotFoundError:
        _infra(f"error: {path} is missing -- the source of the go pin")
    m = re.search(r"^go\s+(\d+\.\d+(?:\.\d+)?)", text, re.MULTILINE)
    if not m:
        _infra(f"error: no 'go' directive found in {path}")
    return m.group(1)


# Tools .mise.toml mirrors, with the authoritative source each one mirrors
# and the name the report calls that source.
SOURCES = [
    ("go", _read_go_version, "go.work's go directive"),
]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Fail when a version pinned in the root .mise.toml no longer "
            "mirrors its authoritative source (go.work)."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root to check (default: current directory); "
        "paths in the report are relative to it",
    )
    args = parser.parse_args(argv)
    root = os.path.abspath(args.root)
    if not os.path.isdir(root):
        print(f"error: --root is not a directory: {args.root}", file=sys.stderr)
        return 2

    mise = _read_mise_tools(root)
    drifted = 0
    missing = 0
    for tool, reader, source_name in SOURCES:
        if tool not in mise:
            print(f"toolchain: MISSING     {tool} is absent from .mise.toml")
            missing += 1
            continue
        mirror = mise[tool]
        source = reader(root)
        if mirror == source:
            print(
                f"toolchain: ok          {tool}: .mise.toml {mirror} == "
                f"{source_name} ({source})"
            )
        else:
            print(
                f"toolchain: MISMATCH    {tool}: .mise.toml pins {mirror} but "
                f"{source_name} says {source}"
            )
            drifted += 1

    if missing:
        # An expected tool absent from .mise.toml is an infrastructure
        # failure per the module docstring's exit-code contract: the gate
        # exists to prove the mirrors cannot drift, and a mirror that does
        # not exist cannot be compared.
        print(
            f"error: {missing} expected tool(s) absent from .mise.toml -- "
            "every tool this gate's SOURCES list pins must appear in the "
            "[tools] table (see the .mise.toml header)",
            file=sys.stderr,
        )
        return 2
    if drifted:
        print(
            f"error: {drifted} tool version(s) drifted -- bump the authoritative "
            "source and .mise.toml together (see the .mise.toml header)",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
