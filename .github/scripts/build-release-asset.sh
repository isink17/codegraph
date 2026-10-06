#!/usr/bin/env bash
set -euo pipefail

version=${1:?usage: build-release-asset.sh TAG GOOS GOARCH ARCHIVE_EXT [DIST]}
goos=${2:?usage: build-release-asset.sh TAG GOOS GOARCH ARCHIVE_EXT [DIST]}
goarch=${3:?usage: build-release-asset.sh TAG GOOS GOARCH ARCHIVE_EXT [DIST]}
archive_ext=${4:?usage: build-release-asset.sh TAG GOOS GOARCH ARCHIVE_EXT [DIST]}
dist=${5:-dist}

[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
case "$goos/$goarch/$archive_ext" in
  linux/amd64/tar.gz|linux/arm64/tar.gz|darwin/amd64/tar.gz|darwin/arm64/tar.gz|windows/amd64/zip|windows/arm64/zip) ;;
  *) echo "unsupported release target: $goos/$goarch/$archive_ext" >&2; exit 1 ;;
esac

if [[ "$goos/$goarch" == windows/arm64 ]]; then
  [[ "$(go env CGO_ENABLED)" == 1 ]] || { echo "Windows ARM64 release requires CGO_ENABLED=1" >&2; exit 1; }
  [[ "$(go env CC)" == *aarch64-w64-mingw32-clang* ]] || { echo "Windows ARM64 release requires the verified llvm-mingw CC" >&2; exit 1; }
  [[ "$(go env CXX)" == *aarch64-w64-mingw32-clang++* ]] || { echo "Windows ARM64 release requires the verified llvm-mingw CXX" >&2; exit 1; }
fi

binary=codegraph
binary_name=codegraph
[[ "$goos" == windows ]] && binary_name=codegraph.exe
archive_base="codegraph_${version}_${goos}_${goarch}"
mkdir -p "$dist/$archive_base"
GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=1 go build -v -x \
  -ldflags "-X github.com/isink17/codegraph/internal/version.Version=$version" \
  -o "$dist/$archive_base/$binary_name" ./cmd/codegraph

actual=""
if [[ "$goos/$goarch" == "$(go env GOOS)/$(go env GOARCH)" ]]; then
  actual=$("$dist/$archive_base/$binary_name" --version)
fi
expected="codegraph $version"
if [[ -n "$actual" && "$actual" != "$expected" ]]; then
  echo "version mismatch: got $actual, want $expected" >&2
  exit 1
fi
cp README.md LICENSE "$dist/$archive_base/"

native_asset="codegraph-${version}-${goos}_${goarch}"
[[ "$goos" == windows ]] && native_asset+=".exe"
cp "$dist/$archive_base/$binary_name" "$dist/$native_asset"

case "$archive_ext" in
  zip)
    if [[ "$goos" == windows ]]; then
      powershell.exe -NoProfile -NonInteractive -Command "Compress-Archive -Path '$dist/$archive_base/*' -DestinationPath '$dist/$archive_base.zip' -Force"
    else
      (cd "$dist/$archive_base" && zip -q -r "../$archive_base.zip" .)
    fi
    archive="$dist/$archive_base.zip"
    ;;
  tar.gz)
    tar -C "$dist" -czf "$dist/$archive_base.tar.gz" "$archive_base"
    archive="$dist/$archive_base.tar.gz"
    ;;
esac

sha256() {
  node -e 'const p=process.argv[1]; const d=require("node:crypto").createHash("sha256").update(require("node:fs").readFileSync(p)).digest("hex"); process.stdout.write(`${d}  ${p}\n`)' "$1"
}
sha256 "$archive" >"$archive.sha256"
sha256 "$dist/$native_asset" | awk '{print $1}' >"$dist/$native_asset.sha256"
[[ -s "$archive" && -s "$archive.sha256" && -s "$dist/$native_asset" && -s "$dist/$native_asset.sha256" ]]
