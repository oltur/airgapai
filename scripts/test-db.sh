#!/usr/bin/env bash
# Runs the Go tests that need PostgreSQL, for the retrieval service and the gateway, against one
# throwaway container started from the pinned pgvector image. The retrieval schema is read from
# deploy/retrieval/postgres.yaml and the gateway schema from images/gateway/schema.sql. Needs Docker.
# The tests skip without a database, so this is what runs them, here and in CI, and it fails if any
# of them skips.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
source "$root/infra/versions.env"
name="airgapai-db-test-$$"
tmp="$(mktemp -d)"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

# The retrieval init script, exactly as the cluster's ConfigMap holds it.
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
cp "$root/images/gateway/schema.sql" "$tmp/gateway.sql"
cat >"$tmp/20-gateway.sh" <<'EOF'
set -eu
psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "CREATE DATABASE gateway"
psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d gateway -v gateway_password="$GATEWAY_PASSWORD" \
  -f /docker-entrypoint-initdb.d/gateway.sql.txt
EOF

# Test-only passwords, for a container that listens on loopback and is removed on exit.
docker create --name "$name" -p 127.0.0.1::5432 -e POSTGRES_DB=rag -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=test-postgres -e INGEST_PASSWORD=test-ingest -e RETRIEVAL_PASSWORD=test-retrieval \
  -e GATEWAY_PASSWORD=test-gateway_app \
  -e "POSTGRES_INITDB_ARGS=--auth-local=scram-sha-256 --auth-host=scram-sha-256" "$PGVECTOR_IMAGE" >/dev/null
docker cp "$tmp/10-schema.sh" "$name:/docker-entrypoint-initdb.d/10-schema.sh"
docker cp "$tmp/20-gateway.sh" "$name:/docker-entrypoint-initdb.d/20-gateway.sh"
# A name the entrypoint does not run by itself: 20-gateway.sh applies it to the gateway database.
docker cp "$tmp/gateway.sql" "$name:/docker-entrypoint-initdb.d/gateway.sql.txt"
docker start "$name" >/dev/null
port="$(docker port "$name" 5432/tcp | head -1 | sed 's/.*://')"
# The entrypoint restarts the server after initialising, so wait for that and a working login.
for _ in $(seq 60); do
  docker logs "$name" 2>&1 | grep -q "PostgreSQL init process complete" \
    && docker exec -e PGPASSWORD=test-gateway_app "$name" psql -h 127.0.0.1 -U gateway_app -d gateway -Atc "SELECT 1" >/dev/null 2>&1 \
    && break
  sleep 1
done
docker logs "$name" 2>&1 | grep -q "PostgreSQL init process complete" || { docker logs "$name" | tail -20; echo "PostgreSQL did not initialise"; exit 1; }
dsn() { echo "postgres://$1:test-$1@127.0.0.1:$port/$2?sslmode=disable"; }

run() { # module, then environment assignments
  local module=$1 status=0 toolchain
  shift
  toolchain="$(awk '$1 == "toolchain" {print $2}' "$root/images/$module/go.mod")"
  (cd "$root/images/$module" && env "$@" GOTOOLCHAIN="$toolchain" go test -count=1 -v ./... >"$tmp/$module.out" 2>&1) || status=$?
  echo "== $module"
  grep -E '^\s*--- |_test.go:' "$tmp/$module.out" || true
  (( status == 0 )) || { tail -20 "$tmp/$module.out"; echo "$module database tests failed"; exit 1; }
  # A skipped test would mean the database was not reached, which must not pass as success.
  if grep -q -- '--- SKIP' "$tmp/$module.out"; then echo "a $module database test skipped"; exit 1; fi
}
run retrieval RETRIEVAL_TEST_DSN="$(dsn retrieval rag)" INGEST_TEST_DSN="$(dsn ingest rag)"
run gateway GATEWAY_TEST_DSN="$(dsn gateway_app gateway)" GATEWAY_ADMIN_DSN="$(dsn postgres gateway)"
echo "all database tests passed"
