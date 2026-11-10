#!/usr/bin/env bash
# Runs the retrieval service's database tests against a throwaway PostgreSQL container started from
# the pinned pgvector image, with the schema from deploy/retrieval/postgres.yaml. Needs Docker. The
# tests skip without a database, so this is what runs them, here and in CI.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
source "$root/infra/versions.env"
name="airgapai-retrieval-test-$$"
tmp="$(mktemp -d)"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

# The init script, exactly as the cluster's ConfigMap holds it.
python3 -I - "$root/deploy/retrieval/postgres.yaml" "$tmp/10-schema.sh" <<'PY'
import sys
text = open(sys.argv[1]).read()
start = text.index("  10-schema.sh: |\n") + len("  10-schema.sh: |\n")
lines = []
for line in text[start:].splitlines():
    if line.startswith("---") or (line and not line.startswith("    ")):
        break
    lines.append(line[4:])
open(sys.argv[2], "w").write("\n".join(lines).rstrip() + "\n")
PY
grep -q "FORCE ROW LEVEL SECURITY" "$tmp/10-schema.sh" || { echo "could not read the schema from the manifest"; exit 1; }

# Test-only passwords, for a container that listens on loopback and is removed on exit.
docker create --name "$name" -p 127.0.0.1::5432 -e POSTGRES_DB=rag -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=test-superuser -e INGEST_PASSWORD=test-ingest -e RETRIEVAL_PASSWORD=test-retrieval \
  -e "POSTGRES_INITDB_ARGS=--auth-local=scram-sha-256 --auth-host=scram-sha-256" "$PGVECTOR_IMAGE" >/dev/null
docker cp "$tmp/10-schema.sh" "$name:/docker-entrypoint-initdb.d/10-schema.sh"
docker start "$name" >/dev/null
port="$(docker port "$name" 5432/tcp | head -1 | sed 's/.*://')"
# The entrypoint restarts the server after initialising, so wait for a login as the retrieval role.
for _ in $(seq 60); do
  docker exec -e PGPASSWORD=test-retrieval "$name" psql -h 127.0.0.1 -U retrieval -d rag -Atc "SELECT 1" >/dev/null 2>&1 \
    && docker logs "$name" 2>&1 | grep -q "PostgreSQL init process complete" && break
  sleep 1
done
dsn() { echo "postgres://$1:test-$1@127.0.0.1:$port/rag?sslmode=disable"; }
cd "$root/images/retrieval"
toolchain="$(awk '$1 == "toolchain" {print $2}' go.mod)"
status=0
RETRIEVAL_TEST_DSN="$(dsn retrieval)" INGEST_TEST_DSN="$(dsn ingest)" GOTOOLCHAIN="$toolchain" \
  go test -count=1 -v ./... >"$tmp/out" 2>&1 || status=$?
grep -E '^--- |_test.go:' "$tmp/out" || true
(( status == 0 )) || { tail -20 "$tmp/out"; echo "retrieval database tests failed"; exit 1; }
# A skipped test would mean the database was not reached, which must not pass as success.
if grep -q -- '--- SKIP' "$tmp/out"; then echo "a database test skipped"; exit 1; fi
echo "retrieval database tests passed"
