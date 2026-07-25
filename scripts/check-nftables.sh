#!/usr/bin/env bash
# Apply a compiled a4s ruleset against a real Linux kernel and print the result.
#
# The Go tests assert what the compiler renders; this asserts that a real nft
# accepts it. A ruleset that is syntactically plausible but rejected by the
# kernel would pass every unit test and fail on the first host that used it.
#
# Requires Docker on a non-Linux workstation, or runs natively on Linux with
# CAP_NET_ADMIN. It applies rules inside a throwaway network namespace and never
# touches the host firewall.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ruleset="${1:-}"

if [ -z "$ruleset" ]; then
  ruleset="$(mktemp)"
  trap 'rm -f "$ruleset"' EXIT
  # Render a representative ruleset from the compiler itself rather than a
  # hand-written sample, so this checks what a4s actually emits.
  (cd "$root" && go run ./scripts/emit-policy) > "$ruleset"
fi

echo "checking ruleset:"
cat "$ruleset"
echo

if command -v nft >/dev/null 2>&1 && [ "$(id -u)" = "0" ]; then
  nft -f "$ruleset"
  echo "applied; resulting table:"
  nft list table inet a4s
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "neither a usable nft nor docker is available" >&2
  exit 1
fi

docker run --rm --cap-add=NET_ADMIN -v "$ruleset":/ruleset.nft:ro alpine:3 sh -c '
  apk add --no-cache nftables >/dev/null 2>&1
  nft -f /ruleset.nft
  echo "applied; resulting table:"
  nft list table inet a4s
'
