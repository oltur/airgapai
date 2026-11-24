#!/usr/bin/env bash
# Runs inside the VM as root, on a VM that `make rebuild` has built and tested: the claim, end to end,
# with the real model through the deployed gateway. test/lib/probe.sh, forgejo.sh and browser.sh are
# sent ahead of it. `make demo` runs it, then copies the audit log off the VM.
#
# The model's words are its own, and are printed as they came. The demo fails only if the stack breaks
# a rule it enforces: a path out, restricted text for a technician, an uncited answer shown, a ticket
# filed without a confirmation, or a broken audit chain. Anything the model chose differently is a note.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
failed=0
step() { printf '\n== %s\n' "$1"; }
say() { printf '   %s\n' "$@"; }
held() { # name expected actual: a rule the stack enforces
  if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
note() { echo "note $*"; }
cli() { kubectl -n gateway exec deployment/executor -- /executor "$@" 2>&1; }
# What a page shows: the answer, as the browser renders it, and the passages it lists.
answer() {
  python3 -I -c '
import html, re, sys
page = open(sys.argv[1]).read()
m = re.search(r"answer · written by the model</div><pre>(.*?)</pre>", page, re.S)
if not m:
    print("   (no answer is shown)"); sys.exit()
text = " ".join(html.unescape(m.group(1)).split())
print("   " + (text[:400] + " …" if len(text) > 400 else text))' "/tmp/gateway-test-$1.html"
}
passages() { grep -o '<li>[^<]*, chunk [0-9]*, [a-z]*</li>' "/tmp/gateway-test-$1.html" | sed 's/<[^>]*>//g; s/^/   - /'; }
turn() { # user question
  local started=$SECONDS status
  say "$1 asks: $2"
  status="$(ask "$1" "$2")"
  say "(the turn took $((SECONDS - started)) s, on CPU)"
  held "the turn completed" 200 "$status"
}
# The gateway's own rule: an answer is shown only with citations, or withheld.
cited_or_withheld() {
  held "the answer cites passages the turn found, or is withheld" 1 \
    "$(( $(shown "$1" 'answer · written by the model') && $(shown "$1" 'It cites passages') || $(shown "$1" 'No answer is shown,') ))"
}

kubectl -n gateway rollout status deployment/gateway --timeout=120s >/dev/null || { echo "FAIL the gateway is not running"; exit 1; }
test_user alice technicians
test_user bob engineers
cli grant technician create_ticket >/dev/null
rm -f /tmp/gateway-test-*
kubectl -n gateway port-forward svc/gateway 18092:8080 >/dev/null 2>&1 &
forwards=$!
kubectl -n forgejo port-forward svc/forgejo 13000:3000 >/dev/null 2>&1 &
forwards="$forwards $!"
cleanup() {
  # shellcheck disable=SC2086 # two PIDs
  kill $forwards 2>/dev/null
  rm -f /tmp/gateway-test-* /tmp/gateway-browser.py
  stop_probes gateway demo-gateway
}
trap cleanup EXIT
for _ in $(seq 30); do
  curl -fs http://127.0.0.1:18092/ready >/dev/null && curl -fs http://127.0.0.1:13000/api/healthz >/dev/null && break
  sleep 1
done

step "1. Nothing leaves"
say "A pod with the gateway's network label tries the chat model, which it may reach, and the internet."
start_probes gateway demo-gateway=gateway || exit 1
held "the gateway's label reaches the chat model (control)" reached "$(probe gateway demo-gateway chat.inference.svc:8080)"
held "it cannot reach 1.1.1.1:443" timeout "$(probe gateway demo-gateway 1.1.1.1:443)"
held "nor resolve a public name" dns-failed "$(probe gateway demo-gateway example.org:443)"
stop_probes gateway demo-gateway
say "make test-egress opens each layer in turn to show which one holds; this is the closed state."

step "2. A technician gets an answer that cites the manuals"
# shellcheck disable=SC2154 # passwords is set by test/lib/forgejo.sh, sent ahead
held "alice, a technician, signs in through Forgejo" "303 /chat" "$(browse alice signin /chat "${passwords[alice]}")"
turn alice "What is the difference between a resource request and a limit for a container?"
answer alice
say "Passages found:"
passages alice
cited_or_withheld alice

step "3. The same question, two roles"
question="What are the differences between the Baseline and Restricted Pod Security Standards?"
say "The answer is in a page labelled restricted, a synthetic stand-in for text above a technician's role."
turn alice "$question"
held "a technician's turn found no restricted passage" 0 "$(shown alice ', restricted</li>')"
say "Passages found:"
passages alice
answer alice
cited_or_withheld alice
held "bob, an engineer, signs in" "303 /chat" "$(browse bob signin /chat "${passwords[bob]}")"
turn bob "$question"
say "Passages found:"
passages bob
(( $(shown bob ', restricted</li>') )) || note "the engineer's searches found no restricted passage this time"
answer bob
cited_or_withheld bob

step "4. The model proposes a ticket; only the user files it"
before="$(issues)"
turn alice "The web pods in hall 3 keep restarting after the last deployment. Search the manuals for why a container restarts, then file a high priority ticket about the restarts."
held "nothing was filed by the turn" "$before" "$(issues)"
proposal="$(proposal_link alice)"
if [[ -z $proposal ]]; then
  note "the model proposed no ticket this time, so there is nothing to confirm"
else
  say "The page says: A ticket was proposed, not filed."
  browse alice get "$proposal" >/dev/null
  say "alice opens $proposal. It shows what will be filed, marked as the model's, and where:"
  python3 -I -c '
import html, re, sys
page = open(sys.argv[1]).read()
for name, value in re.findall(r"<div class=\"tag\">(\w+) · written by the model</div><pre>(.*?)</pre>", page, re.S):
    text = " ".join(html.unescape(value).split())
    print(f"   {name}: " + (text[:200] + " …" if len(text) > 200 else text))
m = re.search(r"the ticket goes to ([\w/-]+)", page)
print("   repository: " + (m.group(1) if m else "?"))' /tmp/gateway-test-alice.html
  say "Sources shown with their text, read with her role: $(shown alice 'class="source"')"
  held "a confirmation without the session's CSRF token is refused" 403 "$(browse alice post "$proposal/confirm" no-csrf)"
  held "and files nothing" "$before" "$(issues)"
  browse alice get "$proposal" >/dev/null
  held "alice confirms" 200 "$(browse alice post "$proposal/confirm" genuine)"
  say "$(grep -o 'ticket #[0-9]*' /tmp/gateway-test-alice.html | head -1) was filed by the executor."
  held "exactly one ticket filed" "$((before + 1))" "$(issues)"
fi

step "5. Every decision is in the audit log"
chain="$(cli verify)"
held "the audit chain is unbroken" 0 "$(json "d['first_bad_row']" <<<"$chain")"
say "$(json "d['rows']" <<<"$chain") rows. The last of this demo's decisions:"
kubectl -n retrieval exec deployment/postgres -- sh -c "PGPASSWORD=\"\$POSTGRES_PASSWORD\" psql -h 127.0.0.1 -U postgres \
  -d gateway -Atc \"SELECT action, decision, actor FROM gateway.audit ORDER BY seq DESC LIMIT 4\"" 2>/dev/null \
  | tac | sed 's/|/  /g; s/^/   /'
say "make demo now copies the log off the VM, where a later rewrite of these rows would show."

if (( failed )); then printf '\ndemo: a rule did not hold\n'; exit 1; fi
printf '\ndemo: every rule held\n'
