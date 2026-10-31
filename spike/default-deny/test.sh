#!/usr/bin/env bash
# Runs on the spike VM as root, with policy.yaml beside it. Probes with curl and judges by exit code:
#   28 timeout, the only result counted as blocked, since a dropped packet gives no answer
#   7 refused, 6 DNS failure: neither counts as blocked, so a closed port cannot pass as a policy
#   0 reached (-k accepts any certificate, and any HTTP status is still a connection)
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
image=curlimages/curl:8.17.0
here="$(cd "$(dirname "$0")" && pwd)"
failures=0

node_ip="$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
gateway="$(ip route | awk '/^default/ {print $3; exit}')"
api_ip="$(kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}')"

pod() { # namespace name
  kubectl -n "$1" run "$2" --image="$image" --restart=Never --command -- sleep 3600 >/dev/null
  kubectl -n "$1" wait --for=condition=Ready "pod/$2" --timeout=180s >/dev/null
}

code() { # namespace pod url
  local rc=0
  kubectl -n "$1" exec "$2" -- curl -ks -o /dev/null -m 5 "$3" >/dev/null 2>&1 || rc=$?
  echo "$rc"
}

check() { # label wanted namespace pod url; wanted is a code, or "any" to only record it
  local got
  got="$(code "$3" "$4" "$5")"
  if [[ $2 == any ]]; then
    echo "info $1: exit $got"
  elif [[ $got == "$2" ]]; then
    echo "ok   $1: exit $got"
  else
    echo "FAIL $1: wanted $2, got $got"
    failures=$((failures + 1))
  fi
}

targets() { # namespace pod wanted_internet wanted_api
  check "$1 public IP" "$3" "$1" "$2" https://1.1.1.1
  check "$1 HTTPS by name" "$3" "$1" "$2" https://example.com
  check "$1 API server service" "$4" "$1" "$2" "https://$api_ip"
  check "$1 kubelet on node" "$4" "$1" "$2" "https://$node_ip:10250"
  check "$1 API server on node" "$4" "$1" "$2" "https://$node_ip:6443"
  check "$1 gateway" "${5:-any}" "$1" "$2" "http://$gateway:53"
  check "$1 metadata address" "${5:-any}" "$1" "$2" http://169.254.169.254
}

kubectl delete -f "$here/policy.yaml" --ignore-not-found >/dev/null
kubectl delete ns spike-old spike-new spike-peer --ignore-not-found >/dev/null
kubectl create ns spike-old >/dev/null
pod spike-old probe

echo "== policy off: the probes must be able to reach things, or a block proves nothing"
targets spike-old probe 0 0

echo "== policy on: namespace that existed before the policy"
kubectl apply -f "$here/policy.yaml" >/dev/null
sleep 5
targets spike-old probe 28 28 28
# 28 here means the name resolved and the connection then timed out, so DNS itself still works
check "spike-old cluster DNS allowed" 28 spike-old probe http://kubernetes.default.svc:6
kubectl -n spike-old exec probe -- nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1 \
  && echo "ok   spike-old nslookup through CoreDNS" \
  || { echo "FAIL spike-old nslookup through CoreDNS"; failures=$((failures + 1)); }

echo "== policy on: namespace created after the policy, with no labels added"
kubectl create ns spike-new >/dev/null
pod spike-new probe
targets spike-new probe 28 28 28

echo "== policy on: pod to pod across namespaces"
kubectl create ns spike-peer >/dev/null
pod spike-peer peer
peer_ip="$(kubectl -n spike-peer get pod peer -o jsonpath='{.status.podIP}')"
check "spike-new to peer pod" 28 spike-new probe "http://$peer_ip:8080"

echo "== policy on: exempt system namespace keeps working"
kubectl -n kube-system get deploy -l k8s-app=kube-dns --no-headers | sed 's/^/info /'
kubectl -n kube-system run spike-probe --image="$image" --restart=Never --command -- sleep 3600 >/dev/null
kubectl -n kube-system wait --for=condition=Ready pod/spike-probe --timeout=180s >/dev/null
check "kube-system public IP (not covered by this policy)" 0 kube-system spike-probe https://1.1.1.1

echo "== policy removed: the same probes recover, so the policy was what blocked them"
kubectl delete -f "$here/policy.yaml" >/dev/null
sleep 5
check "spike-old public IP" 0 spike-old probe https://1.1.1.1
check "spike-new public IP" 0 spike-new probe https://1.1.1.1
# Nothing listens on the peer, so refused (7) shows the packet arrived once the policy is gone
check "spike-new to peer pod" 7 spike-new probe "http://$peer_ip:8080"

kubectl -n kube-system delete pod spike-probe --now >/dev/null
kubectl delete ns spike-old spike-new spike-peer >/dev/null

echo
if [[ $failures != 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all spike checks passed"
