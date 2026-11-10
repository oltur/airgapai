#!/usr/bin/env bash
# Runs the Go tests that need PostgreSQL, for the retrieval service, the gateway's approval core and
# the executor, against one throwaway container started from the pinned pgvector image. The init
# scripts are read from the postgres-init ConfigMap in deploy/retrieval/postgres.yaml, so the tests
# get exactly the cluster's schema. Needs Docker. The tests skip without a database, so this is what
# runs them, here and in CI, and it fails if any of them skips.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
source "$root/infra/versions.env"
name="airgapai-db-test-$$"
tmp="$(mktemp -d)"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

# Each init script, exactly as the ConfigMap holds it: a key at two spaces, its text at four.
python3 -I - "$root/deploy/retrieval/postgres.yaml" "$tmp" <<'PY'
import os, re, sys
text = open(sys.argv[1]).read()
data = text[text.index("name: postgres-init"):].split("\n---", 1)[0]
scripts, current = {}, None
for line in data.splitlines():
    key = re.match(r"^  ([0-9a-z-]+\.sh): \|$", line)
    if key:
        current = scripts.setdefault(key.group(1), [])
    elif current is not None and (line.startswith("    ") or line == ""):
        current.append(line[4:])
    elif current is not None:
        current = None
assert scripts, "no init scripts found"
for name, lines in scripts.items():
    open(os.path.join(sys.argv[2], name), "w").write("\n".join(lines).rstrip() + "\n")
print(" ".join(sorted(scripts)))
PY
grep -q "FORCE ROW LEVEL SECURITY" "$tmp/10-schema.sh" && grep -q "CREATE TABLE gateway.audit" "$tmp/20-gateway.sh" \
  || { echo "could not read the init scripts from the manifest"; exit 1; }

# Test-only passwords, for a container that listens on loopback and is removed on exit.
docker create --name "$name" -p 127.0.0.1::5432 -e POSTGRES_DB=rag -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=test-postgres -e INGEST_PASSWORD=test-ingest -e RETRIEVAL_PASSWORD=test-retrieval \
  -e GATEWAY_PASSWORD=test-gateway_app \
  -e "POSTGRES_INITDB_ARGS=--auth-local=scram-sha-256 --auth-host=scram-sha-256" "$PGVECTOR_IMAGE" >/dev/null
for script in "$tmp"/*.sh; do docker cp "$script" "$name:/docker-entrypoint-initdb.d/"; done
docker start "$name" >/dev/null
port="$(docker port "$name" 5432/tcp | head -1 | sed 's/.*://')"
# The entrypoint restarts the server after initialising, so wait for that and a working login. The
# logs go to a file first: grep -q stops reading at its match, and under pipefail the writer's broken
# pipe would fail the check.
initialised() {
  docker logs "$name" >"$tmp/postgres.log" 2>&1
  grep -q "PostgreSQL init process complete" "$tmp/postgres.log"
}
ready=0
for _ in $(seq 60); do
  if initialised && docker exec -e PGPASSWORD=test-gateway_app "$name" \
    psql -h 127.0.0.1 -U gateway_app -d gateway -Atc "SELECT 1" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
(( ready )) || { tail -20 "$tmp/postgres.log"; echo "PostgreSQL did not initialise"; exit 1; }
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
run executor GATEWAY_TEST_DSN="$(dsn gateway_app gateway)" GATEWAY_ADMIN_DSN="$(dsn postgres gateway)"
echo "all database tests passed"
