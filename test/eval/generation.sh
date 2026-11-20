#!/usr/bin/env bash
# Runs inside the VM as root, for test/eval/generation.py on the host, which scores what this prints.
# Asks each golden question through the deployed gateway and the real model, as a technician or an
# engineer, and prints each chat page as it came back. QUESTIONS holds [{id, role, question}] as JSON
# in base64. test/lib/probe.sh, forgejo.sh, browser.sh and gateway-copy.sh are sent ahead.
#
# With CANDIDATE set, the questions go instead to a copy of the gateway, beside the deployed one:
# "deployed" for a copy with the deployed image, the control, or a tag of the gateway-candidate test
# image in the registry, such as "worse-prompt". For a tag, PROMPT_SHA256 is the hash of the prompt
# file the host scores against, and the image must carry it as its label, so an image built from an
# older prompt is refused. A copy left by an earlier run is removed first, and this one when it ends.
#
# Output, one record per line: "candidate IMAGE" first, if there is one, then
# "page ID STATUS SECONDS BASE64-PAGE" for each question, with status
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
forwards="" at="" port=18092
cleanup() {
  cli revoke engineer create_ticket >/dev/null
  # shellcheck disable=SC2086 # several PIDs
  kill $forwards 2>/dev/null
  [[ -z ${CANDIDATE:-} ]] || delete_gateway_copy gateway-candidate candidate
  rm -f /tmp/gateway-test-* /tmp/gateway-browser.py
}
trap cleanup EXIT
kubectl -n forgejo port-forward svc/forgejo 13000:3000 >/dev/null 2>&1 &
forwards="$!"
if [[ -n ${CANDIDATE:-} ]]; then
  image="" want="$(kubectl -n gateway get deployment gateway -o jsonpath='{.spec.template.spec.containers[0].image}')"
  if [[ $CANDIDATE != deployed ]]; then
    [[ $CANDIDATE =~ ^[a-z0-9-]+$ ]] || { echo "FAIL CANDIDATE is not a tag: $CANDIDATE"; exit 1; }
    digest="$(registry_digest gateway-candidate "$CANDIDATE")"
    [[ $digest == sha256:* ]] || { echo "FAIL no gateway-candidate:$CANDIDATE in the registry"; exit 1; }
    built="$(registry_label gateway-candidate "$CANDIDATE" airgapai.prompt-sha256)"
    [[ -n ${PROMPT_SHA256:-} && $built == "$PROMPT_SHA256" ]] ||
      { echo "FAIL gateway-candidate:$CANDIDATE was built from another prompt than the host's, ${built:-none}: rebuild"; exit 1; }
    image="10.43.0.50:5000/gateway-candidate@$digest" want="$image"
  fi
  delete_gateway_copy gateway-candidate candidate
  gateway_copy gateway-candidate candidate "$image" GATEWAY_URL=http://127.0.0.1:18093 || { echo "FAIL the candidate could not be made"; exit 1; }
  kubectl -n gateway rollout status deployment/gateway-candidate --timeout=120s >/dev/null || { echo "FAIL the candidate did not start"; exit 1; }
  running="$(kubectl -n gateway get deployment gateway-candidate -o jsonpath='{.spec.template.spec.containers[0].image}')"
  [[ $running == "$want" ]] || { echo "FAIL the candidate runs $running, not $want"; exit 1; }
  echo "candidate $running"
  forward_gateway_copy gateway-candidate 18093
  # shellcheck disable=SC2154 # copy_forward is set by test/lib/gateway-copy.sh, sent ahead
  forwards="$forwards $copy_forward" at=@candidate port=18093
else
  kubectl -n gateway port-forward svc/gateway 18092:8080 >/dev/null 2>&1 &
  forwards="$forwards $!"
fi
ready=0
for _ in $(seq 30); do
  curl -fs "http://127.0.0.1:$port/ready" >/dev/null && curl -fs http://127.0.0.1:13000/api/healthz >/dev/null && { ready=1; break; }
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
for name in alice bob; do
  user="$name$at"
  signed="$(browse "$user" signin /chat "${passwords[$name]}")"
  [[ $signed == "303 /chat" ]] || { echo "FAIL $user did not sign in: $signed"; exit 1; }
done
before="$(counts)"

while IFS=$'\t' read -r id role question; do
  name=alice
  [[ $role == engineer ]] && name=bob
  user="$name$at"
  # A session lasts an hour and a run may take longer: sign in again when the chat page is not given.
  if [[ $(browse "$user" get /chat) != 200 ]]; then
    signed="$(browse "$user" signin /chat "${passwords[$name]}")"
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
