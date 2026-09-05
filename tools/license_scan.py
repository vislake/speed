#!/usr/bin/env python3
"""license_scan.py -- verify tools/dependency-licenses.json against the tree.

The manifest records the license adjudication for every direct third-party
dependency of the repository's implemented Go modules (all go/*/go.mod
direct requires, i.e. requirement lines without the "// indirect" marker)
and npm packages (web/packages/*/package.json dependencies plus
peerDependencies -- what a consumer installs because of a package).
Workspace-internal packages (@speed/*, github.com/vislake/speed/go/*) are
not external dependencies and carry no entry.

examples/reference-app/go.mod is deliberately out of scope: the reference
app is a consumer example, not a shipped library, so its own dependencies
are not part of the repository's dependency-delivery list (its direct
third-party requires are all permissive today). The M4 release-prep
expansion -- transitive coverage and the reference app's own deps -- is
noted in tools/README.md.

Policy, mirroring docs/internal/20-quality-and-security.md:
  * strong copyleft (GPL family, AGPL)           -> FAIL
  * weak copyleft (MPL, LGPL)                     -> FAIL unless the entry
        carries an "adr" field naming a file under docs/ that exists and
        records the adjudication (one entry today, github.com/hashicorp/
        vault/api, adjudicated by
        docs/adr/0003-accept-mpl2-for-pki-signer-vault.md)
  * any other unrecognized license string         -> FAIL with an
        adjudication message: identify the license from the dependency's
        own license file and record the SPDX id plus evidence
  * everything in the permissive set below       -> PASS

Beyond the policy check, --check re-derives the expected dependency set
from the live tree and fails on any drift from the manifest:
  * a dependency newly required by a module/package but absent from the
    manifest (adjudicate and add it),
  * a manifest entry that no module/package requires any more (remove it),
  * a version that changed (go.mod go-list resolution; for npm the exact
    version pinned by web/pnpm-lock.yaml, which the frozen-lockfile CI
    install resolves),
  * a used_by list that no longer matches reality,
  * an npm dependency that package.json declares but pnpm-lock.yaml does
    not resolve (the frozen-lockfile install would fail -- a real find,
    not a license one).

Licenses in the manifest were identified from the license file each
release ships; the evidence field records where (go module cache paths are
deterministic: $GOMODCACHE/<module>@<version>/<file>).

Usage:
  python3 tools/license_scan.py            # check the repository (exit 0/1)
  python3 tools/license_scan.py --selftest # run the planted-fixture suite
                                           # under tools/license_scan_testdata/

Exit codes: 0 all checks pass; 1 any violation found; 2 usage error.
"""

import json
import os
import re
import sys

# Licenses that need no adjudication beyond the manifest entry itself.
PERMISSIVE = {
    "0BSD",
    "Apache-2.0",
    "BSD-2-Clause",
    "BSD-3-Clause",
    "CC0-1.0",
    "ISC",
    "MIT",
    "Unlicense",
}

# Strong copyleft: banned outright. Only the SPDX ids actually in use in
# the wild are listed; anything unlisted falls through to the unknown case
# and fails with an adjudication message, which is the fail-closed branch.
STRONG_COPYLEFT = {
    "AGPL-3.0",
    "AGPL-3.0-only",
    "AGPL-3.0-or-later",
    "GPL-2.0",
    "GPL-2.0-only",
    "GPL-2.0-or-later",
    "GPL-3.0",
    "GPL-3.0-only",
    "GPL-3.0-or-later",
}

# Weak copyleft: pass only with a recorded adjudication (an "adr" field on
# the manifest entry naming an existing file under docs/). One real entry
# carries this today -- github.com/hashicorp/vault/api, adjudicated by
# docs/adr/0003-accept-mpl2-for-pki-signer-vault.md; the fixture suite
# proves both branches (adjudicated and un-adjudicated) independently of
# that real entry.
WEAK_COPYLEFT = {
    "LGPL-2.1",
    "LGPL-2.1-only",
    "LGPL-2.1-or-later",
    "LGPL-3.0",
    "LGPL-3.0-only",
    "LGPL-3.0-or-later",
    "MPL-1.1",
    "MPL-2.0",
}

MANIFEST_REL = os.path.join("tools", "dependency-licenses.json")
TESTDATA_REL = os.path.join("tools", "license_scan_testdata")

# Workspace module path prefixes: not external dependencies.
WORKSPACE_PREFIXES = ("github.com/vislake/speed/", "@speed/")
# npm dependency specs that point at workspace packages or local files.
NPM_SKIP_SPEC = ("workspace:", "file:", "link:", "npm:")


