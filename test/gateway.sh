#!/usr/bin/env bash
# Runs inside the VM as root. Tests the gateway as a browser uses it: users sign in through Forgejo,
# see only their own proposals, rendered from the stored arguments with model-written text marked and
# escaped, and a confirmation files exactly one ticket through the executor. Forms without the
# session's CSRF token or from another origin are refused, a second confirmation is refused, a team
# change after sign-in is caught at execution, and a ticket Forgejo refuses is shown as not filed, with
# nothing filed. Every decision is in an unbroken audit chain, and the
# gateway's network paths reach PostgreSQL, Forgejo and the executor only, with controls.
# test/lib/probe.sh, forgejo.sh, browser.sh, gateway-copy.sh and stub.sh are sent ahead of it.
#
# The browser reaches the gateway and Forgejo through kubectl port-forward, on the addresses the
# gateway's OAuth client is registered with. Proposals for the page's checks are made through the chat
# loop of a copy of the gateway whose model is the compromised-model stub, which proposes exactly the
# arguments it is given. The real model's own turns are checked at the end.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
failed=0
result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
kubectl -n gateway rollout status deployment/gateway --timeout=300s >/dev/null || { echo "FAIL the gateway is not running"; exit 1; }

test_user alice technicians
test_user bob engineers
test_user carol
cli grant technician create_ticket >/dev/null
rm -f /tmp/gateway-test-*

kubectl -n gateway port-forward svc/gateway 18092:8080 >/dev/null 2>&1 &
forwards=$!
kubectl -n forgejo port-forward svc/forgejo 13000:3000 >/dev/null 2>&1 &
forwards="$forwards $!"
cleanup() {
  # shellcheck disable=SC2086 # two PIDs
  kill $forwards 2>/dev/null
  stop_stub
  [[ -n ${archived:-} ]] && status PATCH /repos/maintenance/tickets '{"archived": false}' >/dev/null
  rm -f /tmp/gateway-test-* /tmp/gateway-browser.py
  stop_probes gateway probe-gateway probe-gateway-other
}
trap cleanup EXIT
for _ in $(seq 30); do
  curl -fs http://127.0.0.1:18092/ready >/dev/null && curl -fs http://127.0.0.1:13000/api/healthz >/dev/null && break
  sleep 1
done
start_stub || exit 1

# The model's text, with markup and a right-to-left override, as a compromised model might write it.
body=$'Seen at the seal. <script>alert(1)</script> Invoice evil\u202etxt.exe, tagged\U000E0041'
args() { python3 -c 'import json,sys; print(json.dumps({"title": sys.argv[1], "body": sys.argv[2], "priority": "high"}))' "$1" "$2"; }
# shellcheck disable=SC2154 # passwords is set by test/lib/forgejo.sh, sent ahead
browse alice@stub signin /chat "${passwords[alice]}" >/dev/null
propose() { # title: proposes as alice through the stub, and prints the proposal's ID
  ask alice@stub "scenario propose $(args "$1" "$body")" >/dev/null
  proposal_link alice@stub | sed 's|/proposals/||'
}
proposal="$(propose "Pump 4 leaks")"
path="/proposals/$proposal"
before="$(issues)" confirms_before="$(decisions confirm allowed)" refused_before="$(decisions confirm denied)"
executed_before="$(decisions execute allowed)" exec_refused_before="$(decisions execute denied)"
not_filed_before="$(decisions result failed)"

echo "== Sign-in"
result "a visitor is sent to sign in" "303 /login?next=%2Fproposals%2F$proposal" "$(browse visitor get "$path")"
result "a wrong Forgejo password does not sign in (control)" "forgejo-login 200" "$(browse alice signin / wrong-password)"
result "alice signs in through Forgejo and returns to her proposal" "303 $path" "$(browse alice signin "$path" "${passwords[alice]}")"
browse alice get / >/dev/null
result "the gateway reads her role from her teams" 1 "$(shown alice 'alice (technician)')"
result "carol, with no team, signs in with no role" "303 /" "$(browse carol signin / "${passwords[carol]}")"
browse carol get / >/dev/null
result "carol's page says so" 1 "$(shown carol 'carol (no role)')"

echo "== Proposing needs a role that may use the tool, read from Forgejo's teams"
result "alice's proposal through the chat loop (control)" true "$([[ $proposal =~ ^[0-9a-f-]{36}$ ]] && echo true)"
browse carol@stub signin /chat "${passwords[carol]}" >/dev/null
ask carol@stub "scenario propose $(args "Carol's ticket" b)" >/dev/null
result "carol, with no team, cannot propose" 1 "$(shown carol@stub 'A tool call was refused: denied: the user has no role')"
browse bob@stub signin /chat "${passwords[bob]}" >/dev/null
ask bob@stub "scenario propose $(args "Bob's ticket" b)" >/dev/null
result "bob, an engineer with no grant, cannot propose" 1 "$(shown bob@stub 'A tool call was refused: denied: the user&#39;s role may not use this tool')"
result "neither got a proposal" "" "$(proposal_link carol@stub)$(proposal_link bob@stub)"

