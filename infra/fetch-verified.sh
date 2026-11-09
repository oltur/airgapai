#!/usr/bin/env bash
# Runs on the online host. Downloads URL to FILE unless FILE already has the SHA-256, through a .part
# file that is checked before it replaces anything. Used for the models and the corpus.
# Usage: fetch-verified.sh URL FILE SHA256
set -euo pipefail

url="$1" file="$2" sha="$3"
[[ -f $file ]] && echo "$sha  $file" | sha256sum -c --quiet - 2>/dev/null && exit 0
mkdir -p "$(dirname "$file")"
curl -fsSL --retry 3 -o "$file.part" "$url"
echo "$sha  $file.part" | sha256sum -c --quiet - 2>/dev/null || { rm -f "$file.part"; echo "hash mismatch: $url" >&2; exit 1; }
mv "$file.part" "$file"
