#!/usr/bin/env bash
# Runs inside the VM as root, after make test-ingest has filled the table. Tests that the retrieval
# service gives the same question different chunks under two roles, nothing with no role, never a
# restricted chunk to a technician, refuses unknown roles, and that its network paths reach PostgreSQL
# and the embedding server only, with controls. QUERIES holds test/retrieval/queries.txt in base64.
# Requests go through kubectl port-forward, which does not pass through the network policy.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
k=(kubectl -n retrieval)
port=18090
failed=0

result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
[[ -n ${QUERIES:-} ]] || { echo "FAIL QUERIES is not set"; exit 1; }
"${k[@]}" rollout status deployment/retrieval --timeout=300s >/dev/null || { echo "FAIL the retrieval service is not running"; exit 1; }

"${k[@]}" port-forward svc/retrieval "$port:8080" >/dev/null 2>&1 &
forward=$!
cleanup() {
  kill "$forward" 2>/dev/null
  "${k[@]}" delete pod probe-retrieval probe-other --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT
for _ in $(seq 30); do curl -fs "http://127.0.0.1:$port/ready" >/dev/null && break; sleep 1; done
result "service ready through port-forward (control: it listens)" ok "$(curl -fs "http://127.0.0.1:$port/ready")"

echo "== Roles"
QUERIES="$QUERIES" PORT="$port" python3 - <<'PY' || failed=1
import base64, json, os, urllib.error, urllib.request

url = f"http://127.0.0.1:{os.environ['PORT']}/search"
queries = [q for q in base64.b64decode(os.environ["QUERIES"]).decode().splitlines()
           if q.strip() and not q.startswith("#")]
failed = False

def search(query, role="", k=5, scan="hnsw"):
    body = json.dumps({"query": query, "role": role, "k": k, "scan": scan}).encode()
    request = urllib.request.Request(url, body, {"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return response.status, json.load(response)["results"]
    except urllib.error.HTTPError as error:
        return error.code, None

def result(name, expected, actual):
    global failed
    if expected == actual:
        print(f"ok   {name}: {actual}")
    else:
        print(f"FAIL {name}: expected {expected}, got {actual}")
        failed = True

def labels(results):
    return sorted({r["classification"] for r in results})

question = "What are the three Pod Security Standards profiles?"
_, engineer = search(question, "engineer")
_, technician = search(question, "technician")
print("info engineer:  ", [f"{r['path'].split('concepts/')[1]}#{r['chunk_index']}" for r in engineer])
print("info technician:", [f"{r['path'].split('concepts/')[1]}#{r['chunk_index']}" for r in technician])
result("engineer gets the restricted page on the question",
       True, any(r["path"].endswith("security/pod-security-standards.md") for r in engineer))
result("technician gets only public chunks", ["public"], labels(technician))
result("technician still gets a full five", 5, len(technician))
key = lambda rs: {(r["path"], r["chunk_index"]) for r in rs}  # noqa: E731
result("the two roles get different chunks", True, key(engineer) != key(technician))

for scan in ("hnsw", "hnsw-strict", "exact"):
    result(f"no role gets nothing ({scan})", (200, []), search(question, "", 5, scan))
for role in ("Engineer", "ingest", "postgres", "engineer; RESET ROLE"):
    result(f"unknown role {role!r} refused", 400, search(question, role)[0])
result("k over the limit refused", 400, search(question, "engineer", 21)[0])

# The leak test: every query, every scan, the most results allowed.
leaks = sum(r["classification"] != "public"
            for q in queries for scan in ("hnsw", "hnsw-strict", "exact")
            for r in search(q, "technician", 20, scan)[1])
result(f"restricted chunks returned to a technician over {len(queries)} queries, 3 scans, k=20", 0, leaks)
reached = sum(r["classification"] == "restricted" for q in queries for r in search(q, "engineer", 20)[1])
print(f"info restricted chunks returned to an engineer for the same queries, hnsw, k=20: {reached}")
result("the same queries reach restricted chunks as an engineer (control)", True, reached > 0)
# Alternating on the service's pooled connections: a role must not outlast its request.
after = 0
for q in queries:
    search(q, "engineer")
    after += len(search(q, "")[1])
result(f"no-role requests straight after engineer requests, {len(queries)} pairs", 0, after)
raise SystemExit(1 if failed else 0)
PY

echo "== Network paths"
digest="$(curl -sI -H 'Accept: application/vnd.oci.image.manifest.v1+json' http://10.43.0.50:5000/v2/probe/manifests/v1 | awk 'tolower($1) == "docker-content-digest:" {print $2}' | tr -d '\r')"
[[ $digest == sha256:* ]] || { echo "FAIL could not read the probe digest"; exit 1; }
probe_pod() { # name app-label
  cat <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: retrieval, labels: {app: $2}}
spec:
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: probe
      image: 10.43.0.50:5000/probe@$digest
      args: ["serve", ":8080"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
}
probe_pod probe-retrieval retrieval | kubectl apply -f - >/dev/null
probe_pod probe-other other | kubectl apply -f - >/dev/null
"${k[@]}" wait --for=condition=Ready pod/probe-retrieval pod/probe-other --timeout=120s >/dev/null || { echo "FAIL probe pods not ready"; exit 1; }
probe() { "${k[@]}" exec "$1" -- /probe connect "$2" 2>/dev/null; }
result "retrieval label reaches PostgreSQL" reached "$(probe probe-retrieval postgres.retrieval.svc:5432)"
result "retrieval label reaches the embedding server" reached "$(probe probe-retrieval embed.inference.svc:8080)"
result "retrieval label cannot reach the chat model" timeout "$(probe probe-retrieval chat.inference.svc:8080)"
result "retrieval label cannot reach Forgejo" timeout "$(probe probe-retrieval forgejo.forgejo.svc:3000)"
result "another pod cannot call the retrieval service" timeout "$(probe probe-other retrieval.retrieval.svc:8080)"
result "the retrieval label cannot call it either" timeout "$(probe probe-retrieval retrieval.retrieval.svc:8080)"

if (( failed )); then echo "retrieval checks failed"; exit 1; fi
echo "all retrieval checks passed"
