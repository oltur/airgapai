# shellcheck shell=bash
# Shared by the in-VM tests that run a copy of the deployed gateway beside it: test/lib/stub.sh and
# test/eval/generation.sh. The copy is the deployed Deployment with its name, a component label the
# gateway Service does not select, the given environment values and, if one is given, its image
# changed. Its secrets, other settings and network label are the deployed gateway's, so the same
# network rules apply to it. Argo CD does not track it, so it never syncs or prunes it. Sent ahead of
# each test script, like probe.sh.

# Usage: gateway_copy NAME COMPONENT IMAGE|"" KEY=VALUE...
gateway_copy() {
  local name="$1" component="$2" image="$3"
  shift 3
  kubectl -n gateway get deployment gateway -o json | python3 -I -c '
import json, sys
name, component, image = sys.argv[1:4]
changes = dict(a.split("=", 1) for a in sys.argv[4:])
d = json.load(sys.stdin)
d["metadata"] = {"name": name, "namespace": d["metadata"]["namespace"]}
d.pop("status", None)
spec = d["spec"]
spec["selector"]["matchLabels"]["component"] = component
labels = spec["template"]["metadata"]["labels"]
labels["component"] = component
assert labels["app"] == "gateway"
spec["template"]["metadata"].pop("annotations", None)
containers = spec["template"]["spec"]["containers"]
assert len(containers) == 1
if image:
    containers[0]["image"] = image
for e in containers[0]["env"]:
    if e["name"] in changes:
        e["value"] = changes.pop(e["name"])
assert not changes, changes
print(json.dumps(d))' "$name" "$component" "$image" "$@" | kubectl apply -f - >/dev/null
}

# Waited for, so a test started straight after cannot reach this copy's pods while they stop.
# Usage: delete_gateway_copy NAME COMPONENT
delete_gateway_copy() {
  kubectl -n gateway delete deployment "$1" --ignore-not-found --timeout=120s >/dev/null 2>&1
  kubectl -n gateway wait --for=delete pod -l "app=gateway,component=$2" --timeout=120s >/dev/null 2>&1
}

# The digest of a test image in the in-cluster registry, or nothing.
# Usage: registry_digest REPOSITORY TAG
registry_digest() {
  curl -sI -H 'Accept: application/vnd.oci.image.manifest.v1+json' "http://10.43.0.50:5000/v2/$1/manifests/$2" \
    | awk 'tolower($1) == "docker-content-digest:" {print $2}' | tr -d '\r'
}

# Forwards PORT to a copy and waits until it is ready. Sets copy_forward to the forward's PID.
# Usage: forward_gateway_copy NAME PORT
# shellcheck disable=SC2034 # read by the scripts sent after this one
copy_forward=""

forward_gateway_copy() {
  kubectl -n gateway port-forward "deployment/$1" "$2:8080" >/dev/null 2>&1 &
  # shellcheck disable=SC2034 # as above
  copy_forward=$!
  for _ in $(seq 30); do curl -fs "http://127.0.0.1:$2/ready" >/dev/null && return 0; sleep 1; done
  return 1
}

# A label of a test image in the in-cluster registry, or nothing.
# Usage: registry_label REPOSITORY TAG LABEL
registry_label() {
  local config
  config="$(curl -s -H 'Accept: application/vnd.oci.image.manifest.v1+json' "http://10.43.0.50:5000/v2/$1/manifests/$2" \
    | python3 -I -c 'import json, sys; print(json.load(sys.stdin)["config"]["digest"])' 2>/dev/null)"
  [[ $config == sha256:* ]] || return 0
  curl -s "http://10.43.0.50:5000/v2/$1/blobs/$config" \
    | python3 -I -c 'import json, sys; print((json.load(sys.stdin)["config"].get("Labels") or {}).get(sys.argv[1], ""))' "$3" 2>/dev/null
}
