# npm release preparation

`@isink17/codegraph` is intended to be a public scoped package. An npm account
whose username is exactly `isink17` automatically owns the `@isink17` user
scope; the scope is not separately claimed. Before first package creation, the
owner must confirm that the `isink17` account exists, account-level 2FA is
enabled, and the owner can authenticate with package write access. If the npm
username is not `isink17`, stop: resolve scope ownership before release. Do not
change this package identity automatically.

The package has no publishing workflow. Do not publish until the matching
GitHub Release is public and all six native assets and checksum sidecars are
available and verified.

## First release: v2.0.0 bootstrap

A new package cannot use npm Trusted Publishing for its initial creation:
Trusted Publisher setup requires an existing package. Create v2.0.0 through
staged publishing with the authenticated owner, then configure Trusted
Publishing for later releases.

1. Create and push the approved `v2.0.0` git tag. Do not retag different
   release bytes.
2. Let the GitHub release workflow build and test all six native targets.
3. Confirm the GitHub Release is public and its complete asset set is uploaded.
4. From a clean checkout of that exact `v2.0.0` tag, stage npm package version
   `2.0.0` locally. This edits `npm/package.json` and `npm/package-lock.json`;
   do not commit these release-preparation version edits. Revert them after
   publication if the checkout is kept.
5. Complete every pre-publication check below, including the six-asset checksum
   guard.
6. The authenticated owner runs `npm stage publish`, not `npm publish`.
7. Inspect the staged package and compare it with the verified GitHub assets.
8. The owner approves the staged package with 2FA only after that comparison
   passes.
9. Run the public npm install smoke below.
10. Configure the future Trusted Publisher as described below.
11. CG-26 promotes npm as the recommended README install after public install
    smoke passes.

Do not perform these release steps as part of metadata preparation.

### Pre-publication checks

Run from a clean checkout of the exact tag. Confirm the tag and stage the
package version from it:

```sh
test -z "$(git status --porcelain)"
git describe --tags --exact-match HEAD
# Must print v2.0.0 for the first release.
cd npm
npm version --no-git-tag-version --ignore-scripts 2.0.0
npm test
npm pack --dry-run
bash ../.github/scripts/verify-release-assets.sh package.json v2.0.0 ../release-dist
```

`release-dist` must contain the downloaded assets from the public GitHub
Release. The guard requires the package version to match the tag, the six exact
native asset names, and valid matching SHA-256 sidecars. It rejects missing,
empty, unexpected, or mismatched assets. Confirm the packed contents remain
minimal. Do not stage until the public Release and guard both pass.

Required assets:

- `codegraph-v2.0.0-linux_amd64` and `.sha256`
- `codegraph-v2.0.0-linux_arm64` and `.sha256`
- `codegraph-v2.0.0-darwin_amd64` and `.sha256`
- `codegraph-v2.0.0-darwin_arm64` and `.sha256`
- `codegraph-v2.0.0-windows_amd64.exe` and `.sha256`
- `codegraph-v2.0.0-windows_arm64.exe` and `.sha256`

The GitHub Release may also include the existing human-download archive; the
parity guard requires these six native assets and sidecars.

The installer requests only the matching tag asset and validates its bytes
against that release's SHA-256 sidecar. It fails with the package version,
platform, and expected tag/asset if either is absent; it never falls back to
another version. A sidecar from the same GitHub Release is an integrity check,
not a publisher signature.

### Staged publishing and owner approval

`npm stage publish` creates the package if it does not yet exist and reserves
the staged package version in the registry. It is not publicly installable.
Approval publishes it and requires 2FA. Staged and published versions share a
unique version index. Package versions are immutable: never overwrite a version,
reuse it for different bytes, or retry blindly with changed bytes.

Discover the stage ID from the package's list; do not copy a memorized ID:

```sh
npm stage list @isink17/codegraph
npm stage view <stage-id>
# Optional: run from a temporary inspection directory.
npm stage download <stage-id>
npm stage approve <stage-id>
# If validation fails, reject instead of approving.
npm stage reject <stage-id>
```

The owner must review the stage and approve it with 2FA. Reject also requires
2FA. Do not approve before confirming that all matching GitHub Release assets
are public, complete, and verified.

Use an npm CLI version that implements staged publishing. Confirm
`npm stage --help` works before release preparation; an older CLI may report
`Unknown command: "stage"`.

### Post-approval checks

Only after approval, verify the exact public version and install it into a clean
global prefix:

```sh
prefix="$(mktemp -d)"
npm view @isink17/codegraph@2.0.0 version
npm install --global --prefix "$prefix" @isink17/codegraph@2.0.0
export PATH="$prefix/bin:$PATH"
codegraph --version
codegraph doctor
```

Confirm `codegraph --version` reports `codegraph v2.0.0`. An optional MCP smoke
may follow. Do not add npm to the root README until this public install smoke
passes; CG-26 owns that documentation promotion.

### First-release recovery

- **Stage creation fails before a version exists:** run `npm stage list
  @isink17/codegraph` and `npm view @isink17/codegraph@2.0.0 version`. Diagnose
  the authentication, access, or request failure. Retry only after confirming
  neither a staged nor published `2.0.0` exists, and use the same verified
  package bytes.
- **Stage succeeds but validation fails:** do not approve. Inspect the stage
  with `npm stage view` and, if useful, download it. If package bytes are
  wrong, reject the stage. Do not reuse `2.0.0` for different bytes; correct
  the release under a new version and matching tag.
- **Owner rejects the stage:** rejection removes that staged package. Diagnose
  the reason before another attempt. Never submit different bytes as the same
  version; a corrected package needs a new version and matching release tag.
- **Approval succeeds but the client reports failure:** check
  `npm view @isink17/codegraph@2.0.0 version` and `npm stage list
  @isink17/codegraph`. If the exact public version exists, approval succeeded;
  do not approve or publish again. If it remains staged, inspect it and the
  approval error before any retry. If registry state is uncertain, stop and
  resolve it before retrying.
- **GitHub assets become unavailable between staging and approval:** do not
  approve. Restore availability of the same verified release assets or keep
  the stage pending while investigating. If the release cannot be validated,
  reject the stage and use a new version/tag for corrected release bytes. Never
  retag different bytes.
- **Public install smoke fails after approval:** the npm version is already
  immutable. Diagnose the install or platform failure. Corrected package bytes
  require a new patch version and matching release; never overwrite the
  approved version.

If GitHub Release creation or upload fails, the release workflow leaves an
unpublished draft that blocks automatic reruns. Inspect the draft and
artifacts, confirm it was never public, then delete the incomplete draft before
rerunning. Never replace bytes in a published Release. The workflow refuses an
existing Release and never uses `--clobber`.

## Future releases: Trusted Publishing with OIDC

After `@isink17/codegraph` exists, configure an npm Trusted Publisher for:

- GitHub owner/user: `isink17`
- Repository: `codegraph`
- Exact publishing workflow filename: decide and create this filename before
  configuring the publisher

Prefer GitHub-hosted Actions with OIDC. npm Trusted Publishing requires Node
`>=22.14`, npm `>=11.5.1`, a GitHub-hosted runner, and workflow permission
`id-token: write`. No `NPM_TOKEN` is needed. Do not add a publishing workflow
or configure npm settings in this preparation wave.

For maximum owner control, grant the trust relationship staged publishing
only. Configure it to allow `npm stage publish` and not direct `npm publish`.
Future CI stages the package; the owner reviews the staged package and approves
it with 2FA. Keep final publication owner-controlled unless a concrete reason
supports allowing direct publish.
