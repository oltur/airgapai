#!/usr/bin/env bash
# Runs inside the VM as root, once the node and RKE2's install jobs are ready. Loads the verifier
# from Zot, then turns the policy on. Applied server-side because Kyverno's CRDs are too large for
# the annotation a client-side apply writes.
set -euo pipefail

bootstrap="$(cd "$(dirname "$0")" && pwd)"
kubectl=(/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml)

"${kubectl[@]}" -n zot rollout status deployment/zot --timeout=180s
"${kubectl[@]}" create namespace kyverno --dry-run=client -o yaml | "${kubectl[@]}" apply -f -
"${kubectl[@]}" apply --server-side -f "$bootstrap/kyverno.yaml" >/dev/null
"${kubectl[@]}" -n kyverno rollout status deployment/kyverno-admission-controller --timeout=300s
"${kubectl[@]}" wait --for=condition=Established --timeout=60s crd/imagevalidatingpolicies.policies.kyverno.io
"${kubectl[@]}" apply -f "$bootstrap/signed-images.yaml"

# Kyverno deletes its own webhooks when it stops cleanly, so a rollout or drain would admit anything
# despite failurePolicy Fail. This copy is not Kyverno's to delete, and fails closed with no endpoints.
for _ in $(seq 60); do
  "${kubectl[@]}" get validatingwebhookconfiguration kyverno-resource-validating-webhook-cfg -o json >/tmp/kyverno-webhook.json 2>/dev/null \
    && grep -q 'finegrained-require-signed-images' /tmp/kyverno-webhook.json && break
  sleep 2
done
python3 -I - /tmp/kyverno-webhook.json <<'PY' | "${kubectl[@]}" apply -f -
import json, sys
source = json.load(open(sys.argv[1]))
webhooks = [w for w in source["webhooks"] if "finegrained-require-signed-images" in w["name"]]
assert len(webhooks) == 1
webhooks[0]["name"] = "signed-images-guard.airgapai.local"
print(json.dumps({
    "apiVersion": "admissionregistration.k8s.io/v1",
    "kind": "ValidatingWebhookConfiguration",
    "metadata": {"name": "airgapai-signed-images-guard"},
    "webhooks": webhooks,
}))
PY
rm -f /tmp/kyverno-webhook.json