def repo_root():
    """The repository root: license_scan.py lives in tools/."""
    return os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def direct_go_requires(go_mod_path):
    """Parse one go.mod and return {module_path: version} for its direct
    third-party requires.

    Directness is Go's own textual rule: a requirement line without the
    "// indirect" comment marker. `go mod tidy` emits direct requires
    without the marker and indirect ones with it; a hand-edited file that
    forgets the marker on an indirect dep is treated as direct, which
    fails closed (the manifest must then cover it)."""
    result = {}
    with open(go_mod_path) as fh:
        lines = fh.read().splitlines()
    in_block = False
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("require ("):
            in_block = True
            continue
        if stripped == ")":
            in_block = False
            continue
        if in_block:
            body = stripped
        elif stripped.startswith("require "):
            body = stripped[len("require "):]
        else:
            continue
        if "//" in body:
            body, _, comment = body.partition("//")
            if "indirect" in comment:
                continue
        parts = body.split()
        if len(parts) != 2:
            continue
        name, version = parts
        if any(name.startswith(p) for p in WORKSPACE_PREFIXES):
            continue
        result[name] = version
    return result


def go_expected(go_root):
    """Scan every go/*/go.mod: {(name, version): [module dirs...]}."""
    expected = {}
    if not os.path.isdir(go_root):
        return expected
    for module in sorted(os.listdir(go_root)):
        go_mod = os.path.join(go_root, module, "go.mod")
        if not os.path.isfile(go_mod):
            continue
        for name, version in direct_go_requires(go_mod).items():
            expected.setdefault((name, version), []).append(
                os.path.join("go", module))
    for uses in expected.values():
        uses.sort()
    return expected


def manifest_declared(package_json_path):
    """dependencies + peerDependencies of one package.json, minus workspace
    and local specs: {name: range}."""
    declared = {}
    with open(package_json_path) as fh:
        data = json.load(fh)
    for section in ("dependencies", "peerDependencies"):
        for name, spec in (data.get(section) or {}).items():
            if any(spec.startswith(p) for p in NPM_SKIP_SPEC):
                continue
            declared[name] = spec
    return declared


def lockfile_resolve(lock_path, importer, names):
    """Resolve {name: version} for names from the pnpm-lock.yaml importer
    block of one workspace package (lockfile v9 layout). The exact version
    is what a frozen-lockfile install yields.

    Returns (resolved, errors): versions found; error lines for names that
    cannot be resolved or resolve to more than one distinct version."""
    with open(lock_path) as fh:
        text = fh.read()
    # The importers section ends where the top-level "packages:" section
    # begins (column 0); importer keys sit at two-space indent. \Z (not $)
    # closes the final section -- with re.M, $ would match before every
    # newline and truncate every section to a single line.
    importers_text = text.split("\npackages:")[0]
    sections = {}
    for match in re.finditer(r"^  packages/([^:\n]+):\n(.*?)(?=^  packages/|\Z)",
                             importers_text, re.M | re.S):
        sections[match.group(1)] = match.group(2)
    if importer not in sections:
        return {}, [f"pnpm-lock.yaml has no importer block for packages/{importer}"]
    section = sections[importer]
    resolved, errors = {}, []
    for name in sorted(names):
        quoted = re.escape(name)
        # Name key at six-space indent (inside dependencies /
        # devDependencies / peerDependencies), optional quotes for scoped
        # names; version line directly after the specifier line. The
        # version may carry a peer-resolution suffix in parentheses
        # ("26.4.1(typescript@5.9.3)"), so capture up to the parenthesis.
        pattern = re.compile(
            r"^\s{6}[\"']?" + quoted + r"[\"']?:\s*$"
            r"\n\s{8}specifier:[^\n]*"
            r"\n\s{8}version: ([^\s(]+)",
            re.M)
        versions = pattern.findall(section)
        if not versions:
            errors.append(
                f"npm dependency {name} is declared by packages/{importer} "
                f"but pnpm-lock.yaml resolves no version for it -- the "
                f"frozen-lockfile install is broken or the lockfile is stale")
            continue
        distinct = sorted(set(versions))
        if len(distinct) > 1:
            errors.append(
                f"npm dependency {name} resolves to {distinct} in "
                f"pnpm-lock.yaml packages/{importer} -- cannot pin one "
                f"manifest version")
            continue
        resolved[name] = distinct[0]
    return resolved, errors


def npm_expected(web_root, lock_path):
    """Scan every web/packages/*/package.json with dependencies or peers:
    {(name, version): [package dirs...]}, versions resolved from the
    lockfile. Returns (expected, errors)."""
    expected = {}
    errors = []
    packages_dir = os.path.join(web_root, "packages")
    if not os.path.isdir(packages_dir):
        return expected, errors
    for package in sorted(os.listdir(packages_dir)):
        package_json = os.path.join(packages_dir, package, "package.json")
        if not os.path.isfile(package_json):
            continue
        declared = manifest_declared(package_json)
        if not declared:
            continue
        resolved, resolve_errors = lockfile_resolve(
            lock_path, package, set(declared))
        errors.extend(resolve_errors)
        package_dir = os.path.join("web", "packages", package)
        for name, version in resolved.items():
            expected.setdefault((name, version), []).append(package_dir)
    for uses in expected.values():
        uses.sort()
    return expected, errors


