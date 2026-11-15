#!/usr/bin/env bash
# Runs on the host. Tests the off-VM copy of the gateway's audit log (ADR 0024): it copies the VM's
# rows, copies new honest rows, and then catches a row that the database superuser rewrote with every
# hash after it recomputed, a change the VM's own check cannot see. The rewritten rows are put back
# from the copy, and the test checks the VM's log matches it again. Copies go to a temporary directory,
# not to the lab's own copy under DATA_DIR.
#
# The rewrite and the restore are done by infra/auditcopy/forge, a separate program, so the copier
# itself only reads. Both hold the audit lock and refuse if the log changed after they read it.
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
failed=0
result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; failed=1; fi
}
dir="$(mktemp -d)"
forged=0
cleanup() {
  # If the test stopped with the log rewritten, put it back from the copy before removing the copy.
  if (( forged )) && ! "$dir/forge" -root "$root" -dir "$dir" restore; then
    echo "FAIL the audit log could not be restored; the copy is kept in $dir"
    return
  fi
  rm -rf "$dir"
}
trap cleanup EXIT
# Built once, so no build output mixes with what the tests read, and the exit trap needs no toolchain.
go -C "$root/infra/auditcopy" build -o "$dir/auditcopy" . && go -C "$root/infra/auditcopy" build -o "$dir/forge" ./forge \
  || { echo "FAIL could not build the copier"; exit 1; }
copy() { "$dir/auditcopy" -root "$root" -dir "$dir" 2>&1; }
forge() { "$dir/forge" -root "$root" -dir "$dir" "$@"; }
k8s='sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml'
executor() { "$root/infra/vm-ssh.sh" "$k8s -n gateway exec deployment/executor -- /executor $*" 2>&1; }
verified() { executor verify | python3 -c 'import json,sys; print(json.load(sys.stdin)["first_bad_row"])'; }

echo "== Copying"
first="$(copy)"
result "the first copy" ok "$(cut -d' ' -f1 <<<"$first")"
echo "info $first"
# Two new rows from a grant that is already in place, so the lab's permissions stay as they were.
executor grant technician create_ticket >/dev/null
executor grant technician create_ticket >/dev/null
result "two new honest rows are copied" "ok audit copy: 2 new rows," "$(copy | cut -d' ' -f1-6)"
result "a copy with nothing new" "ok audit copy: 0 new rows," "$(copy | cut -d' ' -f1-6)"

echo "== A rewritten row, with every hash after it recomputed"
forged=1
result "row 2 rewritten as the superuser" "" "$(forge rewrite 2 '{"forged":"by the superuser"}' 2>&1)"
result "the VM's own check still passes (control: the attack is invisible inside)" 0 "$(verified)"
caught="$(copy)"
result "the copy catches it" "FAIL row 2 in the VM differs from the copy: it was changed after it was copied" "${caught%% (copy*}"

# A row appended while the log is rewritten chains to the forged hash. Restoring must re-link it.
executor grant technician create_ticket >/dev/null

echo "== Putting it back"
if forge restore; then forged=0; fi
result "restored" 0 "$forged"
result "the VM's chain verifies" 0 "$(verified)"
result "and matches the copy again, with the row added meanwhile as new" "ok audit copy: 1 new rows," "$(copy | cut -d' ' -f1-6)"

if (( failed )); then echo "audit copy checks failed"; exit 1; fi
echo "all audit copy checks passed"
