#!/usr/bin/env bash
# Runs on the online host. Builds the seed the air-gapped Zot serves: images copied by digest and
# signed with a key that stays on this host. Zot itself writes the tree, so the layout is its own.
# Usage: build-registry.sh bundle-tree
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
source "$here/versions.env"
tree="$(realpath "$1")"
tools="$DATA_DIR/downloads/tools"
keys="$DATA_DIR/keys"
local_reg=127.0.0.1:15000
target="$REGISTRY_IP:5000"
tmp="$(mktemp -d)"
zot_pid=""
cleanup() { [[ -n $zot_pid ]] && kill "$zot_pid" 2>/dev/null || true; rm -rf "$tmp"; }
trap cleanup EXIT

fetch() { # url file sha256
  [[ -f $tools/$2 ]] && echo "$3  $tools/$2" | sha256sum -c --quiet - 2>/dev/null && return
  curl -fsSL -o "$tools/$2" "$1"
  echo "$3  $tools/$2" | sha256sum -c --quiet -
}
mkdir -p "$tools"
fetch "https://github.com/sigstore/cosign/releases/download/$COSIGN_VERSION/cosign-linux-amd64" cosign "$COSIGN_SHA256"
fetch "https://github.com/google/go-containerregistry/releases/download/$CRANE_VERSION/go-containerregistry_Linux_x86_64.tar.gz" gcr.tgz "$CRANE_SHA256"
fetch "https://get.helm.sh/helm-$HELM_VERSION-linux-amd64.tar.gz" helm.tgz "$HELM_SHA256"
fetch "https://kyverno.github.io/kyverno/kyverno-$KYVERNO_CHART_VERSION.tgz" kyverno.tgz "$KYVERNO_CHART_SHA256"
tar -xzf "$tools/gcr.tgz" -C "$tools" crane
tar -xzf "$tools/helm.tgz" -C "$tools" --strip-components=1 linux-amd64/helm
chmod +x "$tools/cosign" "$tools/crane" "$tools/helm"
crane="$tools/crane"
cosign="$tools/cosign"

# The private key never enters the bundle. An empty password is enough here: the file is the secret.
export COSIGN_PASSWORD=""
mkdir -p "$keys"
chmod 700 "$keys"
[[ -f $keys/cosign.key ]] || (cd "$keys" && "$cosign" generate-key-pair >/dev/null 2>&1)
(cd "$tmp" && "$cosign" generate-key-pair >/dev/null 2>&1 && mv cosign.key other.key && rm cosign.pub)

# Zot's image runs here as a plain process, so the seed is written by the same version that serves it.
"$crane" export "$ZOT_IMAGE" - | tar -xf - -C "$tmp" usr/local/bin/zot-linux-amd64
rm -rf "$tree/registry"
mkdir -p "$tree/registry"
cat >"$tmp/zot.json" <<JSON
{"distSpecVersion":"1.1.1","storage":{"rootDirectory":"$tree/registry","gc":false,"dedupe":false},
 "http":{"address":"127.0.0.1","port":"15000"},"log":{"level":"error"}}
JSON
"$tmp/usr/local/bin/zot-linux-amd64" serve "$tmp/zot.json" >"$tmp/zot.log" 2>&1 &
zot_pid=$!
for _ in $(seq 30); do curl -fs "http://$local_reg/v2/" >/dev/null && break; sleep 1; done
curl -fs "http://$local_reg/v2/" >/dev/null || { cat "$tmp/zot.log"; echo "seed registry did not start"; exit 1; }

sign() { # repo digest key
  "$cosign" sign --yes --key "$3" --use-signing-config=false --new-bundle-format=false --tlog-upload=false --allow-http-registry \
    --sign-container-identity "$target/$1@$2" "$local_reg/$1@$2" >"$tmp/sign.log" 2>&1 \
    || { cat "$tmp/sign.log"; echo "signing $1 failed"; exit 1; }
}
"$crane" copy --platform linux/amd64 "$KYVERNO_IMAGE" "$local_reg/kyverno/kyverno:bundle"
"$crane" copy --platform linux/amd64 "$KYVERNOPRE_IMAGE" "$local_reg/kyverno/kyvernopre:bundle"
[[ $("$crane" digest "$local_reg/kyverno/kyverno:bundle") == "${KYVERNO_IMAGE#*@}" ]]
[[ $("$crane" digest "$local_reg/kyverno/kyvernopre:bundle") == "${KYVERNOPRE_IMAGE#*@}" ]]
sign kyverno/kyverno "${KYVERNO_IMAGE#*@}" "$keys/cosign.key"
sign kyverno/kyvernopre "${KYVERNOPRE_IMAGE#*@}" "$keys/cosign.key"

