#!/usr/bin/env bash
# Runs on the VM as root. Tests Pod Security, default deny and DNS, with controls. Manual step until
# the Go egress test replaces it.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
policy=/var/lib/rancher/rke2/server/manifests/default-deny.yaml
# Digests only, as the signed-image policy requires.
digest="$(curl -sI -H 'Accept: application/vnd.oci.image.manifest.v1+json' http://10.43.0.50:5000/v2/probe/manifests/v1 | awk 'tolower($1) == "docker-content-digest:" {print $2}' | tr -d '\r')"
[[ $digest == sha256:* ]] || { echo "FAIL could not read the probe digest"; exit 1; }
image="10.43.0.50:5000/probe@$digest"
node_ip=10.77.0.10
host_ip=10.77.0.1
api_ip="$(kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}')"
failures=0

result() { # label wanted got
  if [[ $2 == "$3" ]]; then
    echo "ok   $1: $3"
  else
    echo "FAIL $1: wanted $2, got $3"
    failures=$((failures + 1))
  fi
}

pod_yaml() { # namespace name args...; passes the restricted profile
  local ns=$1 name=$2
  shift 2
  cat <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $name, namespace: $ns}
spec:
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: probe
      image: $image
      args: [$(printf '"%s",' "$@")]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
}

admitted() { # namespace yaml; server-side dry run. Any other failure must not count as a rejection.
  local out
  if out="$(printf '%s\n' "$2" | kubectl apply --dry-run=server -n "$1" -f - 2>&1)"; then
    echo admitted
  elif [[ $out == *"violates PodSecurity"* ]]; then
    echo rejected
  else
    echo "error: ${out:0:120}"
  fi
}

probe() { kubectl -n test-a exec client -- /probe "$@" 2>/dev/null; }

kubectl delete ns test-a test-b --ignore-not-found --wait >/dev/null
kubectl create ns test-a >/dev/null
kubectl create ns test-b >/dev/null

echo "== cluster state"
result "SELinux" Enforcing "$(getenforce)"
# Reads the file: ausearch reads stdin instead of the log when stdin is not a terminal
if grep -q type=SERVICE_START /var/log/audit/audit.log; then
  result "SELinux denials in the audit log" 0 "$(grep -c 'avc:.*denied' /var/log/audit/audit.log)"
else
  result "audit log holds records" yes no
fi
result "default-deny installed at boot" default-deny-workloads \
  "$(kubectl get globalnetworkpolicies.crd.projectcalico.org default-deny-workloads -o jsonpath='{.metadata.name}' 2>/dev/null)"
result "CoreDNS has no forward plugin" 0 \
  "$(kubectl -n kube-system get configmap rke2-coredns-rke2-coredns -o jsonpath='{.data.Corefile}' | grep -c forward)"
kubectl label ns test-a kubernetes.io/metadata.name=kube-system --overwrite >/dev/null 2>&1
result "namespace cannot claim an exempt name" test-a "$(kubectl get ns test-a -o jsonpath='{.metadata.labels.kubernetes\.io/metadata\.name}')"

echo "== Pod Security"
compliant="$(pod_yaml test-a dry serve :8080)"
result "compliant pod in a new namespace" admitted "$(admitted test-a "$compliant")"
host_network="$(printf '%s\n' "$compliant" | sed 's/^spec:/spec:\n  hostNetwork: true/')"
result "hostNetwork pod in a new namespace" rejected "$(admitted test-a "$host_network")"
privileged="$(printf '%s\n' "$compliant" | sed 's/allowPrivilegeEscalation: false/privileged: true/')"
result "privileged pod in a new namespace" rejected "$(admitted test-a "$privileged")"
exempt="$(printf '%s\n' "$host_network" | sed 's/namespace: test-a/namespace: kube-system/')"
result "same hostNetwork pod in kube-system (control)" admitted "$(admitted kube-system "$exempt")"

pod_yaml test-a client serve :8081 | kubectl apply -f - >/dev/null
pod_yaml test-b server serve :8080 | kubectl apply -f - >/dev/null
kubectl -n test-a wait --for=condition=Ready pod/client --timeout=120s >/dev/null || result "client ready" Ready no
kubectl -n test-b wait --for=condition=Ready pod/server --timeout=120s >/dev/null || result "server ready" Ready no
peer="$(kubectl -n test-b get pod server -o jsonpath='{.status.podIP}'):8080"

echo "== default deny on, in a namespace created after it"
for target in "$api_ip:443" "$node_ip:6443" "$node_ip:10250" "$host_ip:22" 1.1.1.1:443 "$peer"; do
  result "connect $target" timeout "$(probe connect "$target")"
done
result "cluster name resolves" resolved "$(probe lookup kubernetes.default.svc.cluster.local)"
result "external name does not resolve" dns-failed "$(probe lookup example.com)"

echo "== default deny off (control): only the policy changes"
kubectl delete -f "$policy" >/dev/null
sleep 5
result "connect API service" reached "$(probe connect "$api_ip:443")"
result "connect API on node" reached "$(probe connect "$node_ip:6443")"
result "connect kubelet" reached "$(probe connect "$node_ip:10250")"
result "connect peer pod" reached "$(probe connect "$peer")"
# The lower layers still hold with the policy off, which the formal layer matrix will test fully
result "host SSH, stopped by the host firewall" timeout "$(probe connect "$host_ip:22")"
echo "info public IP: $(probe connect 1.1.1.1:443)"
echo "info external name: $(probe lookup example.com)"

echo "== default deny restored"
kubectl apply -f "$policy" >/dev/null
sleep 5
result "connect API service" timeout "$(probe connect "$api_ip:443")"

kubectl delete ns test-a test-b --wait=false >/dev/null
echo
if [[ $failures != 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all hardening checks passed"
