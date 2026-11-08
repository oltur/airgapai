#!/usr/bin/env bash
# Runs on the online host. Fetches the files images/ingest/corpus.lock names into DIRECTORY, keeping
# their paths, and checks each one's SHA-256. Files already there with the right hash are kept.
# Usage: fetch-corpus.sh DIRECTORY
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
lock="$here/../images/ingest/corpus.lock"
dest="$1"
commit="$(awk '$1 == "commit" {print $2}' "$lock")"
[[ $commit =~ ^[0-9a-f]{40}$ ]] || { echo "corpus.lock names no commit"; exit 1; }

grep -E '^[0-9a-f]{64}  ' "$lock" | while read -r sha path; do
  file="$dest/$path"
  [[ -f $file ]] && echo "$sha  $file" | sha256sum -c --quiet - 2>/dev/null && continue
  mkdir -p "$(dirname "$file")"
  curl -fsSL --retry 3 -o "$file.part" "https://raw.githubusercontent.com/kubernetes/website/$commit/$path"
  echo "$sha  $file.part" | sha256sum -c --quiet - || { rm -f "$file.part"; echo "hash mismatch: $path"; exit 1; }
  mv "$file.part" "$file"
done