# Test images: one signed, one with no signature, one signed by a key the policy does not trust.
"$here/../test/build-probe-image.sh" "$tmp/probe.tar"
# Zot accepts only OCI manifests, so the probe's layer goes in on an empty OCI base.
tar -xf "$tmp/probe.tar" -C "$tmp" layer.tar
"$crane" append --oci-empty-base -f "$tmp/layer.tar" -t "$local_reg/probe:v1" >/dev/null 2>&1
"$crane" mutate --entrypoint /probe --user 65534 -t "$local_reg/probe:v1" "$local_reg/probe:v1" >/dev/null 2>&1
probe="$("$crane" digest "$local_reg/probe:v1")"
sign probe "$probe" "$keys/cosign.key"
"$crane" mutate --label airgapai.test=unsigned -t "$local_reg/probe-unsigned:v1" "$local_reg/probe:v1" >/dev/null
"$crane" mutate --label airgapai.test=other-key -t "$local_reg/probe-other-key:v1" "$local_reg/probe:v1" >/dev/null
sign probe-other-key "$("$crane" digest "$local_reg/probe-other-key:v1")" "$tmp/other.key"

"$cosign" verify --key "$keys/cosign.pub" --insecure-ignore-tlog --allow-http-registry "$local_reg/probe@$probe" >/dev/null 2>&1 \
  || { echo "signature does not verify"; exit 1; }
if "$cosign" verify --key "$keys/cosign.pub" --insecure-ignore-tlog --allow-http-registry "$local_reg/probe-other-key:v1" >/dev/null 2>&1; then
  echo "a signature from another key verified"; exit 1
fi

kill "$zot_pid"; wait "$zot_pid" 2>/dev/null || true; zot_pid=""
rm -rf "$tree/registry/.uploads" "$tree/registry/_trivy"
find "$tree/registry" -name .uploads -type d -prune -exec rm -rf {} +

# Zot cannot pull itself from itself, so it loads into containerd as an archive under a fixed name.
"$crane" pull --format=tarball "$ZOT_IMAGE" "$tmp/zot.tar"
mkdir "$tmp/zot-archive"
tar -xf "$tmp/zot.tar" -C "$tmp/zot-archive"
python3 -I - "$tmp/zot-archive/manifest.json" <<'PY'
import json, sys
path = sys.argv[1]
manifest = json.load(open(path))
manifest[0]["RepoTags"] = ["airgapai.local/zot:bundle"]
json.dump(manifest, open(path, "w"))
PY
(cd "$tmp/zot-archive" && tar --owner=0 --group=0 --numeric-owner -cf "$tree/zot.tar" -- *)

mkdir -p "$tree/bootstrap"
"$tools/helm" template kyverno "$tools/kyverno.tgz" --namespace kyverno --no-hooks \
  --set backgroundController.enabled=false,cleanupController.enabled=false,reportsController.enabled=false,admissionController.replicas=1 \
  | sed -E "s#image: \"reg\.kyverno\.io/kyverno/kyverno:[^\"]+\"#image: \"$target/kyverno/kyverno@${KYVERNO_IMAGE#*@}\"#; s#image: \"reg\.kyverno\.io/kyverno/kyvernopre:[^\"]+\"#image: \"$target/kyverno/kyvernopre@${KYVERNOPRE_IMAGE#*@}\"#" \
  >"$tree/bootstrap/kyverno.yaml"
# CRD schemas also have fields called image, with no value after them.
if grep -E '^\s+image: ' "$tree/bootstrap/kyverno.yaml" | grep -v "$target/"; then
  echo "kyverno manifest uses an image outside the bundle"; exit 1
fi
key="$(sed 's/^/            /' "$keys/cosign.pub")"
python3 -I - "$tree/bootstrap/signed-images.yaml" "$key" <<'PY'
import sys
path, key = sys.argv[1:]
text = open(path).read()
assert "__PUBLIC_KEY__" in text
open(path, "w").write(text.replace("            __PUBLIC_KEY__", key))
PY
