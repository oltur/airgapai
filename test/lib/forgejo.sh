# shellcheck shell=bash
# Shared by the in-VM gateway and executor tests: Forgejo as its bootstrap administrator, the test
# users and their teams, and audit counts. Sent ahead of each test script, like probe.sh.

json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
admin_token="$(kubectl -n forgejo get secret forgejo-admin -o jsonpath='{.data.token}' | base64 -d)"
fj() { # method path [json]: prints the body, then the status on its own line
  kubectl -n forgejo exec deployment/forgejo -- curl -s -w '\n%{http_code}' -X "$1" -H "Authorization: token $admin_token" \
    -H 'Content-Type: application/json' ${3:+-d "$3"} "http://localhost:3000/api/v1$2" 2>/dev/null
}
status() { fj "$@" | tail -1; }
team() { fj GET /orgs/maintenance/teams | sed '$d' | json "next(t['id'] for t in d if t['name'] == '$1')"; }
issues() { fj GET "/repos/maintenance/tickets/issues?state=all&type=issues&limit=50" | sed '$d' | json "len(d)"; }

# test_user NAME [TEAM]: creates the user, or resets the password of one an earlier test made, and puts
# them in TEAM. The password is kept in passwords[NAME] for tests that sign in.
declare -A passwords
test_user() {
  local pw
  pw="$(openssl rand -hex 16)"
  # shellcheck disable=SC2034 # read by the test scripts sent after this file
  passwords[$1]=$pw
  [[ $(status POST /admin/users "{\"username\":\"$1\",\"email\":\"$1@airgapai.local\",\"password\":\"$pw\",\"must_change_password\":false}") == 201 ]] \
    || status PATCH "/admin/users/$1" "{\"login_name\":\"$1\",\"source_id\":0,\"password\":\"$pw\",\"must_change_password\":false}" >/dev/null
  [[ -z ${2:-} ]] || status PUT "/teams/$(team "$2")/members/$1" >/dev/null
}

# decisions ACTION DECISION: the audit count, as the superuser, so checks can compare before and after.
# Fixed words from the test scripts, so they can go into the SQL as they are.
decisions() {
  kubectl -n retrieval exec deployment/postgres -- sh -c "PGPASSWORD=\"\$POSTGRES_PASSWORD\" psql -h 127.0.0.1 -U postgres \
    -d gateway -Atc \"SELECT count(*) FROM gateway.audit WHERE action = '$1' AND decision = '$2'\"" 2>/dev/null
}
