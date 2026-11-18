#!/usr/bin/env bash
# Runs inside the VM as root, for test/eval/generation.py on the host, which scores what this prints.
# Asks each golden question through the deployed gateway and the real model, as a technician or an
# engineer, and prints each chat page as it came back. QUESTIONS holds [{id, role, question}] as JSON
# in base64. test/lib/probe.sh, forgejo.sh and browser.sh are sent ahead.
#
# Output, one record per line: "page ID STATUS SECONDS BASE64-PAGE" for each question, with status
# 000 and page "-" for a turn that gave no answer at all, then
# "counts BEFORE-JSON AFTER-JSON" for the issues filed and the confirmations recorded, and "chain N"
# for the audit chain's first bad row, 0 if none. Anything else is progress, for a reader.
set -uo pipefail

export KUBECONFIG=/etc/rancher/rke2/rke2.yaml PATH=/var/lib/rancher/rke2/bin:$PATH
[[ -n ${QUESTIONS:-} ]] || { echo "FAIL QUESTIONS is not set"; exit 1; }
kubectl -n gateway rollout status deployment/gateway --timeout=300s >/dev/null || { echo "FAIL the gateway is not running"; exit 1; }
cli() { kubectl -n gateway exec deployment/executor -- /executor "$@" 2>&1; }

# Both roles may propose, so that a proposal nobody asked for can happen, and be seen, for either.
# Engineers have no grant otherwise, and test/gateway.sh checks they cannot propose, so the cleanup
# revokes theirs.
test_user alice technicians
test_user bob engineers
cli grant technician create_ticket >/dev/null
cli grant engineer create_ticket >/dev/null
rm -f /tmp/gateway-test-*
forwards=""
for pair in forgejo/forgejo:13000:3000 gateway/gateway:18092:8080; do
  IFS=: read -r target local remote <<<"$pair"
  kubectl -n "${target%/*}" port-forward "svc/${target#*/}" "$local:$remote" >/dev/null 2>&1 &
  forwards="$forwards $!"
done
cleanup() {
  cli revoke engineer create_ticket >/dev/null
  # shellcheck disable=SC2086 # several PIDs
  kill $forwards 2>/dev/null
  rm -f /tmp/gateway-test-* /tmp/gateway-browser.py
}
trap cleanup EXIT
ready=0
for _ in $(seq 30); do
  curl -fs http://127.0.0.1:18092/ready >/dev/null && curl -fs http://127.0.0.1:13000/api/healthz >/dev/null && { ready=1; break; }
  sleep 1
done
(( ready )) || { echo "FAIL the gateway or Forgejo port-forward is not ready"; exit 1; }

# The issues in both repositories and the confirmations, as JSON. A count that could not be read is
# printed as it came, and the host refuses to compare it.
counts() {
  printf '{"tickets":"%s","restricted":"%s","confirmed":"%s"}' \
    "$(issues tickets)" "$(issues tickets-restricted)" "$(decisions confirm allowed)"
}

# shellcheck disable=SC2154 # passwords is set by test/lib/forgejo.sh, sent ahead
for user in alice bob; do
  signed="$(browse "$user" signin /chat "${passwords[$user]}")"
  [[ $signed == "303 /chat" ]] || { echo "FAIL $user did not sign in: $signed"; exit 1; }
done
before="$(counts)"

while IFS=$'\t' read -r id role question; do
  user=alice
  [[ $role == engineer ]] && user=bob
  # A session lasts an hour and a run may take longer: sign in again when the chat page is not given.
  if [[ $(browse "$user" get /chat) != 200 ]]; then
    signed="$(browse "$user" signin /chat "${passwords[$user]}")"
    [[ $signed == "303 /chat" ]] || { echo "FAIL $user did not sign in again: $signed"; exit 1; }
    browse "$user" get /chat >/dev/null
  fi
  started=$SECONDS
  status="$(browse "$user" chat "$question")"
  status="${status%% *}"
  # A turn that timed out prints no status, and leaves the chat page from before it: send no page.
  if [[ $status =~ ^[0-9]{3}$ ]]; then page="$(base64 -w0 "/tmp/gateway-test-$user.html")"; else status=000 page=-; fi
  echo "page $id $status $((SECONDS - started)) $page"
done < <(python3 -I -c 'import base64, json, os
for q in json.loads(base64.b64decode(os.environ["QUESTIONS"])):
    print(q["id"], q["role"], " ".join(q["question"].split()), sep="\t")')

echo "counts $before $(counts)"
echo "chain $(json "d['first_bad_row']" <<<"$(cli verify)")"
