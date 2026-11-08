#!/usr/bin/env bash
# Runs on the online host. Builds the ingestion image: the ingest program in one layer, and the corpus
# files images/ingest/corpus.lock names, with a licence notice, in another, on an empty base. Fixed
# owners, modes, times and order make the digest depend only on the inputs, so Git can name it.
# Prints the digest.
# Usage: build-ingest-image.sh crane corpus-directory registry/repository:tag
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
crane="$1"
corpus="$(realpath "$2")"
ref="$3"
src="$here/../images/ingest"
lock="$src/corpus.lock"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/bin" "$tmp/data/corpus"
# The toolchain named in go.mod, exactly: a newer local Go would build different bytes.
toolchain="$(awk '$1 == "toolchain" {print $2}' "$src/go.mod")"
(cd "$src" && GOTOOLCHAIN="$toolchain" GOFLAGS=-mod=readonly CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$tmp/bin/ingest" .)

commit="$(awk '$1 == "commit" {print $2}' "$lock")"
grep -E '^[0-9a-f]{64}  ' "$lock" | while read -r sha path; do
  echo "$sha  $corpus/$path" | sha256sum -c --quiet - || { echo "corpus file differs from the lock: $path"; exit 1; }
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
tar -C "$tmp/bin" --owner=0 --group=0 --numeric-owner --mtime=@0 --mode=0555 -cf "$tmp/bin.tar" ingest
tar -C "$tmp/data" --owner=0 --group=0 --numeric-owner --mtime=@0 --mode=0444 --no-recursion \
  -T "$tmp/files" -cf "$tmp/data.tar"
quiet() { "$@" >"$tmp/crane.log" 2>&1 || { cat "$tmp/crane.log" >&2; echo "failed: $*" >&2; exit 1; }; }
quiet "$crane" append --oci-empty-base -f "$tmp/bin.tar" -f "$tmp/data.tar" -t "$ref"
quiet "$crane" mutate --set-platform linux/amd64 --entrypoint /ingest --user 65534 -t "$ref" "$ref"
"$crane" digest "$ref"
