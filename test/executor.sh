#!/usr/bin/env bash
# Runs inside the VM as root. Tests that the executor refuses a call without a confirmation, that a
# user's Forgejo team decides whether they may propose, that every decision is in an unbroken audit
# chain, and that its network paths reach PostgreSQL and Forgejo only, with only the gateway able to
# call it, with controls. test/lib/probe.sh and test/lib/forgejo.sh are sent ahead of it.
#
# A confirmation is made only on the gateway's page, and its token never leaves the gateway, so the
# confirmed call is tested through that page, in test/gateway.sh. The bindings a stolen token would
# meet, such as another user, altered arguments or a replay, are tested against the same schema in
# make test-db. Proposals are made with the executor's propose command, through kubectl exec, which
# stands in for the model until the chat loop exists. Requests go through kubectl port-forward, which
# does not pass through the network policy.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
port=18091
failed=0

result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
kubectl -n gateway rollout status deployment/executor --timeout=300s >/dev/null || { echo "FAIL the executor is not running"; exit 1; }
cli() { kubectl -n gateway exec deployment/executor -- /executor "$@" 2>&1; }

denied_before="$(decisions execute denied)"
test_user alice technicians
test_user bob engineers
test_user carol

echo "== Setup"
before="$(issues)"
result "tickets repository reachable as admin (control)" true "$([[ $before =~ ^[0-9]+$ ]] && echo true)"
result "grant technician create_ticket" "" "$(cli grant technician create_ticket)"
no_confirm="$(cli confirm alice 00000000-0000-0000-0000-000000000000)"
result "there is no confirm command" "commands: serve, grant, revoke, verify, propose" "$(head -1 <<<"$no_confirm")"
args='{"title":"Pump 3 leaks","body":"Seen at the seal during the morning round.","priority":"high"}'
propose() { cli propose "$1" create_ticket "$2"; }
proposal="$(propose alice "$args" | json "d['proposal']")"
result "alice, a technician, can propose (control)" true "$([[ $proposal =~ ^[0-9a-f-]{36}$ ]] && echo true)"
# Captured first: a refused command exits non-zero, which under pipefail would fail a pipe into grep.
carol="$(propose carol "$args")" bob="$(propose bob "$args")"
result "carol, with no team, cannot propose" "denied: the user has no role" "$(head -1 <<<"$carol")"
result "bob, an engineer with no grant, cannot propose" "denied: the user's role may not use this tool" "$(head -1 <<<"$bob")"

kubectl -n gateway port-forward svc/executor "$port:8080" >/dev/null 2>&1 &
forward=$!
cleanup() { kill "$forward" 2>/dev/null; stop_probes gateway probe-executor probe-executor-other; }
trap cleanup EXIT
for _ in $(seq 30); do curl -fs "http://127.0.0.1:$port/ready" >/dev/null && break; sleep 1; done
execute() { # token user tool args: prints the HTTP status
  curl -s -o /tmp/execute.out -w '%{http_code}' "http://127.0.0.1:$port/execute" \
    -d "{\"token\":\"$1\",\"user\":\"$2\",\"tool\":\"$3\",\"args\":$4}"
}

echo "== Calls without a confirmation file nothing"
result "no token" 403 "$(execute "" alice create_ticket "$args")"
result "forged token" 403 "$(execute "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" alice create_ticket "$args")"
result "no issue filed by the refused calls" "$before" "$(issues)"

echo "== Audit"
chain="$(cli verify)"
result "audit chain unbroken" 0 "$(json "d['first_bad_row']" <<<"$chain")"
echo "info audit rows: $(json "d['rows']" <<<"$chain")"
result "both refused executions logged" "$((denied_before + 2))" "$(decisions execute denied)"

echo "== Network paths"
start_probes gateway probe-executor=executor probe-executor-other=other || exit 1
result "executor label reaches PostgreSQL" reached "$(probe gateway probe-executor postgres.retrieval.svc:5432)"
result "executor label reaches Forgejo" reached "$(probe gateway probe-executor forgejo.forgejo.svc:3000)"
result "executor label cannot reach the embedding server" timeout "$(probe gateway probe-executor embed.inference.svc:8080)"
result "executor label cannot reach the retrieval service" timeout "$(probe gateway probe-executor retrieval.retrieval.svc:8080)"
result "executor label cannot call the gateway" timeout "$(probe gateway probe-executor gateway.gateway.svc:8080)"
result "another label cannot reach PostgreSQL" timeout "$(probe gateway probe-executor-other postgres.retrieval.svc:5432)"
result "another label cannot reach Forgejo" timeout "$(probe gateway probe-executor-other forgejo.forgejo.svc:3000)"
result "another label cannot call the executor" timeout "$(probe gateway probe-executor-other executor.gateway.svc:8080)"

if (( failed )); then echo "executor checks failed"; exit 1; fi
echo "all executor checks passed"
