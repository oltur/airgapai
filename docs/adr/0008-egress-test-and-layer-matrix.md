# 0008: Egress test and layer matrix

**Status:** accepted
**Date:** 2026-11-01

## Context

Phase 1 is done when `make rebuild` passes from the bundle, the layer matrix matches, and every control
behaves as expected. A blocked connection proves little alone: it would also be blocked by a broken
test or a dead listener. So each block needs a case where the same attempt succeeds.

## Decision

- **The test runs on the host, in Go** (`test/egress`), because it must change the host firewall and
  the VM's routes from outside the cluster.
- **The canary sits on temporary host addresses:** a dummy interface holding `198.51.100.1` (a
  documentation range) and the metadata address. "VM path open" means a firewall rule and a VM route to
  those addresses only. No step opens a path to the internet.
- **Everything opened is closed** on exit, on a signal and at the start of the next run, and the test
  ends by checking the firewall, routes and default deny. It refuses to start if the host firewall table
  is missing or differs from the repository.
- **Pods have no standing path to Zot.** Containerd pulls from the node, and no workload needs the
  registry. The plan's positive control uses an allow rule created during the test for one namespace,
  then removed, so it adds no permanent hole.
- **Host listeners are discovered with `ss`,** so a new host service is covered without editing the test.

## Consequences

`make rebuild` passed: 185 checks across the four test suites, of which `test-egress` is the last.
A rerun of `test-egress` on the previous build also passed.

| | Policy on | Policy off |
|---|---|---|
| **VM path closed** | Blocked | Blocked |
| **VM path open** | Blocked | **Reached**, and logged by the canary |

The cells hold for TCP to the canary, TCP to the metadata address and a direct DNS query. Other results:

| Check | Result |
|---|---|
| `egress-new` and `workloads` to public IPs, public IPv6, HTTPS by name, external DNS | Blocked |
| The same pods to gateway, metadata, host listeners, node and API service | Blocked |
| Online host to the same public IPv4 targets, before and after (control) | Reached |
| Cluster DNS (positive control) | Resolves |
| API, kubelet and API service with the policy off (control) | Reached |
| VM and `kube-system` pod to public IP, path closed | Blocked |
| VM and `kube-system` pod to the canary, path open (control) | Reached |
| `forgejo`, `argocd`, `kyverno`, `zot` pods to the canary, path open, policy on | Blocked |
| Host listener on the bridge, from pod and VM, firewall in place | Blocked |
| The same listener, firewall opened (control) | Reached |
| Random DNS label through cluster DNS, path open, no forwarder | Zero logged |
| The same with a forwarder added to CoreDNS (control) | Logged |
| The same with the forwarder and the path closed | Zero logged |
| `hostNetwork` pod; unsigned image; signed pod as control | Rejected; rejected; admitted |
| Pod to Zot: no rule; rule for its namespace; other namespace; rule removed | Blocked; reached; blocked; blocked |
| Registry down: running pod; container restart from cache; new unsigned pod | Continues; works; denied |
| Host firewall boot service, after a real host reboot with the VM off | Loaded the table unaided |

Controls found two faults that a block alone would have hidden:

| Finding | Consequence |
|---|---|
| The forwarder control logged nothing: the Corefile says `cache  30` with two spaces, so the test's edit did nothing | The edit matches any spacing, and the test stops if no forwarder was added |
| `signed-images.sh` accepted the `Running` phase, and its pod's `sleep` argument made the probe exit: a crash-looping pod would have passed | The pod now serves, and the check requires it to be ready |

What this does not prove:

- **The canary is on the host, not on a remote network.** The test shows the path to one address opens
  and closes. It relies on the host forward chain and on the VM's black-hole route for everything else.
  Public targets are blocked, but no open-path control exists for them, by design.
- **The gateway has no open-path control.** It gave `no-route` with the policy off, so its block is not
  shown to come from the policy.
- **IPv6 has no open-path control.** The VM has link-local IPv6 only, and no global address. Pods and the
  VM failed to reach a public IPv6 address. The host bridge had no link-local address, so that check was
  not applicable and is reported as such. The host's own IPv6 connectivity was not tested as a control.
- **Forgejo, Argo CD, Kyverno and Zot were tested with an added probe pod,** not their own pods. The
  labels of the real pods would make their Deployments and Services adopt the probe. Argo's and Kyverno's
  rules apply to the whole namespace. Forgejo's runner and server rules were read, not exercised.
- **Other protocols, such as ICMP, were not tested.** The matrix covers TCP and DNS over UDP.
- **Host listeners are read at test time,** from the listening sockets of that moment.

Known limits:

- **A registry outage stops new pods, signed ones too.** The verifier needs the registry to check a
  signature, so a pod rescheduled during an outage would not start. This fails closed, and it is an
  availability cost.
- **Not closed by this test:** plain HTTP to Zot and Forgejo, policies outside Argo CD with no alert on
  removal, Secrets at rest, no kube-bench scan, and the limits listed in ADRs 0006 and 0007.
- **The bundle build is online,** by design. Only the VM's install and everything after it are offline.

## Measurements

Measured on 2026-11-01 on the NUC, in a VM limited to 8 GiB and 4 vCPUs, from one `make rebuild` with
downloads cached. The memory and disk figures were read straight after its tests.

| What | Value |
|---|---|
| Destroy, bundle and VM up | 225 s |
| All four test suites | 572 s |
| Bundle ISO | 1321 MiB |
| VM disk file | 6.8 GiB |
| Guest memory in use, excluding cache | 3296 MiB |
| `/var/lib/rancher/rke2` | 5.6 GiB |
| `/var/lib/airgapai/registry` | 431 MiB |
| `/var/lib/airgapai/forgejo` | 11 MiB |
