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
 "http":{"address":"127.0.0.1","port":"15000","compat":["docker2s2"]},"log":{"level":"error"}}
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

copy_signed() { # source repo; copies one platform's image by digest and signs it
  "$crane" copy --platform linux/amd64 "$1" "$local_reg/$2:bundle"
  [[ $("$crane" digest "$local_reg/$2:bundle") == "${1#*@}" ]]
  sign "$2" "${1#*@}" "$keys/cosign.key"
}
copy_signed "$FORGEJO_IMAGE" forgejo/forgejo
copy_signed "$ARGOCD_IMAGE" argoproj/argocd
copy_signed "$REDIS_IMAGE" library/redis

# The CI image is built here, so its digest changes with each build, as apk resolves packages then.
cp "$HOME/.local/bin/gitleaks" "$tmp/gitleaks"
[[ $("$tmp/gitleaks" version) == "$GITLEAKS_VERSION" ]]
docker buildx build --quiet --provenance=false --sbom=false --build-arg "BASE=$RUNNER_BASE_IMAGE" \
  --platform linux/amd64 -f "$here/ci-image/Dockerfile" --output "type=oci,dest=$tmp/ci.tar" "$tmp" >/dev/null
mkdir "$tmp/ci-layout"
tar -xf "$tmp/ci.tar" -C "$tmp/ci-layout"
"$crane" push "$tmp/ci-layout" "$local_reg/forgejo/ci-runner:bundle" >/dev/null 2>&1
ci_digest="$("$crane" digest "$local_reg/forgejo/ci-runner:bundle")"
sign forgejo/ci-runner "$ci_digest" "$keys/cosign.key"

# Test images: one signed, one with no signature, one signed by a key the policy does not trust.
"$here/../test/build-probe-image.sh" "$tmp/probe.tar"
# The probe's layer goes in on an empty OCI base, so it needs no Docker-format support of its own.
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
sed -i "s#__FORGEJO_IMAGE__#$target/forgejo/forgejo@${FORGEJO_IMAGE#*@}#" "$tree/bootstrap/forgejo.yaml"
sed -i "s#__RUNNER_IMAGE__#$target/forgejo/ci-runner@$ci_digest#" "$tree/bootstrap/runner.yaml"
fetch "https://raw.githubusercontent.com/argoproj/argo-cd/$ARGOCD_VERSION/manifests/core-install.yaml" argocd-core-install.yaml "$ARGOCD_MANIFEST_SHA256"
sed -E "s#quay\.io/argoproj/argocd:$ARGOCD_VERSION#$target/argoproj/argocd@${ARGOCD_IMAGE#*@}#; s#public\.ecr\.aws/docker/library/redis:[^ ]+#$target/library/redis@${REDIS_IMAGE#*@}#" \
  "$tools/argocd-core-install.yaml" >"$tree/bootstrap/argocd.yaml"
if grep -E '^\s+image: ' "$tree/bootstrap/argocd.yaml" | grep -v "$target/"; then
  echo "argocd manifest uses an image outside the bundle"; exit 1
fi

# The repository as it stands now, without moving HEAD. Only what git would track goes in, and the
# guard scans it first, because the VM has no denylist.
repo="$(git -C "$here" rev-parse --show-toplevel)"
index="$tmp/index"
cp "$(git -C "$repo" rev-parse --path-format=absolute --git-path index)" "$index"
GIT_INDEX_FILE="$index" git -C "$repo" add -A
(cd "$repo" && GIT_INDEX_FILE="$index" scripts/guard.sh staged)
tree_id="$(GIT_INDEX_FILE="$index" git -C "$repo" write-tree)"
commit="$(git -C "$repo" commit-tree "$tree_id" -p HEAD -m "State of the working tree at bundle build")"
git -C "$repo" update-ref refs/heads/lab-bundle "$commit"
git -C "$repo" bundle create --quiet "$tree/bootstrap/lab.bundle" lab-bundle
git -C "$repo" update-ref -d refs/heads/lab-bundle

key="$(sed 's/^/            /' "$keys/cosign.pub")"
python3 -I - "$tree/bootstrap/signed-images.yaml" "$key" <<'PY'
import sys
path, key = sys.argv[1:]
text = open(path).read()
assert "__PUBLIC_KEY__" in text
open(path, "w").write(text.replace("            __PUBLIC_KEY__", key))
PY
