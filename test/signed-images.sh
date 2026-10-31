#!/usr/bin/env bash
# Runs inside the VM as root. Tests signed-image admission and the registry, with controls.
set -uo pipefail

kubectl=(/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml)
registry=10.43.0.50:5000
denied='denied the request: Policy require-signed-images failed'
# Kyverno tries to fetch every image, and refuses one it cannot read. No registry outside is reachable.
unreachable='denied the request: Policy require-signed-images error: failed to evaluate policy'
failed=0
ns=test-signed
last_output="$(mktemp)"
trap 'rm -f "$last_output"' EXIT

result() { # name expected actual
  if [[ $2 == "$3" ]]; then echo "ok   $1: $3"; else echo "FAIL $1: expected $2, got $3"; cat "$last_output"; failed=1; fi
}

pod() { # name image namespace [dry-run flag] [init image]
  local init=""
  [[ -n ${5:-} ]] && init="  initContainers:
    - name: init
      image: $5
      args: [lookup, kubernetes.default.svc.cluster.local]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
"
  cat <<YAML | "${kubectl[@]}" apply ${4:-} -f - 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $3
spec:
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile:
      type: RuntimeDefault
${init}  containers:
    - name: probe
      image: $2
      args: [sleep]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
YAML
}

# A rejection counts only when it comes from this policy, not from a mistake in the test.
admission() { # name image namespace [init image]
  local out
  out="$(pod "$1" "$2" "$3" --dry-run=server "${4:-}")"
  echo "$out" >"$last_output"
  if grep -q "$denied" <<<"$out"; then echo rejected-by-policy; echo "info   $1: ${out##*failed: }" >&2
  elif grep -q "$unreachable" <<<"$out"; then echo rejected-unreachable
  elif grep -qE 'created|configured' <<<"$out"; then echo admitted
  else echo "other: $out"
  fi
}

"${kubectl[@]}" delete namespace "$ns" --ignore-not-found --wait=true >/dev/null
"${kubectl[@]}" create namespace "$ns" >/dev/null

digest() { curl -sI -H 'Accept: application/vnd.oci.image.manifest.v1+json' "http://$registry/v2/$1/manifests/v1" | awk 'tolower($1) == "docker-content-digest:" {print $2}' | tr -d '\r'; }
signed="$registry/probe@$(digest probe)"
unsigned="$registry/probe-unsigned@$(digest probe-unsigned)"
other_key="$registry/probe-other-key@$(digest probe-other-key)"
[[ $signed == *sha256:* && $unsigned == *sha256:* && $other_key == *sha256:* ]] || { echo "FAIL could not read digests"; exit 1; }

result "signed image by digest" admitted "$(admission signed "$signed" "$ns")"
result "same signed image by tag" rejected-by-policy "$(admission by-tag "$registry/probe:v1" "$ns")"
result "unsigned image" rejected-by-policy "$(admission unsigned "$unsigned" "$ns")"
result "image signed by another key" rejected-by-policy "$(admission other-key "$other_key" "$ns")"
result "image from outside the registry" rejected-unreachable "$(admission outside docker.io/library/busybox@sha256:0000000000000000000000000000000000000000000000000000000000000000 "$ns")"
result "signed init container" admitted "$(admission signed-init "$signed" "$ns" "$signed")"
result "unsigned init container" rejected-by-policy "$(admission unsigned-init "$signed" "$ns" "$unsigned")"
# Control: the same unsigned image passes where the policy is exempt, so the rejection above is the policy's.
result "unsigned image in exempt namespace" admitted "$(admission unsigned "$unsigned" kube-system)"

# The signed image really pulls from Zot and runs.
pod signed-run "$signed" "$ns" >"$last_output"
"${kubectl[@]}" -n "$ns" wait --for=condition=Ready pod/signed-run --timeout=120s >/dev/null 2>&1
result "signed image runs" Running "$("${kubectl[@]}" -n "$ns" get pod signed-run -o jsonpath='{.status.phase}')"

