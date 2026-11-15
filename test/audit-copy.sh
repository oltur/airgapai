#!/usr/bin/env bash
# Runs on the host. Tests the off-VM copy of the gateway's audit log (ADR 0024): it copies the VM's
# rows, copies new honest rows, and then catches a row that the database superuser rewrote with every
# hash after it recomputed, a change the VM's own check cannot see. The rewritten rows are put back
# from the copy, and the test checks the VM's log matches it again. Copies go to a temporary directory,
# not to the lab's own copy under DATA_DIR.
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
failed=0
result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
dir="$(mktemp -d)"
k8s='sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml'
auditcopy() { go -C "$root/infra/auditcopy" run . -root "$root" -dir "$dir" "$@" 2>&1; }
as_superuser() { # SQL on stdin, as an attacker with the database superuser would run it
  "$root/infra/vm-ssh.sh" "$k8s -n retrieval exec -i deployment/postgres -- sh -c 'PGPASSWORD=\"\$POSTGRES_PASSWORD\" psql -h 127.0.0.1 -U postgres -d gateway -v ON_ERROR_STOP=1 -qAt'"
}
executor() { "$root/infra/vm-ssh.sh" "$k8s -n gateway exec deployment/executor -- /executor $*" 2>&1; }
verified() { executor verify | python3 -c 'import json,sys; print(json.load(sys.stdin)["first_bad_row"])'; }
forged=0
cleanup() {
  # If the test stopped with the log rewritten, put it back from the copy before removing the copy.
  if (( forged )); then auditcopy restore | as_superuser >/dev/null; fi
  rm -rf "$dir"
}
trap cleanup EXIT

echo "== Copying"
first="$(auditcopy)"
result "the first copy" ok "$(cut -d' ' -f1 <<<"$first")"
echo "info $first"
executor grant engineer create_ticket >/dev/null
executor revoke engineer create_ticket >/dev/null
result "two new honest rows are copied" "ok audit copy: 2 new rows," "$(auditcopy | cut -d' ' -f1-6)"
result "a copy with nothing new" "ok audit copy: 0 new rows," "$(auditcopy | cut -d' ' -f1-6)"

echo "== A rewritten row, with every hash after it recomputed"
sql="$(auditcopy forge 2 '{"forged":"by the superuser"}')"
result "the forged SQL rewrites row 2 onwards" true "$(grep -q 'WHERE seq = 2;' <<<"$sql" && echo true)"
forged=1
as_superuser <<<"$sql" >/dev/null
result "the VM's own check still passes (control: the attack is invisible inside)" 0 "$(verified)"
caught="$(auditcopy)"
result "the copy catches it" "FAIL row 2 in the VM differs from the copy: it was changed after it was copied" "${caught%% (copy*}"

echo "== Putting it back"
auditcopy restore | as_superuser >/dev/null
forged=0
result "the VM's chain verifies" 0 "$(verified)"
result "and matches the copy again" "ok audit copy: 0 new rows," "$(auditcopy | cut -d' ' -f1-6)"

if (( failed )); then echo "audit copy checks failed"; exit 1; fi
echo "all audit copy checks passed"
