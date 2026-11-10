# shellcheck shell=bash
# Shared by the in-VM tests that probe network paths from a pod with a chosen app label. The Makefile
# sends this file ahead of each test script, because the script arrives on stdin and cannot source
# files from the host.

# start_probes NAMESPACE NAME=LABEL...: replaces any pods of these names, then waits until they are
# ready. Names should be unique to each test, so one test's pods, still terminating, are never reused.
start_probes() {
  local ns=$1 digest pair names=()
  shift
  digest="$(curl -sI -H 'Accept: application/vnd.oci.image.manifest.v1+json' http://10.43.0.50:5000/v2/probe/manifests/v1 \
    | awk 'tolower($1) == "docker-content-digest:" {print $2}' | tr -d '\r')"
  [[ $digest == sha256:* ]] || { echo "FAIL could not read the probe digest"; return 1; }
  for pair in "$@"; do names+=("${pair%%=*}"); done
  kubectl -n "$ns" delete pod "${names[@]}" --ignore-not-found --wait=true >/dev/null 2>&1
  for pair in "$@"; do
    kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: ${pair%%=*}, namespace: $ns, labels: {app: ${pair#*=}}}
spec:
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: probe
      image: 10.43.0.50:5000/probe@$digest
      args: ["serve", ":8080"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
  done
  kubectl -n "$ns" wait --for=condition=Ready "${names[@]/#/pod/}" --timeout=120s >/dev/null \
    || { echo "FAIL probe pods not ready"; return 1; }
}

# stop_probes NAMESPACE NAME...
stop_probes() {
  local ns=$1
  shift
  kubectl -n "$ns" delete pod "$@" --ignore-not-found --wait=false >/dev/null 2>&1
}

# probe NAMESPACE POD HOST:PORT: prints reached, refused, timeout, no-route or dns-failed.
probe() { kubectl -n "$1" exec "$2" -- /probe connect "$3" 2>/dev/null; }
