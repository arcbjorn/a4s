#!/usr/bin/env bash
# Verify every relative Markdown link and image in the project resolves to a
# file that exists.
#
# Documentation is part of the protocol here, and the project is meant to stay
# portable when the folder is copied without its parent. A link that escapes
# the project root or points at a deleted file breaks both properties, so this
# runs in CI rather than relying on review to catch it.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

failures=0

# Markdown inline links and images: [text](target) and ![alt](target).
# Skip absolute URLs, protocol-relative URLs, mailto, and pure anchors.
while IFS= read -r file; do
  while IFS= read -r target; do
    [ -n "$target" ] || continue
    case "$target" in
      http://*|https://*|//*|mailto:*|\#*) continue ;;
    esac

    # Strip any fragment; a link to doc.md#section resolves to doc.md.
    path="${target%%#*}"
    [ -n "$path" ] || continue

    resolved="$(cd "$(dirname "$file")" && printf '%s/%s' "$(pwd)" "$path")"

    if [ ! -e "$resolved" ]; then
      printf 'broken link: %s -> %s\n' "$file" "$target"
      failures=$((failures + 1))
      continue
    fi

    # A link that resolves outside the project root defeats portability even
    # though the file exists on this machine.
    real="$(cd "$(dirname "$resolved")" 2>/dev/null && pwd -P)/$(basename "$resolved")"
    case "$real" in
      "$root"/*) ;;
      *)
        printf 'link escapes project root: %s -> %s\n' "$file" "$target"
        failures=$((failures + 1))
        ;;
    esac
  done < <(grep -oE '!?\[[^]]*\]\([^)]+\)' "$file" | sed -E 's/.*\(([^)]+)\).*/\1/')
done < <(find . -name '*.md' -not -path './.git/*')

# HTML img tags, which the README uses for the architecture diagram.
while IFS= read -r file; do
  while IFS= read -r target; do
    [ -n "$target" ] || continue
    case "$target" in
      http://*|https://*|//*) continue ;;
    esac
    resolved="$(cd "$(dirname "$file")" && printf '%s/%s' "$(pwd)" "$target")"
    if [ ! -e "$resolved" ]; then
      printf 'broken image: %s -> %s\n' "$file" "$target"
      failures=$((failures + 1))
    fi
  done < <(grep -oE '<img[^>]+src="[^"]+"' "$file" | sed -E 's/.*src="([^"]+)".*/\1/')
done < <(find . -name '*.md' -not -path './.git/*')

if [ "$failures" -gt 0 ]; then
  printf '\n%d broken link(s)\n' "$failures"
  exit 1
fi

printf 'all relative documentation links resolve\n'
