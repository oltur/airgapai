# 0005: Baseline hardening of the air-gapped cluster

**Status:** accepted
**Date:** 2026-10-31

## Context

The offline install from [ADR 0004](0004-offline-install-and-black-hole-route.md) had no workload
restrictions. Phase 1 calls for Pod Security that holds on its own, default deny, cluster-only DNS,
and the CIS profile within a time box.

## Decision

All of this ships in the bundle, so every rebuild gets it:

- **Pod Security:** `restricted` for every namespace except `kube-system`, from its own config file
  rather than relying on the CIS profile's default.
- **Default deny:** the policy from [ADR 0003](0003-default-deny-with-calico-gnp.md), installed as an
  RKE2 manifest.
- **CoreDNS:** the chart's server list without the `forward` plugin.
- **CIS:** `profile: cis`, with the etcd user and sysctl prerequisites.
- **Disabled:** Traefik and metrics-server, as nothing uses them yet.
- **Test image:** a small static Go probe imported from the bundle, since the VM cannot pull one.

## Alternatives

- **Deferring CIS:** the plan's fallback at the budget cap, not needed so far.
- **Relying on the CIS profile for Pod Security:** deferring CIS would then remove it too.

## Consequences

Tested by `make test-hardening` on a fresh build, and again after a VM reboot. The test judges by how a
connection ended, and counts a pod as rejected only if the API server says it violates Pod Security.

| Connection from a new namespace | Policy on | Policy off (control) |
|---|---|---|
| API service, API on node, kubelet, peer pod in another namespace | Timeout | Reached |
| Host SSH | Timeout | Timeout, stopped by the host firewall |

| Check | Result |
|---|---|
| Cluster name, with the policy on | Resolves |
| Compliant pod in a new namespace | Admitted |
| `hostNetwork` or privileged pod in a new namespace | Rejected |
| The same `hostNetwork` pod in `kube-system` (control) | Admitted |
| Relabelling a namespace with an exempt name | The label stays the namespace's own name |
| SELinux | Enforcing, no denials in the audit log |

What this does not prove:

- **CIS conformance.** The profile is enabled, but no benchmark scan has run. This does not show the
  cluster meets the CIS benchmark.
- **The DNS path.** An external name fails with the policy on and off, because CoreDNS has no `forward`
  and the VM has no route. The DNS canary with an open path is still to come.
- **Public IP.** It times out with the policy on and off, so this test has no open-path control. The
  layer matrix has to provide one.
- **Admission of real pods.** Pod Security rejections use a server-side dry run.
- **`kube-system`.** Exempt from Pod Security and default deny, so anyone able to create pods there
  bypasses both. RBAC is not yet tested.
- **Policy removal.** The test removes the policy and it stays removed for at least 5 seconds. Nothing
  alerts on or restores a removed policy.
- **Image signing and the registry.** Not built yet.

`ausearch` reads stdin rather than the audit log when stdin is not a terminal, which can look like zero
denials or hang. The test reads the log file directly, and checks that the log holds other records.

## Measurements

Measured on 2026-10-31 on the NUC, in a VM limited to 8 GiB and 4 vCPUs, one run:

| What | Value |
|---|---|
| `make vm-up`, until all install jobs complete | 116 s |
| Bundle ISO | 814 MiB |
| Guest memory in use after a reboot and a test run, excluding cache | 1556 MiB |
| `/var/lib/rancher/rke2` | 3.7 GiB |
