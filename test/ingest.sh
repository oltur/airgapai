#!/usr/bin/env bash
# Runs inside the VM as root. Tests that the ingestion Job from Git fills rag.chunks with the corpus,
# that a second run changes nothing, that the labels follow the rule and each reader role sees its
# share, and that the Job's network paths reach PostgreSQL and the embedding server only, with
# controls.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
k=(kubectl -n retrieval)
restricted_prefix=content/en/docs/concepts/security/
failed=0

result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
# sql PASSWORD_VARIABLE USER STATEMENT: over TCP with a password from the pod's environment.
sql() {
  "${k[@]}" exec deployment/postgres -- sh -c '
    PGPASSWORD="$(printenv "$1")" exec psql -h 127.0.0.1 -U "$2" -d rag -AtXq -v ON_ERROR_STOP=1 -c "$3" 2>&1
  ' sh "$@" 2>/dev/null | tr '\n' ' ' | sed 's/ $//'
}
as_role() { sql RETRIEVAL_PASSWORD retrieval "BEGIN; SET LOCAL ROLE $1; $2; COMMIT"; }
# Waits for the Job to finish, then prints its summary line, or why it has none.
job_summary() {
  local start=$SECONDS state uid
  while (( SECONDS - start < 1200 )); do
    state="$("${k[@]}" get job ingest -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{end}' 2>/dev/null)"
    case "$state" in
      *Complete*)
        uid="$("${k[@]}" get job ingest -o jsonpath='{.metadata.uid}')"
        "${k[@]}" logs -l "batch.kubernetes.io/controller-uid=$uid" --tail=-1 2>/dev/null | grep '^files='
        return ;;
      *Failed*) echo "job failed"; return ;;
    esac
    sleep 5
  done
  echo "job did not finish"
}
field() { tr ' ' '\n' <<<"$1" | awk -F= -v k="$2" '$1 == k {print $2}'; }

"${k[@]}" rollout status deployment/postgres --timeout=300s >/dev/null || { echo "FAIL PostgreSQL is not running"; exit 1; }

echo "== The Job from Git fills the table"
first="$(job_summary)"
echo "info first run: $first"
first="${first% seconds=*}"
chunks="$(field "$first" chunks)"
[[ $chunks =~ ^[1-9][0-9]*$ ]] || { echo "FAIL the Job reported no chunks: $first"; exit 1; }
# A run on an empty table inserts every chunk. A run that found rows already shows unchanged ones.
result "first run inserted or kept every chunk" "$chunks" "$(( $(field "$first" inserted) + $(field "$first" unchanged) ))"
count="SELECT count(*) FROM rag.chunks WHERE source = 'kubernetes-docs'"
result "rows match the chunks" "$chunks" "$(sql INGEST_PASSWORD ingest "$count")"
result "files match the paths" "$(field "$first" files)" \
  "$(sql INGEST_PASSWORD ingest "SELECT count(DISTINCT path) FROM rag.chunks WHERE source = 'kubernetes-docs'")"
result "every embedding has 768 dimensions and is not zero" 0 \
  "$(sql INGEST_PASSWORD ingest "SELECT count(*) FROM rag.chunks WHERE source = 'kubernetes-docs' AND (vector_dims(embedding) <> 768 OR vector_norm(embedding) = 0)")"
result "no markup left in the chunks" 0 \
  "$(sql INGEST_PASSWORD ingest "SELECT count(*) FROM rag.chunks WHERE source = 'kubernetes-docs' AND (content LIKE '%{{<%' OR content LIKE '%{{\%%' OR content LIKE '%<!--%')")"

echo "== A second run changes nothing"
before="$(sql INGEST_PASSWORD ingest "SELECT max(id), md5(string_agg(path || chunk_index || content_sha256 || classification || embedding::text, ',' ORDER BY id)) FROM rag.chunks")"
"${k[@]}" delete job ingest --wait >/dev/null
kubectl -n argocd annotate application retrieval argocd.argoproj.io/refresh=hard --overwrite >/dev/null
for _ in $(seq 60); do "${k[@]}" get job ingest >/dev/null 2>&1 && break; sleep 5; done
second="$(job_summary)"
echo "info second run: $second"
second="${second% seconds=*}"
result "Argo CD recreated the Job and it wrote nothing" \
  "files=$(field "$first" files) chunks=$chunks inserted=0 updated=0 relabelled=0 unchanged=$chunks deleted=0 reused=0" "$second"