def expected_set(root):
    """Everything the manifest must cover, plus hard errors from the scan."""
    expected = {}
    errors = []
    go = go_expected(os.path.join(root, "go"))
    for key, uses in go.items():
        expected[("go", key[0], key[1])] = uses
    lock_path = os.path.join(root, "web", "pnpm-lock.yaml")
    npm, npm_errors = npm_expected(os.path.join(root, "web"), lock_path)
    errors.extend(npm_errors)
    for (name, version), uses in npm.items():
        expected[("npm", name, version)] = uses
    return expected, errors


def entry_policy_error(entry, root):
    """License-policy verdict for one manifest entry, or None if it passes."""
    license_id = entry.get("license")
    if license_id in PERMISSIVE:
        return None
    if license_id in STRONG_COPYLEFT:
        return (
            f"dependency {entry['name']}@{entry.get('version')} is licensed "
            f"{license_id} -- strong copyleft is banned by the supply-chain "
            f"policy (docs/internal/20-quality-and-security.md); replace the "
            f"dependency or drop the feature it serves")
    if license_id in WEAK_COPYLEFT:
        adr = entry.get("adr")
        if adr and os.path.isfile(os.path.join(root, adr)):
            return None
        return (
            f"dependency {entry['name']}@{entry.get('version')} is licensed "
            f"{license_id} -- weak copyleft passes only with an ADR "
            f"recording the adjudication (an \"adr\" field on the manifest "
            f"entry naming an existing docs/ file); none exists today")
    return (
        f"dependency {entry['name']}@{entry.get('version')} has an "
        f"unrecognized license id {license_id!r} -- adjudicate it: read the "
        f"license file the dependency ships (record the file as evidence) "
        f"and set the SPDX id in tools/dependency-licenses.json")


def check_root(root):
    """Run all checks against one tree root. Returns (exit_code, lines)."""
    manifest_path = os.path.join(root, MANIFEST_REL)
    if not os.path.isfile(manifest_path):
        return 2, [f"missing manifest: {MANIFEST_REL}"]
    with open(manifest_path) as fh:
        manifest = json.load(fh)
    entries = manifest.get("dependencies")
    if entries is None:
        return 2, [f"manifest {MANIFEST_REL} has no \"dependencies\" list"]

    errors = []
    for entry in entries:
        error = entry_policy_error(entry, root)
        if error:
            errors.append(error)

    expected, scan_errors = expected_set(root)
    errors.extend(scan_errors)

    manifest_keys = {}
    for entry in entries:
        try:
            key = (entry["ecosystem"], entry["name"], entry["version"])
        except KeyError:
            errors.append(
                f"manifest entry {entry!r} lacks ecosystem/name/version")
            continue
        manifest_keys[key] = entry.get("used_by") or []

    for (ecosystem, name, version), uses in sorted(expected.items()):
        key = (ecosystem, name, version)
        if key not in manifest_keys:
            errors.append(
                f"missing manifest entry for {name}@{version} "
                f"(used by {', '.join(uses)}) -- adjudicate its license and "
                f"add it to {MANIFEST_REL}")
            continue
        recorded = manifest_keys[key]
        if recorded != uses:
            errors.append(
                f"manifest entry {name}@{version} has used_by {recorded} but "
                f"the tree says {uses} -- update the entry (regeneration "
                f"procedure in tools/README.md)")

    for key, used_by in sorted(manifest_keys.items()):
        if key not in expected:
            ecosystem, name, version = key
            errors.append(
                f"orphan manifest entry {name}@{version}: nothing in go/ or "
                f"web/packages declares it as a direct dependency any more "
                f"-- remove the entry")

    lines = []
    if errors:
        lines.append(
            f"license scan FAILED: {len(errors)} violation(s), "
            f"{len(manifest_keys)} manifest entries checked:")
        for error in sorted(errors):
            lines.append(f"  error: {error}")
        return 1, lines
    lines.append(
        f"license scan OK: {len(manifest_keys)} manifest entries match the "
        f"tree, all licenses within policy")
    return 0, lines


def main(argv):
    if len(argv) > 1 and argv[1] == "--selftest":
        root = repo_root()
        testdata = os.path.join(root, TESTDATA_REL)
        if not os.path.isdir(testdata):
            print(f"error: no testdata directory at {TESTDATA_REL}")
            return 2
        failures = 0
        for case in sorted(os.listdir(testdata)):
            case_dir = os.path.join(testdata, case)
            expected_file = os.path.join(case_dir, "expected_exit")
            if not os.path.isfile(expected_file):
                print(f"error: {case} lacks expected_exit")
                failures += 1
                continue
            wanted = int(open(expected_file).read().strip())
            code, lines = check_root(case_dir)
            status = "ok" if code == wanted else "MISMATCH"
            if code != wanted:
                failures += 1
            print(f"[{status}] {case}: expected exit {wanted}, got {code}")
            for line in lines:
                print(f"    {line}")
        print(f"selftest: {'PASS' if failures == 0 else f'{failures} case(s) failed'}")
        return 0 if failures == 0 else 1
    if len(argv) > 1 and argv[1] in ("-h", "--help"):
        print(__doc__)
        return 0
    if len(argv) != 1:
        print(__doc__)
        return 2
    code, lines = check_root(repo_root())
    print("\n".join(lines))
    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv))
