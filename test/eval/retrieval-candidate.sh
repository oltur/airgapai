#!/usr/bin/env bash
# Runs inside the VM as root, for test/eval/retrieval.py --candidate on the host. ACTION=up sets up a
# candidate's index beside the deployed one, and ACTION=down removes it. ACTION=check fails if a
# candidate's copies are there already, which up would remove: the host checks first, so that a
# second run cannot break one in progress, and tears down only what its own up made.
#
# Up: a database, rag_candidate, gets the deployed rag database's schema, copied with pg_dump, so its
# table, row-level security, grants and index are the deployed ones. A copy of the deployed ingest
# Job fills it, with CANDIDATE's image: "deployed" for the deployed image, the control, or a tag of
# the ingest-candidate test image, such as "worse-chunking", which must carry CHUNKING_SHA256, the
# hash of the candidate's file on the host, as its label. A copy of the deployed retrieval service
# then serves it, under a component label the retrieval Service does not select. Both copies keep the
# deployed app labels, so the same network rules apply to them, and Argo CD does not track them.
#
# Output, one record per line: "ingest IMAGE", "retrieval IMAGE" and "rows LABEL=N..." for what was
# set up. Anything else is progress, for a reader.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
psql_super() { # database sql: as the superuser, whose password is in the pod's environment
  kubectl -n retrieval exec deployment/postgres -- sh -c \
    "PGPASSWORD=\"\$POSTGRES_PASSWORD\" PGOPTIONS=\"-c client_min_messages=warning\" psql -v ON_ERROR_STOP=1 -h 127.0.0.1 -U postgres -d $1 -Atc \"$2\""
}
down() {
  kubectl -n retrieval delete deployment retrieval-candidate --ignore-not-found --timeout=120s >/dev/null 2>&1
  kubectl -n retrieval delete job ingest-candidate --ignore-not-found --timeout=120s >/dev/null 2>&1
  kubectl -n retrieval wait --for=delete pod -l 'app in (ingest,retrieval),component=candidate' --timeout=120s >/dev/null 2>&1
  psql_super postgres "DROP DATABASE IF EXISTS rag_candidate WITH (FORCE)" >/dev/null
}

case ${ACTION:-} in
  down) down; exit ;;
  check)
    left="$(kubectl -n retrieval get job,deployment -l component=candidate -o name 2>&1)"
    [[ -z $left ]] || { echo "FAIL another candidate run is in progress, or left: $left"; exit 1; }
    exit ;;
  up) ;;
  *) echo "FAIL ACTION must be up, down or check"; exit 1 ;;
esac
[[ ${CANDIDATE:-} =~ ^[a-z0-9-]+$ ]] || { echo "FAIL CANDIDATE is not a tag: ${CANDIDATE:-}"; exit 1; }
kubectl -n retrieval rollout status deployment/retrieval --timeout=120s >/dev/null || { echo "FAIL the retrieval service is not running"; exit 1; }
image="$(kubectl -n retrieval get job ingest -o jsonpath='{.spec.template.spec.containers[0].image}')"
if [[ $CANDIDATE != deployed ]]; then
  digest="$(registry_digest ingest-candidate "$CANDIDATE")"
  [[ $digest == sha256:* ]] || { echo "FAIL no ingest-candidate:$CANDIDATE in the registry"; exit 1; }
  built="$(registry_label ingest-candidate "$CANDIDATE" airgapai.chunking-sha256)"
  [[ -n ${CHUNKING_SHA256:-} && $built == "$CHUNKING_SHA256" ]] ||
    { echo "FAIL ingest-candidate:$CANDIDATE was built from another chunking than the host's, ${built:-none}: rebuild"; exit 1; }
  image="10.43.0.50:5000/ingest-candidate@$digest"
fi

down
psql_super postgres "CREATE DATABASE rag_candidate" >/dev/null || { echo "FAIL rag_candidate could not be created"; exit 1; }
# shellcheck disable=SC2016 # expanded in the pod, where the password is
kubectl -n retrieval exec deployment/postgres -- sh -c \
  'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -h 127.0.0.1 -U postgres -d rag --schema-only \
   | PGPASSWORD="$POSTGRES_PASSWORD" psql -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -U postgres -d rag_candidate' >/dev/null ||
  { echo "FAIL the rag schema could not be copied"; exit 1; }
[[ $(psql_super rag_candidate "SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'rag.chunks'::regclass") == t ]] ||
  { echo "FAIL rag_candidate's chunks table has no forced row-level security"; exit 1; }
[[ $(psql_super rag_candidate "SELECT count(*) FROM rag.chunks") == 0 ]] || { echo "FAIL rag_candidate is not empty"; exit 1; }

# copy KIND NAME IMAGE|"": the deployed object as a candidate, reading rag_candidate.
copy() {
  kubectl -n retrieval get "$1" "$2" -o json | python3 -I -c '
import json, sys
kind, image = sys.argv[1:3]
d = json.load(sys.stdin)
d["metadata"] = {"name": d["metadata"]["name"] + "-candidate", "namespace": "retrieval"}
d.pop("status", None)
spec = d["spec"]
template = spec["template"]
labels = {k: v for k, v in template["metadata"]["labels"].items()
          if k in ("app", "component")}  # not the labels the Job controller adds
labels["component"] = "candidate"
template["metadata"] = {"labels": labels}
if kind == "job":
    spec.pop("selector", None)
    spec.pop("manualSelector", None)
else:
    spec["selector"] = {"matchLabels": labels}
containers = template["spec"]["containers"]
assert len(containers) == 1
if image:
    containers[0]["image"] = image
found = [e for e in containers[0]["env"] if e["name"] == "PGDATABASE"]
assert len(found) == 1 and found[0]["value"] == "rag", found
found[0]["value"] = "rag_candidate"
print(json.dumps(d))' "$1" "$3" | kubectl apply -f - >/dev/null
}

started=$SECONDS
copy job ingest "$image" || { echo "FAIL the ingest Job could not be copied"; exit 1; }
echo "ingest-candidate started with $image"
# Complete or Failed, whichever comes first: a Job that fails must not wait out the timeout.
state=""
until [[ $state == *Complete* || $state == *Failed* ]] || (( SECONDS - started > 1800 )); do
  sleep 5
  state="$(kubectl -n retrieval get job ingest-candidate -o jsonpath='{.status.conditions[?(@.status=="True")].type}')"
done
[[ $state == *Complete* ]] ||
  { echo "FAIL the candidate's ingestion did not complete: ${state:-timed out}"; kubectl -n retrieval logs job/ingest-candidate --tail=5; exit 1; }
echo "ingestion took $((SECONDS - started)) s: $(kubectl -n retrieval logs job/ingest-candidate --tail=1)"
copy deployment retrieval "" || { echo "FAIL the retrieval service could not be copied"; exit 1; }
kubectl -n retrieval rollout status deployment/retrieval-candidate --timeout=120s >/dev/null ||
  { echo "FAIL the candidate's retrieval service did not start"; exit 1; }
echo "ingest $(kubectl -n retrieval get job ingest-candidate -o jsonpath='{.spec.template.spec.containers[0].image}')"
echo "retrieval $(kubectl -n retrieval get deployment retrieval-candidate -o jsonpath='{.spec.template.spec.containers[0].image}')"
echo "rows $(psql_super rag_candidate "SELECT string_agg(classification || '=' || n, ' ' ORDER BY classification) FROM (SELECT classification, count(*) n FROM rag.chunks GROUP BY 1) c")"
