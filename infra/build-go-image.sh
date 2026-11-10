#!/usr/bin/env bash
# Runs on the online host. Builds an image of one static Go program from images/NAME, on an empty
# base, with an optional second layer of data. The program is built with the exact toolchain its
# go.mod names, and owners, modes and times are fixed, so the digest depends only on the inputs and
# Git can name it. Runs as user 65534. Prints the digest.
# Usage: build-go-image.sh crane NAME registry/repository:tag [data-layer.tar]
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
crane="$1" name="$2" ref="$3" data="${4:-}"
src="$here/../images/$name"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/bin"
# A newer local Go would build different bytes.
toolchain="$(awk '$1 == "toolchain" {print $2}' "$src/go.mod")"
(cd "$src" && GOTOOLCHAIN="$toolchain" GOFLAGS=-mod=readonly CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$tmp/bin/$name" .)
tar -C "$tmp/bin" --owner=0 --group=0 --numeric-owner --mtime=@0 --mode=0555 -cf "$tmp/bin.tar" "$name"
layers=(-f "$tmp/bin.tar")
[[ -n $data ]] && layers+=(-f "$data")
quiet() { "$@" >"$tmp/crane.log" 2>&1 || { cat "$tmp/crane.log" >&2; echo "failed: $*" >&2; exit 1; }; }
quiet "$crane" append --oci-empty-base "${layers[@]}" -t "$ref"
quiet "$crane" mutate --set-platform linux/amd64 --entrypoint "/$name" --user 65534 -t "$ref" "$ref"
"$crane" digest "$ref"
