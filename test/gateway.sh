#!/usr/bin/env bash
# Runs inside the VM as root. Tests the gateway as a browser uses it: users sign in through Forgejo,
# see only their own proposals, rendered from the stored arguments with model-written text marked and
# escaped, and a confirmation files exactly one ticket through the executor. Forms without the
# session's CSRF token or from another origin are refused, a second confirmation is refused, and a team
# change after sign-in is caught at execution. Every decision is in an unbroken audit chain, and the
# gateway's network paths reach PostgreSQL, Forgejo and the executor only, with controls.
# test/lib/probe.sh and test/lib/forgejo.sh are sent ahead of it.
#
# The browser reaches the gateway and Forgejo through kubectl port-forward, on the addresses the
# gateway's OAuth client is registered with. Proposals are made with the executor's propose command,
# which stands in for the model until the chat loop exists.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
failed=0
result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
kubectl -n gateway rollout status deployment/gateway --timeout=300s >/dev/null || { echo "FAIL the gateway is not running"; exit 1; }
cli() { kubectl -n gateway exec deployment/executor -- /executor "$@" 2>&1; }

# A browser per user, with its own cookies, that shows redirects rather than following them. It
# prints the status, then the redirect target if there is one, and keeps the last page in a file.
cat >/tmp/gateway-browser.py <<'PY'
import html, http.cookiejar, re, socket, sys, urllib.error, urllib.parse, urllib.request
GW, FJ = "http://127.0.0.1:18092", "http://forgejo.forgejo.svc:13000"
resolve = socket.getaddrinfo  # the browser's name for Forgejo goes to the local tunnel
socket.getaddrinfo = lambda host, *a, **k: resolve("127.0.0.1" if host == "forgejo.forgejo.svc" else host, *a, **k)
user, cmd, args = sys.argv[1], sys.argv[2], sys.argv[3:]
jar = http.cookiejar.LWPCookieJar(f"/tmp/gateway-test-{user}.jar")
try:
    jar.load(ignore_discard=True, ignore_expires=True)
except FileNotFoundError:
    pass
class Stay(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a):
        return None
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), Stay)
def go(url, form=None, headers=None):
    req = urllib.request.Request(url, data=urllib.parse.urlencode(form).encode() if form is not None else None,
                                 headers=headers or {})
    try:
        r = opener.open(req, timeout=60)
    except urllib.error.HTTPError as e:
        r = e
    return r.getcode(), r.headers.get("Location") or "", r.read().decode()
def done(status, location="", page=""):
    open(f"/tmp/gateway-test-{user}.html", "w").write(page)
    jar.save(ignore_discard=True, ignore_expires=True)
    print(f"{status} {location}" if location else status)
    sys.exit(0)
def page_csrf():
    m = re.search(r'name="csrf" value="([^"]+)"', open(f"/tmp/gateway-test-{user}.html").read())
    return m.group(1) if m else ""
if cmd == "signin":  # NEXT PASSWORD
    _, authorize, _ = go(GW + "/login?" + urllib.parse.urlencode({"next": args[0]}))
    status, _, page = go(FJ + "/user/login", {"user_name": user, "password": args[1]}, {"Origin": FJ})
    if status != 303:
        done(f"forgejo-login {status}")
    status, location, page = go(authorize)
    if status == 200:  # Forgejo asks once whether to authorise the gateway
        fields = {k: html.unescape(v) for k, v in re.findall(r'<input type="hidden" name="([^"]+)" value="([^"]*)"', page)}
        status, location, page = go(FJ + "/login/oauth/grant", {**fields, "granted": "true"}, {"Origin": FJ})
    if not location.startswith(GW + "/callback?"):
        done(f"authorize {status}")
    done(*go(location))
if cmd == "get":  # PATH
    done(*go(GW + args[0]))
if cmd == "post":  # PATH MODE: genuine, no-csrf or other-origin
    form = {} if args[1] == "no-csrf" else {"csrf": page_csrf()}
    origin = "http://evil.example" if args[1] == "other-origin" else GW
    done(*go(GW + args[0], form, {"Origin": origin}))
