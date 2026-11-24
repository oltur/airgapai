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
# Reads a user's last page. answer: the answer as a browser shows it. fields: a proposal's fields and
# repository. outcome: cited if the answer is shown and every passage it cites is one the page lists,
# withheld or stopped if the gateway showed none, and otherwise what is wrong.
cat >/tmp/demo-page.py <<'PY'
import html, re, sys
mode, page = sys.argv[1], open(sys.argv[2]).read()
def text(value, cut):
    value = " ".join(html.unescape(value).split())
    return value[:cut] + " …" if len(value) > cut else value
answer = re.search(r"answer · written by the model</div><pre>(.*?)</pre>", page, re.S)
if mode == "answer":
    print("   " + (text(answer.group(1), 400) if answer else "(no answer is shown)"))
elif mode == "fields":
    for name, value in re.findall(r'<div class="tag">(\w+) · written by the model</div><pre>(.*?)</pre>', page, re.S):
        print(f"   {name}: {text(value, 200)}")
    m = re.search(r"the ticket goes to ([\w/-]+)", page)
    print("   repository: " + (m.group(1) if m else "?"))
elif mode == "outcome":
    listed = len(re.findall(r"<li>[^<]*, chunk \d+, \w+</li>", page))
    cites = re.search(r"It cites passages ((?:\[\d+\](?:, )?)+), listed below", page)
    if answer:
        numbers = [int(n) for n in re.findall(r"\d+", cites.group(1))] if cites else []
        ok = numbers and all(1 <= n <= listed for n in numbers)
        print("cited" if ok else f"shown citing {numbers or 'nothing'} of {listed} passages")
    elif "No answer is shown," in page:
        print("withheld")
    elif "was stopped." in page:
        print("stopped")
    else:
        print("no outcome on the page")
PY
page() { python3 -I /tmp/demo-page.py "$1" "/tmp/gateway-test-$2.html"; }
answer() { page answer "$1"; }
passages() { grep -o '<li>[^<]*, chunk [0-9]*, [a-z]*</li>' "/tmp/gateway-test-$1.html" | sed 's/<[^>]*>//g; s/^/   - /'; }
turn() { # user question
  local started=$SECONDS status
  say "$1 asks: $2"
  status="$(ask "$1" "$2")"
  say "(the turn took $((SECONDS - started)) s, on CPU)"
  held "the turn completed" 200 "$status"
}
# The gateway's own rule: an answer is shown only citing passages its turn found. Withholding it, or
# stopping a model that keeps calling tools, keeps the rule; which one happens is the model's doing.
cited_or_withheld() {
  local outcome
  outcome="$(page outcome "$1")"
  case $outcome in
    cited | withheld | stopped) held "the answer cites passages the turn found, or none is shown ($outcome)" x x ;;
    *) held "the answer cites passages the turn found, or none is shown" "cited, withheld or stopped" "$outcome" ;;
  esac
}
# The tickets in maintenance/tickets, or a failure: a count Forgejo did not give must not pass a check.
count_issues() {
  local n
  n="$(issues)"
  [[ $n =~ ^[0-9]+$ ]] || { echo "FAIL Forgejo gave no ticket count: $n"; exit 1; }
  echo "$n"
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
  rm -f /tmp/gateway-test-* /tmp/gateway-browser.py /tmp/demo-page.py
  stop_probes gateway demo-gateway
}
trap cleanup EXIT
ready=""
for _ in $(seq 30); do
  curl -fs http://127.0.0.1:18092/ready >/dev/null && curl -fs http://127.0.0.1:13000/api/healthz >/dev/null && { ready=1; break; }
  sleep 1
done
[[ -n $ready ]] || { echo "FAIL the tunnels to the gateway and Forgejo did not come up in 30 s"; exit 1; }
audit_start="$(json "d['rows']" <<<"$(cli verify)")"
[[ $audit_start =~ ^[0-9]+$ ]] || { echo "FAIL could not read the audit log"; exit 1; }

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
before="$(count_issues)" || exit 1
turn alice "The web pods in hall 3 keep restarting after the last deployment. Search the manuals for why a container restarts, then file a high priority ticket about the restarts."
held "nothing was filed by the turn" "$before" "$(count_issues)"
proposal="$(proposal_link alice)"
if [[ -z $proposal ]]; then
  note "the model proposed no ticket this time, so there is nothing to confirm"
else
  say "The page says: A ticket was proposed, not filed."
  browse alice get "$proposal" >/dev/null
  say "alice opens $proposal. It shows what will be filed, marked as the model's, and where:"
  page fields alice
  say "Sources shown with their text, read with her role: $(shown alice 'class="source"')"
  held "a confirmation without the session's CSRF token is refused" 403 "$(browse alice post "$proposal/confirm" no-csrf)"
  held "and files nothing" "$before" "$(count_issues)"
  browse alice get "$proposal" >/dev/null
  held "alice confirms" 200 "$(browse alice post "$proposal/confirm" genuine)"
  filed="$(grep -o 'ticket #[0-9]*' /tmp/gateway-test-alice.html | head -1)"
  say "${filed:-No ticket} was filed by the executor."
  held "exactly one ticket filed" "$((before + 1))" "$(count_issues)"
  # The proposal's own page, not the result page, which also says "filed it as": only the proposal's
  # says "You confirmed this at", and it must name the same ticket.
  held "alice reopens the proposal" 200 "$(browse alice get "$proposal")"
  say "Back on the proposal's page: $(grep -o 'ticket #[0-9]*' /tmp/gateway-test-alice.html | head -1 || true)"
  held "the proposal's page says alice confirmed it, names ${filed:-the ticket}, and offers no second confirmation" "1 1 0" \
    "$(shown alice 'You confirmed this at') $(shown alice ">${filed:-no ticket}</a>") $(shown alice 'Confirm and file')"
fi

step "5. Every decision is in the audit log"
chain="$(cli verify)"
held "the audit chain is unbroken" 0 "$(json "d['first_bad_row']" <<<"$chain")"
rows="$(json "d['rows']" <<<"$chain")"
say "$rows rows, $((rows - audit_start)) of them written during this demo:"
kubectl -n retrieval exec deployment/postgres -- sh -c "PGPASSWORD=\"\$POSTGRES_PASSWORD\" psql -h 127.0.0.1 -U postgres \
  -d gateway -Atc \"SELECT action, decision, actor FROM gateway.audit ORDER BY seq OFFSET $audit_start\"" 2>/dev/null \
  | sed 's/|/  /g; s/^/   /'
say "make demo now copies the log off the VM, where a later rewrite of these rows would show."

if (( failed )); then printf '\ndemo: a rule did not hold\n'; exit 1; fi
printf '\ndemo: every rule held\n'
