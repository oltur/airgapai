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

# Git, then CI, then Argo CD. Nothing here is exempt: every image below must pass the policy above.
# The repository travels as a ConfigMap: copying it through kubectl exec -i truncated it one time in three.
"${kubectl[@]}" create namespace forgejo --dry-run=client -o yaml | "${kubectl[@]}" apply -f -
"${kubectl[@]}" -n forgejo create configmap lab-bundle --from-file=lab.bundle="$bootstrap/lab.bundle" >/dev/null
"${kubectl[@]}" apply -f "$bootstrap/forgejo.yaml"
"${kubectl[@]}" -n forgejo rollout status deployment/forgejo --timeout=300s
# No -i, as vm-up feeds its remaining commands on stdin.
forgejo=("${kubectl[@]}" -n forgejo exec deployment/forgejo --)

password="$(openssl rand -hex 24)"
"${forgejo[@]}" forgejo admin user create --admin --username airgapai --password "$password" \
  --email admin@airgapai.local --must-change-password=false >/dev/null
token="$("${forgejo[@]}" forgejo admin user generate-access-token --username airgapai --token-name bootstrap \
  --scopes all --raw)"
"${kubectl[@]}" -n forgejo create secret generic forgejo-admin \
  --from-literal=password="$password" --from-literal=token="$token" >/dev/null
"${forgejo[@]}" curl -fsS -o /dev/null -X POST -H "Authorization: token $token" -H 'Content-Type: application/json' \
  -d '{"name":"lab","default_branch":"main"}' http://localhost:3000/api/v1/user/repos
"${forgejo[@]}" sh -c "cd /tmp && git init -q --bare lab.git && cd lab.git && git fetch -q /bundle/lab.bundle lab-bundle:main \
  && git push -q http://airgapai:$token@localhost:3000/airgapai/lab.git main; rc=\$?; rm -rf /tmp/lab.git; exit \$rc"

secret="$(openssl rand -hex 20)"
"${forgejo[@]}" forgejo forgejo-cli actions register --name lab-runner --secret "$secret" >/dev/null
"${kubectl[@]}" -n forgejo create secret generic runner-registration --from-literal=secret="$secret" >/dev/null
"${kubectl[@]}" apply -f "$bootstrap/runner.yaml"
"${kubectl[@]}" -n forgejo rollout status deployment/runner --timeout=180s

"${kubectl[@]}" create namespace argocd --dry-run=client -o yaml | "${kubectl[@]}" apply -f -
"${kubectl[@]}" -n argocd apply --server-side --force-conflicts -f "$bootstrap/argocd.yaml" >/dev/null
for workload in deployment/argocd-repo-server deployment/argocd-redis statefulset/argocd-application-controller; do
  "${kubectl[@]}" -n argocd rollout status "$workload" --timeout=300s
done
# PostgreSQL's passwords exist only in the cluster. Argo CD deploys the database, which reads them.
# An existing Secret is kept: the database learns its passwords once, when its volume is empty.
"${kubectl[@]}" create namespace retrieval --dry-run=client -o yaml | "${kubectl[@]}" apply -f -
"${kubectl[@]}" -n retrieval get secret postgres >/dev/null 2>&1 ||
  "${kubectl[@]}" -n retrieval create secret generic postgres --from-literal=superuser="$(openssl rand -hex 24)" \
    --from-literal=ingest="$(openssl rand -hex 24)" --from-literal=retrieval="$(openssl rand -hex 24)" \
    --from-literal=gateway="$(openssl rand -hex 24)" >/dev/null

# Tickets live in a private Forgejo organisation. Its teams are the reader roles, and the executor is
# an owner of this organisation only, with a token scoped to issues and to reading the organisation:
# enough to check team membership and file issues, and not to change teams or repositories. See ADR 0016.
api() { # method path [json]: as the bootstrap admin, failing on any error status
  "${forgejo[@]}" curl -fsS -o /dev/null -X "$1" -H "Authorization: token $token" -H 'Content-Type: application/json' \
    ${3:+-d "$3"} "http://localhost:3000/api/v1$2"
}
team_id() { # name
  "${forgejo[@]}" curl -fsS -H "Authorization: token $token" http://localhost:3000/api/v1/orgs/maintenance/teams \
    | python3 -c "import json,sys; print(next(t['id'] for t in json.load(sys.stdin) if t['name'] == sys.argv[1]))" "$1"
}
api POST /orgs '{"username":"maintenance","visibility":"private"}'
api POST /orgs/maintenance/repos '{"name":"tickets","private":true,"default_branch":"main"}'
for team in technicians engineers; do
  api POST /orgs/maintenance/teams "{\"name\":\"$team\",\"permission\":\"read\",\"units\":[\"repo.issues\"],\"includes_all_repositories\":true}"
done
"${forgejo[@]}" forgejo admin user create --username executor --password "$(openssl rand -hex 24)" \
  --email executor@airgapai.local --must-change-password=false >/dev/null
api PUT "/teams/$(team_id Owners)/members/executor"
executor_token="$("${forgejo[@]}" forgejo admin user generate-access-token --username executor --token-name executor \
  --scopes write:issue,read:organization --raw)"
"${kubectl[@]}" create namespace gateway --dry-run=client -o yaml | "${kubectl[@]}" apply -f -
"${kubectl[@]}" -n gateway create secret generic executor --from-literal=forgejo-token="$executor_token" \
  --from-literal=database="$("${kubectl[@]}" -n retrieval get secret postgres -o jsonpath='{.data.gateway}' | base64 -d)" >/dev/null
"${kubectl[@]}" apply -f "$bootstrap/root-app.yaml"
