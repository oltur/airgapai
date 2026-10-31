#!/usr/bin/env bash
# Runs inside the VM as root. Tests that Git drives the cluster and that CI catches a secret, with controls.
set -uo pipefail

kubectl=(/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml)
# No -i: this script arrives on stdin, and exec would swallow the rest of it.
forgejo=("${kubectl[@]}" -n forgejo exec deployment/forgejo --)
failed=0

result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
"${kubectl[@]}" -n forgejo rollout status deployment/forgejo --timeout=300s >/dev/null || { echo "FAIL Forgejo is not running"; exit 1; }
token="$("${kubectl[@]}" -n forgejo get secret forgejo-admin -o jsonpath='{.data.token}' | base64 -d)"
api() { # method path [body]
  "${forgejo[@]}" curl -sS -X "$1" -H "Authorization: token $token" -H 'Content-Type: application/json' \
    ${3:+-d "$3"} "http://localhost:3000/api/v1/repos/airgapai/lab$2"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
app() { "${kubectl[@]}" -n argocd get application "$1" -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null; }
wait_for() { # seconds expected command...
  local limit=$1 expected=$2 start=$SECONDS got; shift 2
  while (( SECONDS - start < limit )); do
    got="$("$@")"
    [[ $got == "$expected" ]] && { echo "$got"; return; }
    sleep 5
  done
  echo "$got"
}
marker() { "${kubectl[@]}" -n workloads get configmap deployed-from-git -o jsonpath='{.data.revision}' 2>/dev/null; }
refresh() { "${kubectl[@]}" -n argocd annotate application workloads argocd.argoproj.io/refresh=hard --overwrite >/dev/null; }
run_status() { # sha
  api GET "/actions/tasks?limit=50" | python3 -c "
import json,sys
runs=[r for r in json.load(sys.stdin).get('workflow_runs',[]) if r.get('head_sha')=='$1']
print(runs[0]['status'] if runs else 'none')"
}

echo "== Git drives the cluster"
result "root application" Synced/Healthy "$(wait_for 240 Synced/Healthy app root)"
result "child application" Synced/Healthy "$(wait_for 240 Synced/Healthy app workloads)"
path=deploy/workloads/workloads.yaml
file="$(api GET "/contents/$path")"
sha="$(json "d['sha']" <<<"$file")"
new="commit-$RANDOM$RANDOM"
before="$(marker)"
[[ $before != "$new" ]] && echo "ok   value differs before the commit (control): $before"
content="$(json "__import__('base64').b64decode(d['content']).decode()" <<<"$file" | sed "s/^  revision: .*/  revision: $new/" | base64 -w0)"
clean="$(api PUT "/contents/$path" "{\"content\":\"$content\",\"sha\":\"$sha\",\"message\":\"Change the marker\",\"branch\":\"main\"}" | json "d['commit']['sha']")"
refresh
result "commit reaches the cluster" "$new" "$(wait_for 180 "$new" marker)"

"${kubectl[@]}" -n workloads patch configmap deployed-from-git --type=merge -p '{"data":{"revision":"drifted"}}' >/dev/null
result "drift is reverted" "$new" "$(wait_for 180 "$new" marker)"

echo "== CI"
status_of() { run_status "$1"; }
result "clean commit passes make check" success "$(wait_for 300 success status_of "$clean")"
# Generated here, so no secret-shaped value is ever stored in this repository.
fake="AKIA$(tr -dc 'A-Z2-7' </dev/urandom | head -c 16)"
secret_b64="$(printf 'aws_access_key_id = %s\naws_secret_access_key = %s\n' "$fake" "$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 40)" | base64 -w0)"
api POST /branches '{"new_branch_name":"ci-control","old_branch_name":"main"}' >/dev/null
bad="$(api POST /contents/ci-control.txt "{\"content\":\"$secret_b64\",\"message\":\"CI control\",\"branch\":\"ci-control\"}" | json "d['commit']['sha']")"
result "commit with a generated secret fails make check" failure "$(wait_for 300 failure status_of "$bad")"
api DELETE /branches/ci-control >/dev/null
# The status alone does not say why it failed, so the job's own log has to name the leak.
# Read on the node, as the Forgejo image has no zstd. Clean runs say "no leaks found", so that wording does not count.
leak_log="$(find /var/lib/airgapai/forgejo/data/actions_log -name '*.log.zst' -exec zstd -dcq {} + 2>/dev/null | grep -E 'leaks found: [1-9]' | grep -c -v 'no leaks')"
[[ $leak_log -gt 0 ]] && leak_log=reported
result "job log reports the leak" reported "$leak_log"

echo "== Isolation"
denied="$("${kubectl[@]}" get pods -n forgejo -o jsonpath='{.items[*].spec.containers[*].image}')"
[[ $denied == *10.43.0.50:5000/* ]] && echo "ok   images come from the internal registry"
result "git service has no outbound path" failed "$("${forgejo[@]}" sh -c 'curl -s -m 5 -o /dev/null https://1.1.1.1 && echo reached || echo failed')"
# A privileged request is refused by Pod Security here, as in any namespace that is not exempt.
privileged='{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pss-check","namespace":"forgejo"},"spec":{"hostNetwork":true,"containers":[{"name":"c","image":"'"$("${kubectl[@]}" -n forgejo get deployment forgejo -o jsonpath='{.spec.template.spec.containers[0].image}')"'"}]}}'
pss="$(echo "$privileged" | "${kubectl[@]}" apply --dry-run=server -f - 2>&1)"
grep -q 'violates PodSecurity' <<<"$pss" && pss=rejected
result "hostNetwork pod in the forgejo namespace" rejected "$pss"

[[ $failed == 0 ]] && echo PASS || { echo FAILED; exit 1; }
