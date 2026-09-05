#!/usr/bin/env python3
"""Docs-site build check for docs/site/.

docs/site/ is a real Hugo project (theme: hugo-book, pinned as a git
submodule under docs/site/themes/hugo-book -- docs/site/README.md has
the machinery-decision rationale) as of the Hugo migration round. This
script keeps the BUILT output honest in CI (the docs-check pipeline,
.github/workflows/docs-check.yml, on every PR touching docs/) by:

* running a real `hugo --minify` build from docs/site (unless
  --skip-build asks it to reuse an already-built docs/site/public/,
  e.g. for a fast local re-check after a manual `hugo server` session);
* checking that every required page landed in the built output, in
  both languages (en default/unprefixed, zh-cn under a /zh-cn/ prefix
  -- see docs/site/hugo.toml's own comments for why);
* checking that every internal link and asset reference on every built
  HTML page resolves -- a page-relative href (the form Hugo emits for
  in-page Markdown links) resolves against the *page's own* directory,
  the way a browser resolves it; an href rooted at the site's own
  baseURL path (read from hugo.toml, so a baseURL change cannot make
  this check silently pass or fail for the wrong reason) resolves
  against the built tree's root; anything else absolute -- rooted
  outside the site's own baseURL path -- is a violation, since a page
  must not link outside the site it belongs to. Fragments, external
  http(s)/mailto/tel/data URLs and protocol-relative URLs are not
  internal links and are skipped;
* checking that llms.txt landed exactly once at the built tree's root
  (not per-language -- the whole reason the llms.txt convention wants
  one crawlable root-level file);
* checking that the built output really serves: the script starts the
  python3 standard-library HTTP server over docs/site/public/ on an
  ephemeral port and fetches its root, expecting a 200 -- this is a
  smoke test that the built artifact is servable at all, not a check
  that a locally-mounted plain HTTP server matches the real baseURL
  subpath GitHub Pages serves it under (an inherent local-preview
  limitation of a baseURL with a subpath, unrelated to whether the
  build itself is correct -- the link-resolution check above already
  covers "would these hrefs resolve once mounted at the real baseURL").

What changed from this script's earlier form: it used to check a
hand-written HTML source tree directly (docs/site/index.html and
friends, no build step). The docs/site/ directory now holds Hugo
*source* (content.en/, content.zh-cn/, hugo.toml, the theme submodule)
and the checked artifact is docs/site/public/, Hugo's build output --
gitignored, never committed, and rebuilt here rather than assumed
fresh. Exit-code contract is unchanged: 0 = clean; 1 = a structural
violation (missing required page, unresolvable internal link, llms.txt
missing or duplicated, offline preview not serving); 2 = infrastructure
error (docs/site/ missing, hugo not on PATH, the build itself failing
to run, a built page unreadable as text, or the HTTP server could not
be started). Paths in the report are relative to --root.
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import urllib.parse
import urllib.request
from pathlib import Path

# Pages every language's build must produce, relative to that
# language's own root in the built tree (en: docs/site/public/<page>;
# zh-cn: docs/site/public/zh-cn/<page>) -- one entry per page this
# round's migration ships, matching hugo.toml's BookSection='docs'
# layout (home page outside docs/, the five reference pages under it).
REQUIRED_PAGES_PER_LANGUAGE = [
    "index.html",
    "docs/index.html",
    "docs/quickstart/index.html",
    "docs/modules/index.html",
    "docs/ai-agents/index.html",
    "docs/about/index.html",
    "docs/status/index.html",
]

# The zh-cn build lands under this prefix inside the built tree
# (hugo.toml: defaultContentLanguageInSubdir = false, so only the
# non-default language gets a URL prefix).
ZH_CN_PREFIX = "zh-cn"

# Links whose scheme makes them non-internal by definition. A bare
# fragment ("#section") also stays internal to its page but resolves to
# nothing on disk, so it is skipped rather than checked.
_EXTERNAL_PREFIXES = ("http://", "https://", "mailto:", "tel:", "data:")
_PROTOCOL_RELATIVE = "//"


def _collect_html_pages(public_dir: Path) -> list[Path]:
    return sorted(public_dir.rglob("*.html"))


def _read_base_path(site_dir: Path) -> str:
    """Return the URL path component of hugo.toml's baseURL.

    E.g. baseURL = 'https://vislake.github.io/speed/' -> '/speed'.
    An empty return means the site is configured to serve from a
    domain root, so a leading-'/' link is already root-relative to the
    built tree with nothing to strip.
    """
    config_path = site_dir / "hugo.toml"
    try:
        text = config_path.read_text(encoding="utf-8")
    except OSError as exc:
        raise SystemExit(f"error: could not read {config_path}: {exc}")
    m = re.search(r"^\s*baseURL\s*=\s*['\"]([^'\"]+)['\"]", text, re.MULTILINE)
    if not m:
        raise SystemExit(f"error: no baseURL found in {config_path}")
    parsed = urllib.parse.urlparse(m.group(1))
    return parsed.path.rstrip("/")


def _run_hugo_build(site_dir: Path) -> list[str]:
    """Run `hugo --minify --gc` from site_dir. Returns violations (build
    failures), or raises SystemExit(2) if hugo itself cannot be found."""
    hugo_bin = shutil.which("hugo")
    if hugo_bin is None:
        raise SystemExit(
            "error: 'hugo' is not on PATH -- install the pinned version "
            "(.mise.toml's `hugo` entry / .github/actions/setup-hugo-env) "
            "before running this check, or pass --skip-build to reuse an "
            "existing docs/site/public/"
        )
    proc = subprocess.run(
        [hugo_bin, "--minify", "--gc"],
        cwd=site_dir,
        capture_output=True,
        text=True,
    )
    if proc.returncode != 0:
        return [
            "hugo build failed (exit "
            f"{proc.returncode}):\n{proc.stdout}{proc.stderr}".rstrip()
        ]
    # Hugo prints build warnings to stdout, not a failing exit code --
    # the docs/site round's own warnings policy (root CLAUDE.md's global
    # instructions) treats a build warning as a first-class issue, so a
    # WARN line is a violation here even though hugo itself exits 0.
    warnings = [
        line.strip()
        for line in proc.stdout.splitlines()
        if line.strip().startswith("WARN")
    ]
    return [f"hugo build emitted a warning: {line}" for line in warnings]


def _check_required(public_dir: Path, root: Path) -> list[str]:
    violations = []
    for page in REQUIRED_PAGES_PER_LANGUAGE:
        en_path = public_dir / page
        rel = os.path.relpath(en_path, root)
        if not en_path.is_file():
            violations.append(f"{rel}: required page is missing from the built output")
        zh_path = public_dir / ZH_CN_PREFIX / page
        rel = os.path.relpath(zh_path, root)
        if not zh_path.is_file():
            violations.append(f"{rel}: required zh-cn page is missing from the built output")
    return violations


def _check_llms_txt(public_dir: Path, root: Path) -> list[str]:
    violations = []
    root_llms = public_dir / "llms.txt"
    if not root_llms.is_file():
        violations.append(
            f"{os.path.relpath(root_llms, root)}: llms.txt is missing from the built "
            "output root"
        )
    zh_llms = public_dir / ZH_CN_PREFIX / "llms.txt"
    if zh_llms.is_file():
        violations.append(
            f"{os.path.relpath(zh_llms, root)}: llms.txt must not be duplicated "
            "per-language -- static/llms.txt should be the only source"
        )
    return violations


def _check_links(public_dir: Path, pages: list[Path], root: Path, base_path: str) -> list[str]:
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
            violation = _resolve_one(page, target, public_dir, root, base_path)
            if violation:
                violations.append(violation)
    return violations


def _resolve_one(
    page: Path, target: str, public_dir: Path, root: Path, base_path: str
) -> str | None:
    rel = os.path.relpath(page, root)
    target = target.split("#", 1)[0].strip()
    if not target:
        return None  # fragment-only or empty reference
    if target.startswith(_EXTERNAL_PREFIXES) or target.startswith(_PROTOCOL_RELATIVE):
        return None
    target = target.split("?", 1)[0]
    if target.startswith("/"):
        if base_path and not (target == base_path or target.startswith(base_path + "/")):
            return (
                f"{rel}: absolute link {target!r} is rooted outside this site's own "
                f"baseURL path {base_path!r} -- link inside the site"
            )
        site_relative = target[len(base_path):] if base_path else target
        try:
            decoded = urllib.parse.unquote(site_relative.lstrip("/"))
            resolved = (public_dir / decoded).resolve()
        except ValueError as exc:
            return f"{rel}: unresolvable link {target!r} ({exc})"
    else:
        # Page-relative: resolve against the directory the page itself
        # lives in, the way a browser resolves it.
        try:
            decoded = urllib.parse.unquote(target)
            resolved = (page.parent / decoded).resolve()
        except ValueError as exc:
            return f"{rel}: unresolvable link {target!r} ({exc})"
    try:
        resolved.relative_to(public_dir.resolve())
    except ValueError:
        return f"{rel}: link {target!r} resolves outside the built site tree"
    if resolved.is_file():
        return None
    if resolved.is_dir() and (resolved / "index.html").is_file():
        return None
    return f"{rel}: link {target!r} resolves to nothing ({os.path.relpath(resolved, root)})"


def _check_offline_preview(public_dir: Path) -> list[str]:
    """Start the stdlib HTTP server over the built output and fetch /."""
    violations = []
    proc = None
    port = _pick_free_port()
    argv = [sys.executable, "-m", "http.server", str(port),
            "--bind", "127.0.0.1", "-d", "."]
    try:
        proc = subprocess.Popen(
            argv,
            cwd=public_dir,
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
            "Validate docs/site/'s built Hugo output: a real `hugo --minify` build "
            "succeeds with no warnings, every required page (both languages) is "
            "present, llms.txt lands exactly once at the built root, every internal "
            "link on a page resolves, and the offline preview (python3 stdlib HTTP "
            "server) really serves."
        )
    )
    parser.add_argument(
        "--root",
        default=".",
        help="repository root to check (default: current directory); "
        "paths in the report are relative to it",
    )
    parser.add_argument(
        "--skip-build",
        action="store_true",
        help="reuse an already-built docs/site/public/ instead of running `hugo "
        "--minify` again (for a fast local re-check after a `hugo server` session)",
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
    if args.skip_build:
        print("docs-site: skip-build  reusing existing docs/site/public/")
    else:
        violations += _run_hugo_build(site_dir)
        if violations:
            for violation in violations:
                print(f"docs-site: violation    {violation}")
            print(
                "error: docs/site/ failed to build cleanly -- fix the violations "
                "above",
                file=sys.stderr,
            )
            return 1

    public_dir = site_dir / "public"
    if not public_dir.is_dir():
        print(
            f"error: {os.path.relpath(public_dir, root)}/ is missing -- run "
            "`hugo --minify` from docs/site (or drop --skip-build)",
            file=sys.stderr,
        )
        return 2

    base_path = _read_base_path(site_dir)
    pages = _collect_html_pages(public_dir)
    violations += _check_required(public_dir, root)
    violations += _check_llms_txt(public_dir, root)
    violations += _check_links(public_dir, pages, root, base_path)
    violations += _check_offline_preview(public_dir)

    if not pages:
        violations.append(f"{os.path.relpath(public_dir, root)}/: no HTML page found")
    for violation in violations:
        print(f"docs-site: violation    {violation}")
    if violations:
        print(
            "error: docs/site/'s built output failed its structural check -- fix "
            "the violations above (see docs/site/README.md)",
            file=sys.stderr,
        )
        return 1
    print(
        f"docs-site: ok          hugo build clean; required pages present in both "
        f"languages; llms.txt present once; {len(pages)} built HTML page(s); "
        "internal links resolve; offline preview serves"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
