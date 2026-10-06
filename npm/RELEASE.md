# npm release preparation

This package is an installation channel for the native CodeGraph release. It
has no publication workflow and must not be published before the matching
GitHub Release assets are available.

For tag `vX.Y.Z`, in a clean checkout of that exact tag, stage the package
version from the tag without a commit. `npm version` updates `package.json` and
the lockfile locally; discard those two release-prep edits after publication.

```sh
tag="$(git describe --tags --exact-match HEAD)"
version="${tag#v}"
cd npm
npm version --no-git-tag-version "$version"
npm pack --dry-run
npm test
```

The tag's release must contain both the existing human-download archive and
these npm assets for each target:

- `codegraph-vX.Y.Z-linux_amd64` and `codegraph-vX.Y.Z-linux_amd64.sha256`
- `codegraph-vX.Y.Z-linux_arm64` and `codegraph-vX.Y.Z-linux_arm64.sha256`
- `codegraph-vX.Y.Z-darwin_amd64` and `codegraph-vX.Y.Z-darwin_amd64.sha256`
- `codegraph-vX.Y.Z-darwin_arm64` and `codegraph-vX.Y.Z-darwin_arm64.sha256`
- `codegraph-vX.Y.Z-windows_amd64.exe` and its `.sha256` sidecar
- `codegraph-vX.Y.Z-windows_arm64.exe` and its `.sha256` sidecar

Publish only after release asset upload succeeds. The installer requests only
tag `vX.Y.Z` and validates the selected asset against its SHA-256 sidecar before
installing it. If an asset or sidecar is absent, install fails with the package
version, platform and expected tag/asset; it never falls back to another
version. A sidecar from the same GitHub Release is an integrity check, not a
publisher signature.

Before publication, compare `npm view @isink17/codegraph@X.Y.Z version` after
publishing with `git describe --tags --exact-match HEAD` and confirm the
stripped tag equals the package version staged above. Then install that exact
npm version in a clean temporary prefix and run `codegraph --version`.

Package ownership for the `@isink17` scope and either an authorized npm token
or configured npm trusted publisher remain release prerequisites. No credential
or trusted publisher is configured by this implementation.

If `npm publish` reports failure, first check
`npm view @isink17/codegraph@X.Y.Z version`. If the exact version exists,
publication succeeded despite the client error; verify it and do not retry. If
it does not exist, inspect registry state and the publish error, then retry the
same immutable version only after confirming no publication occurred. npm
versions cannot be overwritten; a partially published version must be
recovered by diagnosing registry visibility or publishing a new patch version
through the normal release process, never by retagging different bytes.

The eventual public install flow is `npm install -g @isink17/codegraph`. Do not
add it to the root README until the package is published and public install
smoke passes. CG-26 owns that documentation promotion; CG-28 owns tag-time
release preparation and publication credentials.
