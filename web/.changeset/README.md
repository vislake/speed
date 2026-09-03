# web/.changeset — the npm half of the lockstep release

This directory is the changesets bootstrap of the `web/` pnpm workspace
(the conventional location changesets tooling reads). It exists so that
when the release round lands, changesets has nothing left to invent: the
configuration that encodes the repository's lockstep discipline is already
committed and verified.

## What exists today (M0)

- `config.json` — one **fixed version group** over every deliverable npm
  package of the workspace (`@speed/tokens`, `@speed/i18n`,
  `@speed/ui-kit`), `access: public`, `baseBranch: main`. This is
  configuration only, no version bookkeeping.
- This README. There are deliberately **no changeset entries** — a
  changeset entry is a request for a version bump, and M0 ships no bump:
  every package sits at `0.0.0`, in the same transition state as the Go
  half's `go.mod` files (see the repository release doc).

## The fixed group and why it is the only mode

`docs/internal/02-repo-and-release.md` fixes all npm packages to **one
version number**, shared with the Go half — lockstep versioning. In
changesets terms that means every package must live in a single fixed
group: a fixed group is exactly the mechanism that guarantees packages
are only ever versioned together, so the npm half can never diverge
internally. Any future configuration that lets one package bump alone
would violate the release contract and must not be introduced.

`config.json` lists today's packages. The list is not derived from the
workspace at runtime (changesets cannot do that), so the release
coordinator verifies it instead: `tools/release/lockstep-release.py`
asserts, in its default verification mode, that the fixed group covers
exactly the packages found under `web/packages/` — the npm mirror of the
`go.work` drift guard on the Go side. **When a package joins or leaves
the workspace, update the fixed group in `config.json` in the same
change, or release verification fails loudly.**

## Cross-half alignment

The fixed group keeps the npm half uniform internally; it says nothing
about the Go half. Cross-half alignment — both halves at the SAME version
number — is enforced by the release coordinator, not by changesets: one
version input drives both halves of the plan, and the coordinator's
verification (uniform package versions, fixed-group coverage, one version
per module tag) is what makes a mixed-version release unrepresentable.
Run it from the repository root:

```
python3 tools/release/lockstep-release.py v1.2.0   # offline verification
```

## What is not here yet (waits for the release round at M4)

- changesets itself is **not installed** in `web/` (no `@changesets/cli`
  dependency, no version/changelog script) — nothing in this repository
  runs changesets today, and this M0 round never calls it.
- No `.changeset/*.md` entries exist, and no `CHANGELOG.md` files: there
  has been no release, so there is nothing to record. The release round
  at M4 (the v1.0 release) adds the changesets invocation, generates the
  per-package changelogs, and publishes to the npm registry — wiring
  `publishConfig`, provenance and the `npm publish` step together with
  the Go half's release in the same pipeline.
