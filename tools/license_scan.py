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
third-party requires are all permissive). What is not checked: transitive
(non-direct) dependencies, and the reference app's own dependencies.

Policy:
  * strong copyleft (GPL family, AGPL)           -> FAIL
  * weak copyleft (MPL, LGPL)                     -> FAIL unless the entry
        carries an "adr" field naming a file under docs/ that exists and
        records the adjudication (github.com/hashicorp/vault/api,
        adjudicated by
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
# carries this -- github.com/hashicorp/vault/api, adjudicated by
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
# npm dependency specs that point at workspace packages or local files --
# never registry packages, so they are not part of the delivery list.
# An npm: spec is deliberately NOT skipped: pnpm aliases a real registry
# package under a local name ("copy": "npm:lodash-es@^4.17.21" installs
# lodash-es's package), and the license that ships is lodash-es's -- the
# alias must be adjudicated as the registry package it is, resolved by
# _alias_registry_name below.
NPM_SKIP_SPEC = ("workspace:", "file:", "link:")


def _alias_registry_name(spec):
    """Registry package name an npm: alias spec points at, or None.

    A plain spec (no npm: prefix) has no alias; its declared name IS the
    registry name. An alias spec names the registry package after the
    prefix, optionally with a version range: "npm:lodash-es@^4.17.21"
    aliases lodash-es, "npm:@scope/pkg@^1.0.0" aliases the scoped
    @scope/pkg (the leading '@' belongs to the scope; only a second one
    starts the version), and "npm:lodash-es" aliases lodash-es at its
    default tag.
    """
    if not spec.startswith("npm:"):
        return None
    rest = spec[len("npm:"):]
    if rest.startswith("@"):
        second_at = rest.find("@", 1)
        return rest if second_at == -1 else rest[:second_at]
    at = rest.rfind("@")
    return rest if at == -1 else rest[:at]


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
    and local specs: {declared_name: registry_name}.

    For a plain dependency the declared name IS the registry name. For an
    npm: alias -- "copy": "npm:lodash-es@^4.17.21" -- the declared name
    is the local alias while the registry name (lodash-es) is what the
    manifest must adjudicate: the license that ships is lodash-es's. The
    manifest entry is therefore keyed by registry name in both cases,
    and the lockfile lookup below keys on the declared (alias) name,
    which is what the importer block's key is.
    """
    declared = {}
    with open(package_json_path) as fh:
        data = json.load(fh)
    for section in ("dependencies", "peerDependencies"):
        for name, spec in (data.get(section) or {}).items():
            if any(spec.startswith(p) for p in NPM_SKIP_SPEC):
                continue
            declared[name] = _alias_registry_name(spec) or name
    return declared


def _declared_label(declared_name, registry_name):
    """How an error message names one dependency: the declared key, and
    the aliased registry package when the two differ."""
    if declared_name == registry_name:
        return declared_name
    return f"{declared_name} (an npm: alias of {registry_name})"


def lockfile_resolve(lock_path, importer, declared):
    """Resolve {registry_name: version} for a package.json's declared
    dependencies from the pnpm-lock.yaml importer block of one workspace
    package (lockfile v9 layout). declared maps the package.json key --
    the importer block's key, which for an npm: alias is the local alias
    name -- to the registry name the manifest must adjudicate. The exact
    version is what a frozen-lockfile install yields.

    Returns (resolved, errors): versions found, keyed by registry name;
    error lines for declared keys that cannot be resolved or resolve to
    more than one distinct version."""
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
    resolved: dict[str, set[str]] = {}
    errors: dict[str, list[str]] = {}
    for declared_name in sorted(declared):
        registry_name = declared[declared_name]
        label = _declared_label(declared_name, registry_name)
        quoted = re.escape(declared_name)
        # Declared-name key at six-space indent (inside dependencies /
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
            errors.setdefault(registry_name, []).append(
                f"npm dependency {label} is declared by packages/{importer} "
                f"but pnpm-lock.yaml resolves no version for it -- the "
                f"frozen-lockfile install is broken or the lockfile is stale")
            continue
        # For an aliased dependency pnpm spells the resolved version with
        # the registry name prefixed ("version: lodash-es@4.17.21" under
        # the local alias key -- verified against pnpm v11's own lockfile
        # output), since the importer key is the alias, not the package.
        # Strip that prefix so the manifest version is the plain one; a
        # plain dependency's version never carries a name prefix.
        versions = [
            v[len(registry_name) + 1:] if v.startswith(registry_name + "@")
            else v
            for v in versions
        ]
        distinct = sorted(set(versions))
        if len(distinct) > 1:
            errors.setdefault(registry_name, []).append(
                f"npm dependency {label} resolves to {distinct} in "
                f"pnpm-lock.yaml packages/{importer} -- cannot pin one "
                f"manifest version")
            continue
        resolved.setdefault(registry_name, set()).add(distinct[0])
    problems: list[str] = []
    for registry_name in sorted(errors):
        problems.extend(errors[registry_name])
    return resolved, problems


def npm_expected(web_root, lock_path):
    """Scan every web/packages/*/package.json with dependencies or peers:
    {(registry_name, version): [package dirs...]}, versions resolved from
    the lockfile; an aliased npm: spec is adjudicated under the registry
    package it really is. Returns (expected, errors)."""
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
            lock_path, package, declared)
        errors.extend(resolve_errors)
        package_dir = os.path.join("web", "packages", package)
        for registry_name, versions in resolved.items():
            for version in sorted(versions):
                expected.setdefault(
                    ("npm", registry_name, version), []
                ).append(package_dir)
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
    for (ecosystem, name, version), uses in npm.items():
        expected[(ecosystem, name, version)] = uses
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