sys.exit(f"unknown command {cmd}")
PY
browse() { python3 -I /tmp/gateway-browser.py "$@"; }
shown() { grep -o -F -- "$2" "/tmp/gateway-test-$1.html" | wc -l; } # occurrences, not lines

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
  rm -f /tmp/gateway-test-* /tmp/gateway-browser.py
  stop_probes gateway probe-gateway probe-gateway-other
}
trap cleanup EXIT
for _ in $(seq 30); do
  curl -fs http://127.0.0.1:18092/ready >/dev/null && curl -fs http://127.0.0.1:13000/api/healthz >/dev/null && break
  sleep 1
done

# The model's text, with markup and a right-to-left override, as a compromised model might write it.
body=$'Seen at the seal. <script>alert(1)</script> Invoice evil\u202etxt.exe, tagged\U000E0041'
args() { python3 -c 'import json,sys; print(json.dumps({"title": sys.argv[1], "body": sys.argv[2], "priority": "high"}))' "$1" "$2"; }
propose() { cli propose alice create_ticket "$(args "$1" "$body")" | json "d['proposal']"; }
proposal="$(propose "Pump 4 leaks")"
path="/proposals/$proposal"
before="$(issues)" confirms_before="$(decisions confirm allowed)" refused_before="$(decisions confirm denied)"
executed_before="$(decisions execute allowed)" exec_refused_before="$(decisions execute denied)"

echo "== Sign-in"
result "a visitor is sent to sign in" "303 /login?next=%2Fproposals%2F$proposal" "$(browse visitor get "$path")"
result "a wrong Forgejo password does not sign in (control)" "forgejo-login 200" "$(browse alice signin / wrong-password)"
# shellcheck disable=SC2154 # passwords is set by test/lib/forgejo.sh, sent ahead
result "alice signs in through Forgejo and returns to her proposal" "303 $path" "$(browse alice signin "$path" "${passwords[alice]}")"
browse alice get / >/dev/null
result "the gateway reads her role from her teams" 1 "$(shown alice 'alice (technician)')"
result "carol, with no team, signs in with no role" "303 /" "$(browse carol signin / "${passwords[carol]}")"
browse carol get / >/dev/null
result "carol's page says so" 1 "$(shown carol 'carol (no role)')"

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

echo "== Audit"
chain="$(cli verify)"
result "audit chain unbroken" 0 "$(json "d['first_bad_row']" <<<"$chain")"
echo "info audit rows: $(json "d['rows']" <<<"$chain")"
result "both confirmations logged" "$((confirms_before + 2))" "$(decisions confirm allowed)"
result "the second confirmation's refusal logged" "$((refused_before + 1))" "$(decisions confirm denied)"
result "one execution allowed" "$((executed_before + 1))" "$(decisions execute allowed)"
result "the execution after the team change refused and logged" "$((exec_refused_before + 1))" "$(decisions execute denied)"

echo "== Network paths"
start_probes gateway probe-gateway=gateway probe-gateway-other=other || exit 1
result "gateway label reaches PostgreSQL" reached "$(probe gateway probe-gateway postgres.retrieval.svc:5432)"
result "gateway label reaches Forgejo" reached "$(probe gateway probe-gateway forgejo.forgejo.svc:3000)"
result "gateway label reaches the executor" reached "$(probe gateway probe-gateway executor.gateway.svc:8080)"
result "gateway label cannot reach the chat model" timeout "$(probe gateway probe-gateway chat.inference.svc:8080)"
result "gateway label cannot reach the retrieval service" timeout "$(probe gateway probe-gateway retrieval.retrieval.svc:8080)"
result "another label cannot call the gateway" timeout "$(probe gateway probe-gateway-other gateway.gateway.svc:8080)"
result "another label cannot reach the executor" timeout "$(probe gateway probe-gateway-other executor.gateway.svc:8080)"

if (( failed )); then echo "gateway checks failed"; exit 1; fi
echo "all gateway checks passed"
