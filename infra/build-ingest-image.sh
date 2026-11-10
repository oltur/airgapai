#!/usr/bin/env bash
# Runs on the online host. Builds the ingestion image: the ingest program, through build-go-image.sh,
# and a layer of the corpus files images/ingest/corpus.lock names, with a licence notice. Fixed owners,
# modes, times and order make the digest depend only on the inputs, so Git can name it. Prints the
# digest.
# Usage: build-ingest-image.sh crane corpus-directory registry/repository:tag
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
crane="$1"
corpus="$(realpath "$2")"
ref="$3"
lock="$here/../images/ingest/corpus.lock"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/data/corpus"
commit="$(awk '$1 == "commit" {print $2}' "$lock")"
grep -E '^[0-9a-f]{64}  ' "$lock" | while read -r sha path; do
  # Messages go to stderr: this loop's output is the file list.
  echo "$sha  $corpus/$path" | sha256sum -c --quiet - 2>/dev/null || { echo "corpus file differs from the lock: $path" >&2; exit 1; }
  mkdir -p "$(dirname "$tmp/data/corpus/$path")"
  cp "$corpus/$path" "$tmp/data/corpus/$path"
  echo "corpus/$path"
done >"$tmp/files"
cat >"$tmp/data/NOTICE" <<EOF
The files under /corpus are from the Kubernetes documentation, github.com/kubernetes/website,
commit $commit, by The Kubernetes Authors, licensed under CC BY 4.0:
https://creativecommons.org/licenses/by/4.0/. They are unchanged here. The ingest program removes
markup and cuts them into chunks when it reads them.
EOF
echo NOTICE >>"$tmp/files"
LC_ALL=C sort -o "$tmp/files" "$tmp/files"

# No directory entries: the runtime creates the parents. Modes are set in the archive.
tar -C "$tmp/data" --owner=0 --group=0 --numeric-owner --mtime=@0 --mode=0444 --no-recursion \
  -T "$tmp/files" -cf "$tmp/data.tar"
"$here/build-go-image.sh" "$crane" ingest "$ref" "$tmp/data.tar"
