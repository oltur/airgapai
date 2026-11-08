#!/usr/bin/env bash
# Runs inside the VM as root. Tests that row-level security decides which chunks each reader role sees,
# that an unset role sees nothing, and that the retrieval login cannot widen its own access, with
# controls. Test rows are labelled source = 'rls-test' and removed at the end.
set -uo pipefail

kubectl=(/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml -n retrieval)
failed=0

result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
# sql PASSWORD_VARIABLE USER STATEMENT...: each statement runs as its own transaction, over TCP with a
# password. The passwords are already in the pod's environment, so none appears on a command line.
# No -i: this script arrives on stdin, and exec would swallow the rest of it.
sql() {
  "${kubectl[@]}" exec deployment/postgres -- sh -c '
    variable=$1 user=$2; shift 2; n=$#
    for s in "$@"; do set -- "$@" -c "$s"; done; shift "$n"
    PGPASSWORD="$(printenv "$variable")" exec psql -h 127.0.0.1 -U "$user" -d rag -AtXq -v ON_ERROR_STOP=1 "$@" 2>&1
  ' sh "$@" 2>/dev/null | tr '\n' ' ' | sed 's/ $//'  # psql's own errors are in its output already
}
denied() { # output: says only whether the server refused
  case "$1" in
    *"permission denied"*|*"must be owner"*|*"password authentication failed"*) echo denied ;;
    *) echo "allowed: $1" ;;
  esac
}

"${kubectl[@]}" rollout status deployment/postgres --timeout=300s >/dev/null || { echo "FAIL PostgreSQL is not running"; exit 1; }
for _ in $(seq 30); do [[ $(sql POSTGRES_PASSWORD postgres "SELECT 1") == 1 ]] && break; sleep 2; done

echo "== Set-up"
result "pgvector version" 0.8.7 "$(sql POSTGRES_PASSWORD postgres "SELECT extversion FROM pg_extension WHERE extname = 'vector'")"
result "row-level security enabled and forced" "t|t" \
  "$(sql POSTGRES_PASSWORD postgres "SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'rag.chunks'::regclass")"
result "retrieval login: superuser, bypass, inherit" "f|f|f" \
  "$(sql POSTGRES_PASSWORD postgres "SELECT rolsuper, rolbypassrls, rolinherit FROM pg_roles WHERE rolname = 'retrieval'")"
result "table owner is not a login" "airgapai_owner|f" \
  "$(sql POSTGRES_PASSWORD postgres "SELECT r.rolname, r.rolcanlogin FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE c.oid = 'rag.chunks'::regclass")"
result "right password accepted (control)" retrieval "$(sql RETRIEVAL_PASSWORD retrieval "SELECT current_user")"
result "wrong password refused" denied "$(denied "$(sql INGEST_PASSWORD retrieval "SELECT current_user")")"

echo "== Rows"
vector="array_fill(0.1::real, ARRAY[768])::vector"
result "test rows written" "" "$(sql INGEST_PASSWORD ingest "DELETE FROM rag.chunks WHERE source = 'rls-test'" \
  "INSERT INTO rag.chunks (source, path, chunk_index, classification, content, content_sha256, embedding) VALUES
     ('rls-test', 'a.md', 0, 'public', 'public test chunk', 'none', $vector),
     ('rls-test', 'b.md', 0, 'restricted', 'restricted test chunk', 'none', $vector)")"
count="SELECT count(*) FROM rag.chunks WHERE source = 'rls-test'"
labels="SELECT coalesce(string_agg(classification, ',' ORDER BY classification), 'none') FROM rag.chunks WHERE source = 'rls-test'"
as_role() { sql RETRIEVAL_PASSWORD retrieval "BEGIN; SET LOCAL ROLE $1; $2; COMMIT"; }
result "the writer sees both test rows (control)" 2 "$(sql INGEST_PASSWORD ingest "$count")"
result "unset role sees nothing" 0 "$(sql RETRIEVAL_PASSWORD retrieval "$count")"
result "technician sees public only" public "$(as_role technician "$labels")"
result "engineer sees both (control)" public,restricted "$(as_role engineer "$labels")"
# The policy, not the query, does the filtering: widened in a transaction that rolls back, it leaks.
result "technician with a widened policy (control)" public,restricted \
  "$(sql POSTGRES_PASSWORD postgres "BEGIN; ALTER POLICY technician_public ON rag.chunks USING (true); SET LOCAL ROLE technician; $labels; ROLLBACK")"
result "technician policy unchanged after the control" "(classification = 'public'::text)" \
  "$(sql POSTGRES_PASSWORD postgres "SELECT qual FROM pg_policies WHERE policyname = 'technician_public'")"
result "technician asking for the restricted row by name" 0 \
  "$(as_role technician "SELECT count(*) FROM rag.chunks WHERE source = 'rls-test' AND path = 'b.md'")"
result "role ends with its transaction" 0 \
  "$(sql RETRIEVAL_PASSWORD retrieval "BEGIN; SET LOCAL ROLE engineer; COMMIT" "$count")"
result "SET LOCAL outside a transaction has no effect" 0 \
  "$(sql RETRIEVAL_PASSWORD retrieval "SET LOCAL ROLE engineer" "$count" | sed 's/^WARNING: .* //')"
result "iterative index scans can be turned on" relaxed_order \
  "$(as_role engineer "SET LOCAL hnsw.iterative_scan = relaxed_order; SHOW hnsw.iterative_scan")"

echo "== The retrieval login cannot widen its access"
for role in ingest airgapai_owner postgres; do
  result "switch to $role" denied "$(denied "$(sql RETRIEVAL_PASSWORD retrieval "SET ROLE $role")")"
done
result "insert a row" denied "$(denied "$(as_role engineer "INSERT INTO rag.chunks (source, path, chunk_index, classification, content, content_sha256, embedding) VALUES ('rls-test', 'c.md', 0, 'public', 'x', 'none', $vector)")")"
result "turn row-level security off" denied "$(denied "$(sql RETRIEVAL_PASSWORD retrieval "ALTER TABLE rag.chunks DISABLE ROW LEVEL SECURITY")")"

echo "== FORCE binds the owner"
result "owner, forced" 0 "$(sql POSTGRES_PASSWORD postgres "BEGIN; SET LOCAL ROLE airgapai_owner; $count; ROLLBACK")"
# The control runs in a transaction that rolls back, so the table is never left unforced.
result "owner, not forced (control)" 2 \
  "$(sql POSTGRES_PASSWORD postgres "BEGIN; ALTER TABLE rag.chunks NO FORCE ROW LEVEL SECURITY; SET LOCAL ROLE airgapai_owner; $count; ROLLBACK")"
result "still forced after the control" t \
  "$(sql POSTGRES_PASSWORD postgres "SELECT relforcerowsecurity FROM pg_class WHERE oid = 'rag.chunks'::regclass")"

sql INGEST_PASSWORD ingest "DELETE FROM rag.chunks WHERE source = 'rls-test'" >/dev/null
result "test rows removed" 0 "$(sql INGEST_PASSWORD ingest "$count")"

if (( failed )); then echo "row-level security checks failed"; exit 1; fi
echo "all row-level security checks passed"
