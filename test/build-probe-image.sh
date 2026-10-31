#!/usr/bin/env bash
# Writes a one-binary image as a docker-archive tar, which RKE2 imports from its images directory.
# Usage: build-probe-image.sh output.tar
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
output="$(realpath "$1")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

(cd "$here/probe" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o "$tmp/probe" .)
tar -C "$tmp" --owner=0 --group=0 --numeric-owner --mtime=@0 -cf "$tmp/layer.tar" probe
layer="$(sha256sum "$tmp/layer.tar" | cut -d' ' -f1)"
cat >"$tmp/config.json" <<EOF
{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/probe"],"User":"65534"},"rootfs":{"type":"layers","diff_ids":["sha256:$layer"]}}
EOF
cat >"$tmp/manifest.json" <<EOF
[{"Config":"config.json","RepoTags":["airgapai.local/probe:v1"],"Layers":["layer.tar"]}]
EOF
tar -C "$tmp" --owner=0 --group=0 --numeric-owner --mtime=@0 -cf "$output" manifest.json config.json layer.tar
