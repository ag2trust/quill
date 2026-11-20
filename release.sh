#!/bin/sh
# release.sh — build the release binaries and their checksums into dist/.
#
# Usage:
#   ./release.sh vX.Y.Z          # or vX.Y.Z-suffix for a pre-release
#
# Writes dist/quill-<os>-<arch> for linux/amd64, linux/arm64 and darwin/arm64
# (static, CGO_ENABLED=0) and dist/SHA256SUMS. The version reported in
# run.started is the tag without its leading "v".
#
# The Release workflow runs this on a pushed tag. Run it locally only to check
# a build; releases are published by the workflow, never by hand.

set -eu

TAG=${1:-}
printf '%s\n' "$TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' || {
  printf 'release.sh: tag must look like v1.2.3 or v1.2.3-rc.1, got: %s\n' "$TAG" >&2
  exit 2
}
VERSION=${TAG#v}

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

rm -rf dist
mkdir dist
for target in linux/amd64 linux/arm64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-X quill/internal/events.Version=$VERSION" \
    -o "dist/quill-$os-$arch" ./cmd/quill
  printf 'built quill-%s-%s\n' "$os" "$arch"
done

cd dist
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum quill-* > SHA256SUMS
else
  shasum -a 256 quill-* > SHA256SUMS
fi
cat SHA256SUMS