echo "== The page shows the stored proposal"
result "alice's proposal" 200 "$(browse alice get "$path")"
result "each argument marked as the model's" 3 "$(shown alice 'written by the model')"
result "the model's markup escaped" 1 "$(shown alice '&lt;script&gt;alert(1)&lt;/script&gt;')"
result "no markup from the model" 0 "$(shown alice '<script>')"
result "the right-to-left override shown as a code point" 1 "$(shown alice 'evil⟨U&#43;202E⟩txt.exe')"
result "and an invisible tag character" 1 "$(shown alice 'tagged⟨U&#43;E0041⟩')"
result "bob signs in" "303 /" "$(browse bob signin / "${passwords[bob]}")"
result "bob cannot see alice's proposal" 404 "$(browse bob get "$path")"
result "nor any of its text" 0 "$(shown bob 'Pump 4 leaks')"

echo "== Confirming"
result "a form without the session's CSRF token" 403 "$(browse alice post "$path/confirm" no-csrf)"
result "a form from another origin" 403 "$(browse alice post "$path/confirm" other-origin)"
result "bob confirming alice's proposal" 404 "$(browse bob post "$path/confirm" genuine)"
result "no confirmation recorded for those" "$confirms_before" "$(decisions confirm allowed)"
result "no issue filed for those" "$before" "$(issues)"
browse alice get "$path" >/dev/null
result "alice confirms" 200 "$(browse alice post "$path/confirm" genuine)"
number="$(grep -o 'ticket #[0-9]*' /tmp/gateway-test-alice.html | tr -dc 0-9)"
issue="$(fj GET "/repos/maintenance/tickets/issues/${number:-0}" | sed '$d')"
result "the ticket has the stored title" "Pump 4 leaks" "$(json "d.get('title')" <<<"$issue")"
result "and names alice and the proposal, then the confirmed body" true \
  "$(json "str(d['body'].startswith('Filed for alice after their confirmation of proposal $proposal') and d['body'].endswith('evil\u202etxt.exe, tagged\U000E0041')).lower()" <<<"$issue")"
result "exactly one new issue" "$((before + 1))" "$(issues)"
result "a second confirmation" 403 "$(browse alice post "$path/confirm" genuine)"
result "is refused as already confirmed" 1 "$(shown alice 'already confirmed')"
result "still one new issue" "$((before + 1))" "$(issues)"

echo "== A team change after sign-in is caught at execution"
second="$(propose "Pump 5 leaks")"
technicians="$(team technicians)"
status DELETE "/teams/$technicians/members/alice" >/dev/null
browse alice get "/proposals/$second" >/dev/null
result "alice, removed from technicians since signing in, confirms" 403 "$(browse alice post "/proposals/$second/confirm" genuine)"
result "the executor refused it" 1 "$(shown alice '<h1>Refused at execution</h1>')"
status PUT "/teams/$technicians/members/alice" >/dev/null
result "no issue filed" "$((before + 1))" "$(issues)"

echo "== A ticket Forgejo refuses is shown as not filed (ADR 0029)"
# Forgejo refuses an issue on an archived repository with a 4xx, which the executor reports as certain.
third="$(propose "Pump 6 leaks")"
archived=1
result "maintenance/tickets archived" 200 "$(status PATCH /repos/maintenance/tickets '{"archived": true}')"
browse alice get "/proposals/$third" >/dev/null
result "alice confirms while Forgejo refuses tickets" 502 "$(browse alice post "/proposals/$third/confirm" genuine)"
result "the page says not filed" 1 "$(shown alice '<h1>Not filed</h1>')"
result "with Forgejo's refusal as the reason" 1 "$(shown alice 'Forgejo refused the ticket (423)')"
result "and not that it may have been filed" 0 "$(shown alice 'Result unknown')"
result "maintenance/tickets unarchived" 200 "$(status PATCH /repos/maintenance/tickets '{"archived": false}')"
archived=""
result "nothing was filed" "$((before + 1))" "$(issues)"
result "alice reopens the proposal" 200 "$(browse alice get "/proposals/$third")"
result "its page says nothing was filed" 1 "$(shown alice 'The executor reports that nothing was filed.')"
result "and offers no second confirmation" 0 "$(shown alice 'Confirm and file')"

