#!/usr/bin/env bash
# Runs inside the VM as root. Tests that the executor files a Forgejo issue only for a call confirmed
# by the same user with the same arguments, once, with the role read from Forgejo's teams at the time
# of execution, that every decision is in an unbroken audit chain, and that its network paths reach
# PostgreSQL and Forgejo only, with controls. test/lib/probe.sh is sent ahead of it.
#
# There is no confirmation page yet, so proposals and confirmations are made with the executor's
# administrator commands through kubectl exec. Requests go through kubectl port-forward, which does
# not pass through the network policy.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
port=18091
failed=0

result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
kubectl -n gateway rollout status deployment/executor --timeout=300s >/dev/null || { echo "FAIL the executor is not running"; exit 1; }
cli() { kubectl -n gateway exec deployment/executor -- /executor "$@" 2>&1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

# Forgejo, as its bootstrap administrator: test users and their teams.
admin_token="$(kubectl -n forgejo get secret forgejo-admin -o jsonpath='{.data.token}' | base64 -d)"
fj() { # method path [json]: prints the status, then the body
  kubectl -n forgejo exec deployment/forgejo -- curl -s -w '\n%{http_code}' -X "$1" -H "Authorization: token $admin_token" \
    -H 'Content-Type: application/json' ${3:+-d "$3"} "http://localhost:3000/api/v1$2" 2>/dev/null
}
status() { fj "$@" | tail -1; }
team() { fj GET /orgs/maintenance/teams | sed '$d' | json "next(t['id'] for t in d if t['name'] == '$1')"; }
issues() { fj GET "/repos/maintenance/tickets/issues?state=all&type=issues&limit=50" | sed '$d' | json "len(d)"; }
technicians="$(team technicians)" engineers="$(team engineers)"
# Audit counts by action and decision, as the superuser, so the checks below compare before and after.
decisions() { # action decision: fixed words from this script, so they can go into the SQL as they are
  kubectl -n retrieval exec deployment/postgres -- sh -c "PGPASSWORD=\"\$POSTGRES_PASSWORD\" psql -h 127.0.0.1 -U postgres \
    -d gateway -Atc \"SELECT count(*) FROM gateway.audit WHERE action = '$1' AND decision = '$2'\"" 2>/dev/null
}
denied_before="$(decisions execute denied)" succeeded_before="$(decisions result succeeded)"
for user in alice bob carol; do
  status POST /admin/users "{\"username\":\"$user\",\"email\":\"$user@airgapai.local\",\"password\":\"$(openssl rand -hex 16)\",\"must_change_password\":false}" >/dev/null
done
status PUT "/teams/$technicians/members/alice" >/dev/null
status PUT "/teams/$engineers/members/bob" >/dev/null

echo "== Setup"
before="$(issues)"
result "tickets repository reachable as admin (control)" true "$([[ $before =~ ^[0-9]+$ ]] && echo true)"
result "grant technician create_ticket" "" "$(cli grant technician create_ticket)"
args='{"title":"Pump 3 leaks","body":"Seen at the seal during the morning round.","priority":"high"}'
propose() { cli propose "$1" create_ticket "$2"; }
confirm() { cli confirm "$1" "$2" | json "d['token']"; }
proposal="$(propose alice "$args" | json "d['proposal']")"
token="$(confirm alice "$proposal")"
result "alice's proposal confirmed" true "$([[ ${#token} -ge 40 ]] && echo true)"

kubectl -n gateway port-forward svc/executor "$port:8080" >/dev/null 2>&1 &
forward=$!
cleanup() { kill "$forward" 2>/dev/null; stop_probes gateway probe-executor probe-executor-other; }
trap cleanup EXIT
for _ in $(seq 30); do curl -fs "http://127.0.0.1:$port/ready" >/dev/null && break; sleep 1; done
execute() { # token user tool args: prints the HTTP status
  curl -s -o /tmp/execute.out -w '%{http_code}' "http://127.0.0.1:$port/execute" \
    -d "{\"token\":\"$1\",\"user\":\"$2\",\"tool\":\"$3\",\"args\":$4}"
}

echo "== Calls without a valid confirmation file nothing"
result "no token" 403 "$(execute "" alice create_ticket "$args")"
result "forged token" 403 "$(execute "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" alice create_ticket "$args")"
result "another user, bob, with alice's token" 403 "$(execute "$token" bob create_ticket "$args")"
result "another tool" 403 "$(execute "$token" alice delete_repository "$args")"
result "altered arguments" 403 "$(execute "$token" alice create_ticket "${args/high/low}")"
result "no issue filed by the refused calls" "$before" "$(issues)"
# Captured first: a refused command exits non-zero, which under pipefail would fail a pipe into grep.
carol="$(propose carol "$args")" bob="$(propose bob "$args")"
result "carol, with no team, cannot propose" "denied: the user has no role" "$(head -1 <<<"$carol")"
result "bob, an engineer with no grant, cannot propose" "denied: the user's role may not use this tool" "$(head -1 <<<"$bob")"

echo "== The confirmed call files one issue"
result "alice's confirmed call" 200 "$(execute "$token" alice create_ticket "$args")"
number="$(json "d['issue']" </tmp/execute.out)"
issue="$(fj GET "/repos/maintenance/tickets/issues/$number" | sed '$d')"
result "the issue's title" "Pump 3 leaks" "$(json "d['title']" <<<"$issue")"
result "the issue names the proposal" true "$(json "str('$proposal' in d['body'] and d['body'].startswith('Filed for alice') and d['body'].endswith('Seen at the seal during the morning round.')).lower()" <<<"$issue")"
result "replay of the same token" 403 "$(execute "$token" alice create_ticket "$args")"
result "exactly one new issue" "$((before + 1))" "$(issues)"

echo "== The role is read from Forgejo at execution"
proposal="$(propose alice "$args" | json "d['proposal']")"
token="$(confirm alice "$proposal")"
status DELETE "/teams/$technicians/members/alice" >/dev/null
result "alice removed from technicians after confirming" 403 "$(execute "$token" alice create_ticket "$args")"
status PUT "/teams/$technicians/members/alice" >/dev/null
result "the refusal did not use the token: back in the team, it runs" 200 "$(execute "$token" alice create_ticket "$args")"
result "two issues in all" "$((before + 2))" "$(issues)"

echo "== Audit"
chain="$(cli verify)"
result "audit chain unbroken" 0 "$(json "d['first_bad_row']" <<<"$chain")"
echo "info audit rows: $(json "d['rows']" <<<"$chain")"
result "all 7 refused executions logged" "$((denied_before + 7))" "$(decisions execute denied)"
result "both results logged" "$((succeeded_before + 2))" "$(decisions result succeeded)"

echo "== Network paths"
start_probes gateway probe-executor=executor probe-executor-other=other || exit 1
result "executor label reaches PostgreSQL" reached "$(probe gateway probe-executor postgres.retrieval.svc:5432)"
result "executor label reaches Forgejo" reached "$(probe gateway probe-executor forgejo.forgejo.svc:3000)"
result "executor label cannot reach the embedding server" timeout "$(probe gateway probe-executor embed.inference.svc:8080)"
result "executor label cannot reach the retrieval service" timeout "$(probe gateway probe-executor retrieval.retrieval.svc:8080)"
result "another label cannot reach PostgreSQL" timeout "$(probe gateway probe-executor-other postgres.retrieval.svc:5432)"
result "another label cannot reach Forgejo" timeout "$(probe gateway probe-executor-other forgejo.forgejo.svc:3000)"
result "nothing can call the executor" timeout "$(probe gateway probe-executor-other executor.gateway.svc:8080)"

if (( failed )); then echo "executor checks failed"; exit 1; fi
echo "all executor checks passed"