result "rows, ids and embeddings unchanged" "$before" \
  "$(sql INGEST_PASSWORD ingest "SELECT max(id), md5(string_agg(path || chunk_index || content_sha256 || classification || embedding::text, ',' ORDER BY id)) FROM rag.chunks")"
# Argo CD replaces the Job only when Git changes it. Its sync starts when the live Job differs from Git,
# so after a fresh comparison the application must be in sync and the Job the same one.
uid="$("${k[@]}" get job ingest -o jsonpath='{.metadata.uid}')"
reconciled() { kubectl -n argocd get application retrieval -o jsonpath='{.status.reconciledAt}'; }
was="$(reconciled)"
sleep 1  # reconciledAt has a resolution of one second
kubectl -n argocd annotate application retrieval argocd.argoproj.io/refresh=hard --overwrite >/dev/null
for _ in $(seq 60); do [[ $(reconciled) != "$was" ]] && break; sleep 2; done
result "Argo CD compared the application again" changed "$([[ $(reconciled) != "$was" ]] && echo changed || echo "unchanged: $was")"
result "retrieval application in sync after the comparison" Synced/Healthy \
  "$(kubectl -n argocd get application retrieval -o jsonpath='{.status.sync.status}/{.status.health.status}')"
sleep 30  # time for a sync to have started, had one been needed
result "the Job is not replaced while Git is unchanged" "$uid" "$("${k[@]}" get job ingest -o jsonpath='{.metadata.uid}')"

echo "== Labels and roles"
by_label="SELECT coalesce(sum((classification = 'public')::int), 0) || '/' || coalesce(sum((classification = 'restricted')::int), 0) FROM rag.chunks WHERE source = 'kubernetes-docs'"
labels="$(sql INGEST_PASSWORD ingest "$by_label")"
public="${labels%/*}" restricted="${labels#*/}"
echo "info public/restricted chunks: $labels"
(( public > 0 && restricted > 0 )) && echo "ok   both labels present" || { echo "FAIL both labels present: $labels"; failed=1; }
result "restricted exactly under $restricted_prefix" 0 \
  "$(sql INGEST_PASSWORD ingest "SELECT count(*) FROM rag.chunks WHERE source = 'kubernetes-docs' AND (classification = 'restricted') <> (path LIKE '$restricted_prefix%')")"
result "unset role sees nothing" 0 "$(sql RETRIEVAL_PASSWORD retrieval "$count")"
result "technician sees the public chunks" "$public/0" "$(as_role technician "$by_label")"
result "engineer sees all chunks" "$public/$restricted" "$(as_role engineer "$by_label")"

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
cleanup() { "${k[@]}" delete pod probe-ingest probe-other --ignore-not-found --wait=false >/dev/null 2>&1; }
trap cleanup EXIT
probe_pod probe-ingest ingest | kubectl apply -f - >/dev/null
probe_pod probe-other other | kubectl apply -f - >/dev/null
"${k[@]}" wait --for=condition=Ready pod/probe-ingest pod/probe-other --timeout=120s >/dev/null || { echo "FAIL probe pods not ready"; exit 1; }
probe() { "${k[@]}" exec "$1" -- /probe connect "$2" 2>/dev/null; }
# The allowed paths, which also show the probe can connect at all.
result "ingest label reaches PostgreSQL" reached "$(probe probe-ingest postgres.retrieval.svc:5432)"
result "ingest label reaches the embedding server" reached "$(probe probe-ingest embed.inference.svc:8080)"
result "ingest label cannot reach the chat model" timeout "$(probe probe-ingest chat.inference.svc:8080)"
result "ingest label cannot reach Forgejo" timeout "$(probe probe-ingest forgejo.forgejo.svc:3000)"
result "ingest label cannot reach the registry" timeout "$(probe probe-ingest 10.43.0.50:5000)"
result "other label cannot reach PostgreSQL" timeout "$(probe probe-other postgres.retrieval.svc:5432)"
result "other label cannot reach the embedding server" timeout "$(probe probe-other embed.inference.svc:8080)"

if (( failed )); then echo "ingestion checks failed"; exit 1; fi
echo "all ingestion checks passed"