# The stub was only for making proposals. Remove it now, so the rest runs against the deployed setup.
stop_stub
result "the stub's gateway is gone" NotFound "$(kubectl -n gateway get deployment gateway-stub 2>&1 | grep -o NotFound)"
result "and both test-only network rules" 0 "$(kubectl get globalnetworkpolicy.crd.projectcalico.org -o name | grep -c '/test-')"

echo "== Audit"
chain="$(cli verify)"
result "audit chain unbroken" 0 "$(json "d['first_bad_row']" <<<"$chain")"
echo "info audit rows: $(json "d['rows']" <<<"$chain")"
result "the three confirmations logged" "$((confirms_before + 3))" "$(decisions confirm allowed)"
result "the second confirmation's refusal logged" "$((refused_before + 1))" "$(decisions confirm denied)"
result "two executions allowed" "$((executed_before + 2))" "$(decisions execute allowed)"
result "one of them logged as not filed" "$((not_filed_before + 1))" "$(decisions result failed)"
result "the execution after the team change refused and logged" "$((exec_refused_before + 1))" "$(decisions execute denied)"

echo "== Chat with the model"
# The model's choices decide some of this: the checks are on what the gateway does with them.
browse alice get /chat >/dev/null
started=$SECONDS
result "a question about the manuals" 200 "$(browse alice chat "How do Kubernetes Secrets and RBAC control who can read a Secret?")"
echo "info the turn took $((SECONDS - started)) s"
cp /tmp/gateway-test-alice.html /tmp/gateway-test-question.html
result "the model searched the manuals" 1 "$(shown alice 'Searched the manuals for')"
# Either the answer cites the passages found, or the gateway withholds it: never an uncited answer.
result "and answered with citations, or had its answer withheld" 1 \
  "$(( $(shown alice 'answer · written by the model') + $(shown alice 'No answer is shown,') ))"
echo "info the answer was $( (( $(shown alice 'It cites passages') )) && echo "shown, citing: $(grep -o 'It cites passages [^,]*\(, \[[0-9]*\]\)*' /tmp/gateway-test-alice.html | head -1)" || echo withheld)"
result "a technician was shown no restricted passage" 0 "$(shown alice ', restricted</li>')"
echo "info passages shown: $(shown alice ', public</li>') public"
before_chat="$(issues)" proposals_before="$(decisions propose allowed)"
started=$SECONDS
result "a request to file a ticket" 200 "$(browse alice chat "The web pods in hall 3 keep restarting after the last deployment. Search the manuals for why a container restarts, then file a high priority ticket about the restarts.")"
echo "info the turn took $((SECONDS - started)) s"
result "the model proposed a ticket, not filed" 1 "$(shown alice 'A ticket was proposed, not filed')"
result "and nothing was filed by the turn" "$before_chat" "$(issues)"
result "the proposal is logged as alice's" "$((proposals_before + 1))" "$(decisions propose allowed)"
chat_proposal="$(proposal_link alice)"
result "alice opens it" 200 "$(browse alice get "${chat_proposal:-/proposals/none}")"
echo "info sources shown with text: $(shown alice 'class="source"')"
result "its sources are those the turn found, readable by her" 0 "$(shown alice 'Not readable with your role')"
result "alice confirms it" 200 "$(browse alice post "$chat_proposal/confirm" genuine)"
result "one ticket filed" "$((before_chat + 1))" "$(issues)"
chain="$(cli verify)"
result "audit chain still unbroken" 0 "$(json "d['first_bad_row']" <<<"$chain")"

echo "== Network paths"
start_probes gateway probe-gateway=gateway probe-gateway-other=other || exit 1
result "gateway label reaches PostgreSQL" reached "$(probe gateway probe-gateway postgres.retrieval.svc:5432)"
result "gateway label reaches Forgejo" reached "$(probe gateway probe-gateway forgejo.forgejo.svc:3000)"
result "gateway label reaches the executor" reached "$(probe gateway probe-gateway executor.gateway.svc:8080)"
result "gateway label reaches the chat model" reached "$(probe gateway probe-gateway chat.inference.svc:8080)"
result "gateway label reaches the retrieval service" reached "$(probe gateway probe-gateway retrieval.retrieval.svc:8080)"
result "gateway label cannot reach the embedding server" timeout "$(probe gateway probe-gateway embed.inference.svc:8080)"
result "another label cannot reach the chat model" timeout "$(probe gateway probe-gateway-other chat.inference.svc:8080)"
result "another label cannot reach the retrieval service" timeout "$(probe gateway probe-gateway-other retrieval.retrieval.svc:8080)"
result "another label cannot call the gateway" timeout "$(probe gateway probe-gateway-other gateway.gateway.svc:8080)"
result "another label cannot reach the executor" timeout "$(probe gateway probe-gateway-other executor.gateway.svc:8080)"

if (( failed )); then echo "gateway checks failed"; exit 1; fi
echo "all gateway checks passed"
