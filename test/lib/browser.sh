# shellcheck shell=bash
# Shared by the in-VM gateway tests: a scripted browser. Sent ahead of each test script, like probe.sh.
# The tests port-forward the gateway to 18092, Forgejo to 13000, and the stub's gateway to 18093.

# A browser per user, with its own cookies, that shows redirects rather than following them. It
# prints the status, then the redirect target if there is one, and keeps the last page in a file.
# "alice" uses the deployed gateway on 18092, and "alice@stub" the copy on 18093 that test/lib/stub.sh
# runs against the compromised-model stub, each with its own cookies.
cat >/tmp/gateway-browser.py <<'PY'
import html, http.cookiejar, re, socket, sys, urllib.error, urllib.parse, urllib.request
FJ = "http://forgejo.forgejo.svc:13000"
resolve = socket.getaddrinfo  # the browser's name for Forgejo goes to the local tunnel
socket.getaddrinfo = lambda host, *a, **k: resolve("127.0.0.1" if host == "forgejo.forgejo.svc" else host, *a, **k)
token, cmd, args = sys.argv[1], sys.argv[2], sys.argv[3:]
user, _, where = token.partition("@")
GW = "http://127.0.0.1:18093" if where == "stub" else "http://127.0.0.1:18092"
jar = http.cookiejar.LWPCookieJar(f"/tmp/gateway-test-{token}.jar")
try:
    jar.load(ignore_discard=True, ignore_expires=True)
except FileNotFoundError:
    pass
class Stay(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a):
        return None
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), Stay)
def go(url, form=None, headers=None, timeout=60):
    req = urllib.request.Request(url, data=urllib.parse.urlencode(form).encode() if form is not None else None,
                                 headers=headers or {})
    try:
        r = opener.open(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        r = e
    return r.getcode(), r.headers.get("Location") or "", r.read().decode()
def done(status, location="", page=""):
    open(f"/tmp/gateway-test-{token}.html", "w").write(page)
    jar.save(ignore_discard=True, ignore_expires=True)
    print(f"{status} {location}" if location else status)
    sys.exit(0)
def page_csrf():
    m = re.search(r'name="csrf" value="([^"]+)"', open(f"/tmp/gateway-test-{token}.html").read())
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
if cmd == "chat":  # QUESTION: a turn with the model, which can take minutes on CPU
    done(*go(GW + "/chat", {"csrf": page_csrf(), "question": args[0]}, {"Origin": GW}, timeout=420))
sys.exit(f"unknown command {cmd}")
PY
browse() { python3 -I /tmp/gateway-browser.py "$@"; }
shown() { grep -o -F -- "$2" "/tmp/gateway-test-$1.html" | wc -l; } # occurrences, not lines
# proposal_links USER: every distinct proposal linked from the user's last page; proposal_link, the first.
proposal_links() { grep -o '/proposals/[0-9a-f-]\{36\}' "/tmp/gateway-test-$1.html" | sort -u; }
proposal_link() { grep -o '/proposals/[0-9a-f-]\{36\}' "/tmp/gateway-test-$1.html" | head -1; }
# ask USER QUESTION: one chat turn, on a freshly loaded chat page for its CSRF token. Prints the status.
ask() { browse "$1" get /chat >/dev/null && browse "$1" chat "$2"; }
