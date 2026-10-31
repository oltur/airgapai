# 0006: Read-only registry and signed-image admission

**Status:** accepted
**Date:** 2026-10-31

## Context

Phase 1 calls for images by digest from an internal registry, a stable endpoint for containerd
without `hostNetwork`, and one minimal signed-image policy that fails closed outside named exempt
namespaces.

## Decision

- **Zot serves a read-only copy of a seed built on the online host.** The same Zot version writes the
  seed during `make bundle`, so the layout is its own. Inside the VM, anonymous users can only read,
  and nothing pushes.
- **Bootstrap order:** Zot loads into containerd from an archive in the bundle, as it cannot pull
  itself. Kyverno then pulls from Zot, and the policy goes on last.
- **Endpoint:** a Service with the fixed address `10.43.0.50`, reached by containerd through the
  service route from ADR 0004. No `hostNetwork` and no NodePort.
- **Signing:** a cosign key pair on the online host. Only the public key enters the bundle.
- **Policy:** one `ImageValidatingPolicy`, using the public key, ignoring the transparency log, and
  covering pods and `pods/ephemeralcontainers`. Digests are required and tags are refused.
- **Exempt namespaces:** `kube-system`, `kube-public` and `kube-node-lease` run RKE2's own images;
  `zot` cannot depend on itself; `kyverno` cannot verify itself. Only `kube-system` and `zot` are
  exempt from Pod Security, as Zot needs `hostPath`.
- **Guard webhook:** a copy of the policy's webhook, under a name Kyverno does not manage.

Testing forced four of these choices:

| Finding | Consequence |
|---|---|
| Kyverno deletes its own webhooks on a clean stop, so a pod was admitted with `failurePolicy: Fail` set and Kyverno scaled to zero | The guard webhook, which stays and fails closed |
| An unsigned ephemeral container was added to a running pod, as the rules covered only `pods` | The subresource is covered, and the expression checks every container kind |
| Matching only the internal registry admitted an outside image, as `required` and `images` see only matched images | The policy matches every image |
| Tags were refused for a while after a restart, as the mutating webhook returns after the guard | Tags are always refused, with no rewriting |

## Alternatives

- **Pushing into Zot inside the VM:** needs a job, credentials and a write path that the read-only
  design avoids.
- **NodePort for containerd:** opens a port on the node, where the service route needs none.
- **Converting Docker-format images to OCI:** would change their digests and break the pins. Zot's
  Docker compatibility is on, because Argo CD's image needs it ([ADR 0007](0007-git-ci-and-argo-inside-the-air-gap.md)).
- **Cosign's newer signature format:** not tried. Signing with no transparency log needs a separate
  signing config in that format, and the older format worked with Kyverno.

## Consequences

`make test-signed-images` passed on a fresh build and again after a VM reboot, and `make
test-hardening` passed in both states:

| Check | Result |
|---|---|
| Signed image by digest | Admitted, pulled from Zot and running |
| Same image by tag | Rejected by the policy |
| Unsigned image, and image signed by another key | Rejected by the policy |
| Unsigned init container, and unsigned ephemeral container | Rejected by the policy |
| Signed init container, and signed ephemeral container | Admitted |
| Unsigned image in an exempt namespace (control) | Admitted |
| Image from outside the registry | Refused, as it cannot be read |
| Registry read, and registry push | Read works (control); push refused |
| Signed image after a clean stop of Kyverno, and after a forced kill | Denied |
| After Kyverno returns | Signed admitted, unsigned rejected |

A rejection counts only with the API server's `denied the request: Policy require-signed-images
failed` message. An earlier version of the test matched the policy name alone, which also matched a
webhook timeout, because the webhook's URL contains that name.

What this does not prove:

- **Provenance.** Our signature attests only that this digest was mirrored here. `cosign verify` found
  no upstream signature for the Kyverno image at `reg.kyverno.io` or `ghcr.io`, or for the Zot image
  at `ghcr.io`, using each project's GitHub Actions identity. Those images are pinned by digest and
  fetched over HTTPS, and are not otherwise verified. The search may have missed signatures published
  elsewhere.
- **Exempt namespaces are unverified.** Anyone able to create pods in them bypasses this policy, and
  RBAC is untested. The Zot image is checksummed in the bundle but not signature-checked.
- **Policy and guard removal.** Someone able to delete them turns this off. Nothing alerts or restores.
- **Kyverno's certificate rotation is untested.** The guard holds a copy of the CA bundle, so a rotation
  would probably deny every pod in scope until the guard is regenerated. That fails closed, but it would
  be an outage.
- **Registry outage is untested.** The Go egress test will cover it. Workload pods also have no path to
  Zot yet, which that test's positive control expects, so this needs a decision there.
- **Push refusal** was tested with one anonymous upload request.

Known limits:

- **Plain HTTP to Zot.** Integrity rests on digests and signatures. The traffic stays inside the node,
  but it is not confidential or authenticated.
- **Kyverno tries to fetch an outside image before refusing it.** It tries to resolve the name, which is
  blocked: CoreDNS has no `forward`, and Kyverno's egress is limited to DNS, the API server and Zot.
  An image name can therefore cause a blocked lookup. The layer matrix has yet to test this path.
- **Deprecated flags.** Signing relies on `--tlog-upload=false`, `--sign-container-identity`,
  `--use-signing-config=false` and `--new-bundle-format=false`. A later cosign may remove them.
- **Signing key.** It sits unencrypted on the online host, protected by file mode only, with no backup.
  Losing it means new signatures and a new bundle.
- **After a Kyverno restart** pods can be denied for a short time. Admission recovered within 18 s of the
  rollout finishing in the runs seen. Across a VM reboot, a wait for rollout status can pass on stale
  status while pods are still starting.
- **Host memory.** On 2026-10-31 this VM, Docker and a 13.7 GB local model together ran the host out of
  memory, and the kernel killed the desktop session and the model. `make vm-up` now needs VM memory plus
  3 GiB available, which covers only the moment it starts.

## Measurements

Measured on 2026-10-31 on the NUC, in a VM limited to 8 GiB and 4 vCPUs, with Zot and Kyverno added:

| What | Value |
|---|---|
| `make vm-up`, until the policy and guard are on | 103, 118 and 133 s across three runs |
| `make bundle`, with downloads already cached | 61 s |
| Bundle ISO | 978 MiB |
| Guest memory in use after a reboot and a test run, excluding cache | 1975 MiB |
| `/var/lib/rancher/rke2` | 4.4 GiB |
| `/var/lib/airgapai/registry` | 90 MiB |
