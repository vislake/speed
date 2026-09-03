#!/usr/bin/env python3
"""Docs-site skeleton check for docs/site/.

The docs site (docs/site/) is a real, previewable skeleton -- static HTML
with no build step, no npm project and no network (docs/site/README.md) --
whose full machinery (per-version release directories, llms.txt at the
public root, a build step/SSG) is a later roadmap milestone per
docs/internal/13-documentation-standards.md. This script keeps the
skeleton honest in CI (the docs-check pipeline, .github/workflows/
docs-check.yml, on every PR touching docs/) by checking what can be
checked without any tooling:

* the required entry files exist at the site root (index.html,
  README.md);
* every internal link and asset reference on every HTML page resolves
  within the site tree (fragments, external http(s)/mailto/tel/data
  URLs and protocol-relative URLs are not internal links and are
  skipped; an absolute path that would escape the site tree is a
  violation, since pages must link inside the site);
* the offline preview really serves: the script starts the python3
  standard-library HTTP server (the exact `python3 -m http.server`
  command docs/site/README.md and the Taskfile docs:serve task use) on
  an ephemeral port and fetches the site root, expecting a 200.

Exit codes: 0 = clean; 1 = a structural violation (missing required
file, unresolvable internal link, offline preview not serving); 2 =
infrastructure error (site tree missing, a page unreadable as text, or
the HTTP server could not be started). Paths in the report are relative
to --root.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
import urllib.parse
import urllib.request
from pathlib import Path

# Entry files every docs-site round must keep at the site root.
REQUIRED = ["index.html", "README.md"]

# Links whose scheme makes them non-internal by definition. A bare
# fragment ("#section") also stays internal to its page but resolves to
# nothing on disk, so it is skipped rather than checked.
_EXTERNAL_PREFIXES = ("http://", "https://", "mailto:", "tel:", "data:")
_PROTOCOL_RELATIVE = "//"


def _collect_html_pages(site_dir: Path) -> list[Path]:
    return sorted(site_dir.rglob("*.html"))


def _check_required(site_dir: Path, root: Path) -> list[str]:
    violations = []
    for name in REQUIRED:
        path = site_dir / name
        rel = os.path.relpath(path, root)
        if not path.is_file():
            violations.append(f"{rel}: required entry file is missing")
    return violations


def _check_links(site_dir: Path, pages: list[Path], root: Path) -> list[str]:
    violations = []
    link_re = re.compile(r"""(?:href|src)\s*=\s*"([^"]+)"|(?:href|src)\s*=\s*'([^']+)'""")
    for page in pages:
        try:
            text = page.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as exc:
            rel = os.path.relpath(page, root)
            violations.append(f"{rel}: page unreadable as UTF-8 text ({exc})")
            continue
        for match in link_re.finditer(text):
            target = match.group(1) if match.group(1) is not None else match.group(2)
            violation = _resolve_one(page, target, site_dir, root)
            if violation:
                violations.append(violation)
    return violations


def _resolve_one(page: Path, target: str, site_dir: Path, root: Path) -> str | None:
    rel = os.path.relpath(page, root)
    target = target.split("#", 1)[0].strip()
    if not target:
        return None  # fragment-only or empty reference
    if target.startswith(_EXTERNAL_PREFIXES) or target.startswith(_PROTOCOL_RELATIVE):
        return None
    if target.startswith("/"):
        return f"{rel}: absolute link {target!r} would escape the site tree -- link inside docs/site/"
    try:
        decoded = urllib.parse.unquote(target)
        resolved = (page.parent / decoded).resolve()
    except ValueError as exc:
        return f"{rel}: unresolvable link {target!r} ({exc})"
    try:
        resolved.relative_to(site_dir.resolve())
    except ValueError:
        return f"{rel}: link {target!r} resolves outside the site tree"
    if resolved.is_file():
        return None
    if resolved.is_dir() and (resolved / "index.html").is_file():
        return None
    return f"{rel}: link {target!r} resolves to nothing ({os.path.relpath(resolved, root)})"


def _check_offline_preview(site_dir: Path) -> list[str]:
    """Start the stdlib HTTP server over the site tree and fetch /."""
    violations = []
    proc = None
    port = _pick_free_port()
    # Same server as the preview command in docs/site/README.md and the
    # Taskfile docs:serve task (`python3 -m http.server <port> --bind
    # 127.0.0.1 -d docs/site`), on an ephemeral port so the check never
    # collides with a developer's own preview instance.
    argv = [sys.executable, "-m", "http.server", str(port),
            "--bind", "127.0.0.1", "-d", "."]
    try:
        proc = subprocess.Popen(
            argv,
            cwd=site_dir,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        url = f"http://127.0.0.1:{port}/"
        status = _fetch_with_retries(url)
        if status is None:
            violations.append(f"offline preview: GET {url} never succeeded -- "
                              "the stdlib HTTP server did not come up")
        elif status != 200:
            violations.append(f"offline preview: GET {url} returned {status}")
    except OSError as exc:
        violations.append(f"offline preview: could not start the HTTP server ({exc})")
    finally:
        if proc is not None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
    return violations


def _fetch_with_retries(url: str, deadline: float = 15.0) -> int | None:
    """GET the URL, tolerating the server's startup window.

    subprocess.Popen returns before the interpreter inside has bound the
    listen socket, so the first attempt can hit connection refused; poll
    until the deadline rather than treating that as failure.
    """
    import time

    started = time.monotonic()
    while time.monotonic() - started < deadline:
        try:
            with urllib.request.urlopen(url, timeout=5) as response:
                return int(response.status)
        except OSError:
            time.sleep(0.2)
    return None


def _pick_free_port() -> int:
    """Bind an ephemeral port, release it, and hand the number back.

    The http.server module of modern pythons logs through the logging
    machinery rather than printing a parseable banner, so the checker
    picks the port itself instead of parsing the server's startup line.
    The bind-and-release window is a benign race for a CI self-check.
    """
    import socket

    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Validate the docs/site/ skeleton: required entry files present, every "
            "internal link on a page resolves inside the tree, and the offline "
            "preview (python3 stdlib HTTP server) really serves."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root to check (default: current directory); "
        "paths in the report are relative to it",
    )
    args = parser.parse_args(argv)
    root = Path(args.root).resolve()
    if not root.is_dir():
        print(f"error: --root is not a directory: {args.root}", file=sys.stderr)
        return 2
    site_dir = root / "docs" / "site"
    if not site_dir.is_dir():
        print(
            "error: docs/site/ is missing under --root -- this check guards an "
            "existing tree",
            file=sys.stderr,
        )
        return 2

    violations: list[str] = []
    pages = _collect_html_pages(site_dir)
    violations += _check_required(site_dir, root)
    violations += _check_links(site_dir, pages, root)
    violations += _check_offline_preview(site_dir)

    if not pages:
        violations.append("docs/site/: no HTML pages found")
    for violation in violations:
        print(f"docs-site: violation    {violation}")
    if violations:
        print(
            "error: docs/site/ failed its structural check -- fix the violations "
            "above (see docs/site/README.md)",
            file=sys.stderr,
        )
        return 1
    print(
        f"docs-site: ok          required files present; {len(pages)} HTML page(s); "
        "internal links resolve; offline preview serves"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
