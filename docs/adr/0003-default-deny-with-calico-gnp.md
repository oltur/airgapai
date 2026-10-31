# 0003: Cluster-wide default deny with a Calico GlobalNetworkPolicy

**Status:** accepted
**Date:** 2026-10-31

## Context

Kubernetes NetworkPolicy is per namespace, so a new namespace is open until someone adds a policy.
The air gap needs a default that also covers namespaces that do not exist yet. RKE2's default CNI,
Canal, uses Calico for policy, and its GlobalNetworkPolicy CRD is present. Whether this would work
through the CRD alone, without the Calico API server or `calicoctl`, was an open question.

## Decision

Keep Canal. Apply [`spike/default-deny/policy.yaml`](../../spike/default-deny/policy.yaml): a
GlobalNetworkPolicy covering every namespace except `kube-system`, `kube-public` and
`kube-node-lease`, selected on `kubernetes.io/metadata.name`, allowing only DNS to CoreDNS.

## Alternatives

- **Switching CNI:** not needed, as the spike passed.
- **A NetworkPolicy per namespace:** leaves a new namespace open until one is added.

## Consequences

Tested on a throwaway single-node VM with internet access, using
[`spike/default-deny/test.sh`](../../spike/default-deny/test.sh): AlmaLinux 9.8, RKE2
v1.36.5+rke2r1, SELinux enforcing. Two consecutive runs passed.

| Result | Evidence |
|---|---|
| Blocks a namespace that existed before the policy | Public IP, HTTPS by name, API server and kubelet go from reached to timeout |
| Covers a new namespace with no labels added | The same targets time out on the namespace's first pod |
| Blocks pod to pod across namespaces | Timeout with the policy on, refused with it off |
| Leaves DNS working | `nslookup` through CoreDNS succeeds with the policy on |
| The policy is the cause | Public IP and pod-to-pod traffic recover when it is removed |

What this does not prove:

- **Gateway and metadata address:** both also timed out with the policy off, so these checks had
  no working control. The Phase 1 egress test needs a target that answers when the path is open.
- **External DNS:** names still resolve under the policy, because CoreDNS still forwards. Removing
  `forward` is a separate Phase 1 step, as is the DNS canary.
- **Exempt namespaces:** a pod in `kube-system` reached a public IP with the policy on. This layer
  does not contain them, so the VM layer has to.
- **Who can create what:** anyone able to create pods in an exempt namespace bypasses this policy.
  RBAC and admission have to restrict that.
- **`hostNetwork` pods and IPv6:** not tested here. Both are in the Phase 1 egress test.
- **Time to enforce:** the test waits 5 seconds after applying the policy, and only until a pod is
  Ready in a new namespace. It does not measure any window before enforcement.
- **Air gap:** the spike ran with online installs and image pulls.

Applying the policy through `crd.projectcalico.org/v1` directly is a shortcut, since Calico
recommends its own API for this. It worked on this version, and an RKE2 upgrade should re-run this
test.

## Measurements

Measured on 2026-10-31 on the NUC, in a VM limited to 6 GiB and 4 vCPUs, with the idle cluster and
no workloads beyond the test:

| What | Value |
|---|---|
| Guest memory in use, excluding cache | 1827 MiB |
| `/var/lib/rancher/rke2` | 3.5 GiB |

These are one reading on a fresh node, not a sizing result for the full stack.
