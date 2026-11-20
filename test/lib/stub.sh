# shellcheck shell=bash
# Shared by the in-VM gateway tests: the compromised-model stub. start_stub runs it in the inference
# namespace, with a copy of the deployed gateway that uses it as its model, and two temporary rules
# that let the gateway's label reach it. The copy, made by test/lib/gateway-copy.sh, changes only its
# model and its own address; its image is the deployed gateway's. stop_stub removes all of it. See
# ADR 0019. Sent ahead of each test script, like probe.sh, after gateway-copy.sh.

stub_forward=""
start_stub() {
  local digest
  digest="$(registry_digest model-stub v1)"
  [[ $digest == sha256:* ]] || { echo "FAIL could not read the stub's digest"; return 1; }
  kubectl apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: model-stub, namespace: inference}
spec:
  replicas: 1
  selector: {matchLabels: {app: chat-stub}}
  template:
    metadata: {labels: {app: chat-stub}}
    spec:
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: stub
          image: 10.43.0.50:5000/model-stub@$digest
          ports: [{name: http, containerPort: 8080}]
          readinessProbe: {httpGet: {path: /health, port: http}, periodSeconds: 2}
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
---
apiVersion: v1
kind: Service
metadata: {name: model-stub, namespace: inference}
spec:
  selector: {app: chat-stub}
  ports: [{name: http, port: 8080, targetPort: http}]
---
apiVersion: crd.projectcalico.org/v1
kind: GlobalNetworkPolicy
metadata: {name: test-gateway-egress-to-stub}
spec:
  selector: app == "gateway"
  namespaceSelector: kubernetes.io/metadata.name == "gateway"
  types: [Egress]
  egress:
    - {action: Allow, protocol: TCP, destination: {namespaceSelector: kubernetes.io/metadata.name == "inference", selector: app == "chat-stub", ports: [8080]}}
---
apiVersion: crd.projectcalico.org/v1
kind: GlobalNetworkPolicy
metadata: {name: test-stub-reachable-from-gateway}
spec:
  selector: app == "chat-stub"
  namespaceSelector: kubernetes.io/metadata.name == "inference"
  types: [Ingress]
  ingress:
    - {action: Allow, protocol: TCP, source: {namespaceSelector: kubernetes.io/metadata.name == "gateway", selector: app == "gateway"}, destination: {ports: [8080]}}
YAML
  gateway_copy gateway-stub stub-test "" CHAT_URL=http://model-stub.inference.svc:8080 GATEWAY_URL=http://127.0.0.1:18093
  kubectl -n inference rollout status deployment/model-stub --timeout=120s >/dev/null &&
    kubectl -n gateway rollout status deployment/gateway-stub --timeout=120s >/dev/null || { echo "FAIL the stub did not start"; return 1; }
  kubectl -n gateway port-forward deployment/gateway-stub 18093:8080 >/dev/null 2>&1 &
  stub_forward=$!
  for _ in $(seq 30); do curl -fs http://127.0.0.1:18093/ready >/dev/null && return 0; sleep 1; done
  echo "FAIL the stub's gateway is not ready"; return 1
}

stop_stub() {
  [[ -z $stub_forward ]] || kill "$stub_forward" 2>/dev/null
  stub_forward=""
  delete_gateway_copy gateway-stub stub-test
  kubectl -n inference delete deployment,service model-stub --ignore-not-found --timeout=120s >/dev/null 2>&1
  kubectl delete globalnetworkpolicy.crd.projectcalico.org test-gateway-egress-to-stub test-stub-reachable-from-gateway \
    --ignore-not-found >/dev/null 2>&1
}
