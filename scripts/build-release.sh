#!/usr/bin/env bash
# Build release binaries with stamped version metadata and checksums.
#
# Every artifact records the exact commit it came from, and the checksum file
# lets someone verify what they downloaded is what was built. Both exist so a
# report from a running cluster can be traced back to a specific source tree.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

version="${1:-}"
if [ -z "$version" ]; then
  echo "usage: $0 <version>" >&2
  echo "example: $0 0.3.0" >&2
  exit 1
fi

commit="$(git rev-parse HEAD)"
date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

if ! git diff --quiet || ! git diff --cached --quiet; then
  # A release built from a dirty tree cannot be reproduced from its commit,
  # which defeats the reason the commit is stamped in at all.
  echo "refusing to build a release from a modified working tree" >&2
  exit 1
fi

out="$root/dist"
rm -rf "$out"
mkdir -p "$out"

ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date"

# Linux is the only supported target for the node; the operator CLI is useful
# elsewhere, so darwin is built too.
build() {
  local goos="$1" goarch="$2"
  local name="a4s-$version-$goos-$goarch"
  echo "building $name"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$ldflags" -o "$out/$name" ./cmd/a4s
}

build linux amd64
build linux arm64
build darwin amd64
build darwin arm64

(
  cd "$out"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum a4s-* > SHA256SUMS
  else
    shasum -a 256 a4s-* > SHA256SUMS
  fi
)

echo
echo "built $version from $commit"
cat "$out/SHA256SUMS"
echo
echo "to sign the checksums:"
echo "  gpg --detach-sign --armor $out/SHA256SUMS"
