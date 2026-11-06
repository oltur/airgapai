#!/usr/bin/env bash
# Runs on the online host. Builds a model image: the model-copy program and one GGUF file, each in its
# own layer, on an empty base. Fixed owners, modes and times make the digest depend only on the
# inputs, so Git can name it. Prints the digest.
# Usage: build-model-image.sh crane model.gguf registry/repository:tag
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
crane="$1"
model="$(realpath "$2")"
ref="$3"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/copy" "$tmp/model"
(cd "$here/../images/model-copy" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -ldflags='-s -w' -o "$tmp/copy/model-copy" .)
ln "$model" "$tmp/model/model.gguf" 2>/dev/null || cp "$model" "$tmp/model/model.gguf"
# Modes are set in the archive, so a hard-linked source keeps its own. No directory entries are needed.
layer() { # directory file mode
  tar -C "$tmp/$1" --owner=0 --group=0 --numeric-owner --mtime=@0 --mode="$3" -cf "$tmp/$1.tar" "$2"
}
layer copy model-copy 0555
layer model model.gguf 0444
"$crane" append --oci-empty-base -f "$tmp/copy.tar" -f "$tmp/model.tar" -t "$ref" >/dev/null 2>&1
"$crane" mutate --set-platform linux/amd64 --entrypoint /model-copy,/model.gguf --user 65534 -t "$ref" "$ref" >/dev/null 2>&1
"$crane" digest "$ref"