# Ephemeral containers reach a running pod through a subresource of their own.
ephemeral() { # name image
  local out
  out="$("${kubectl[@]}" -n "$ns" patch pod signed-run --subresource=ephemeralcontainers --type=strategic -p "{\"spec\":{\"ephemeralContainers\":[{\"name\":\"$1\",\"image\":\"$2\",\"args\":[\"sleep\"],\"securityContext\":{\"allowPrivilegeEscalation\":false,\"capabilities\":{\"drop\":[\"ALL\"]},\"runAsNonRoot\":true,\"seccompProfile\":{\"type\":\"RuntimeDefault\"}}}]}}" 2>&1)"
  echo "$out" >"$last_output"
  if grep -q "$denied" <<<"$out"; then echo rejected-by-policy
  elif grep -q patched <<<"$out"; then echo admitted
  else echo "other: $out"
  fi
}
result "unsigned ephemeral container" rejected-by-policy "$(ephemeral bad "$unsigned")"
result "signed ephemeral container" admitted "$(ephemeral good "$signed")"

# Reads work, so a refused write is the access policy and not an unreachable registry.
result "registry read" 200 "$(curl -s -o /dev/null -w '%{http_code}' "http://$registry/v2/probe/tags/list")"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://$registry/v2/probe/blobs/uploads/")"
[[ $code == 401 || $code == 403 ]] && code=refused
result "registry push" refused "$code"

# Fail closed: with the verifier down, even a signed image is not admitted.
while_down() { # name
  local out
  out="$(pod "$1" "$signed" "$ns" --dry-run=server)"
  echo "$out" >"$last_output"
  if grep -qE 'failed calling webhook|no endpoints|connection refused' <<<"$out"; then echo denied-webhook-down
  elif grep -qE 'created|configured' <<<"$out"; then echo admitted
  else echo "other: $out"
  fi
}
restore() {
  "${kubectl[@]}" -n kyverno scale deployment kyverno-admission-controller --replicas=1 >/dev/null
  "${kubectl[@]}" -n kyverno rollout status deployment/kyverno-admission-controller --timeout=180s >/dev/null
}
# After a restart the webhook times out for a while, which is still a denial. Wait with a limit, and report it.
recovery() { # name
  local start=$SECONDS state
  while (( SECONDS - start < 180 )); do
    state="$(admission signed "$signed" "$ns" 2>/dev/null)"
    [[ $state == admitted ]] && { echo "info verifier answering $((SECONDS - start)) s after rollout" >&2; break; }
    sleep 3
  done
  echo "$state"
}
selector=app.kubernetes.io/component=admission-controller

# A clean stop, in which Kyverno deletes its own webhooks.
"${kubectl[@]}" -n kyverno scale deployment kyverno-admission-controller --replicas=0 >/dev/null
"${kubectl[@]}" -n kyverno wait --for=delete pod -l "$selector" --timeout=120s >/dev/null 2>&1
kyverno_webhook="$("${kubectl[@]}" get validatingwebhookconfiguration kyverno-resource-validating-webhook-cfg -o name 2>/dev/null)"
echo "info Kyverno's own webhook after a clean stop: ${kyverno_webhook:-deleted}"
result "signed image after a clean stop" denied-webhook-down "$(while_down clean-stop)"
restore
result "signed image after the verifier returns" admitted "$(recovery)"
result "unsigned image after the verifier returns" rejected-by-policy "$(admission unsigned "$unsigned" "$ns")"

# A crash: no chance to clean up. The replica count drops first so that nothing replaces the pod.
"${kubectl[@]}" -n kyverno scale deployment kyverno-admission-controller --replicas=0 >/dev/null
"${kubectl[@]}" -n kyverno delete pod -l "$selector" --grace-period=0 --force >/dev/null 2>&1
"${kubectl[@]}" -n kyverno wait --for=delete pod -l "$selector" --timeout=60s >/dev/null 2>&1
result "signed image after a forced kill" denied-webhook-down "$(while_down forced-kill)"
restore
result "signed image after a second return" admitted "$(recovery)"
result "unsigned image after a second return" rejected-by-policy "$(admission unsigned "$unsigned" "$ns")"

"${kubectl[@]}" delete namespace "$ns" --wait=false >/dev/null
[[ $failed == 0 ]] && echo PASS || { echo FAILED; exit 1; }
