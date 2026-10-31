# 0007: Git, CI and Argo CD inside the air gap

**Status:** accepted
**Date:** 2026-10-31

## Context

Phase 1 needs Forgejo, an Argo CD app of apps and a CI job running `make check`, loaded after the
registry and verifier, with no path out.

## Decision

- **Forgejo** uses a local PersistentVolume, not `hostPath`, so its namespace keeps the restricted Pod
  Security level. SSH, sign-up and outbound features are off.
- **Repository:** `make bundle` commits the working tree to a temporary ref without moving `HEAD`,
  scans it with the guard, and bundles it. Only what Git would track goes in. Bootstrap pushes it to
  Forgejo from a mounted ConfigMap.
- **CI** uses a runner image built on the host: the upstream runner plus `bash`, `git`, `grep`, `make`
  and `gitleaks`. Jobs run inside the runner's container, as restricted Pod Security rules out nested
  containers. The job clones the repository and runs `DENYLIST_OPTIONAL=1 make check`.
- **Argo CD** uses the core install, with no API server, UI or Dex. A root application reads
  `deploy/apps`, with automated sync, prune and self-heal. The `default` project takes manifests only
  from the internal repository.
- **Security policies stay out of Argo CD for now.** The hardening test removes the default deny as its
  control, and Argo CD would restore it during the test.

Testing changed two choices:

| Finding | Consequence |
|---|---|
| Copying the bundle through `kubectl exec -i` truncated it in one of three tries, to 32768 of 66279 bytes | It travels as a ConfigMap |
| A hand-written runner registration file was rejected | The runner writes its own, from a shared secret |

The core install leaves out what creates the `default` project, so bootstrap defines it.

## Alternatives

- **`hostPath` for Forgejo:** would exempt another namespace from Pod Security.
- **Nested containers for CI:** need privileges that restricted Pod Security refuses.
- **Argo CD's full install:** adds a UI and API that nothing here needs, and more memory.

## Consequences

`make test-gitops` passed on a fresh build and on a rebooted VM. `make test-signed-images` and `make
test-hardening` passed in both states with these services running.

| Check | Result |
|---|---|
| Root and child applications | Synced and healthy |
| Commit through Forgejo's API | Reached the cluster; the value differed before it (control) |
| Manual change to a deployed object | Reverted |
| Clean commit in CI (control) | `make check` passes |
| Commit with a generated fake key, on a branch | `make check` fails, and a job log reports the leak |
| Forgejo to a public IP | Fails |
| `hostNetwork` pod in the `forgejo` namespace | Rejected by Pod Security |
| Git data across a VM reboot | Kept |

What this does not prove:

- **CI runs the gitleaks part only.** The VM has no denylist, so its terms are checked only on the
  online host: by the local hooks, and by the guard when the bundle is built.
- **The leak-log check reads all job logs.** It shows a leak was reported in some run, and the failing
  status belongs to the control commit. It does not tie the two together.
- **Forgejo's outbound block has no open-path control,** and the runner's and Argo CD's were not tested
  separately. The Go egress test covers these.
- **Argo CD's source restriction is untested** against a repository outside the air gap.
- **Delivery is not authenticated beyond the network policy.** The repository is public inside the
  cluster, over plain HTTP. Network policy limits who reaches it to Argo CD and the runner. Anyone who
  can push to it controls the cluster, because Argo CD can deploy any kind of resource.
- **Argo CD's manifests are not signature-checked.** Images are, by the policy. Git commits are not.

Known limits:

- **Secrets.** Forgejo's admin password and token sit in a Kubernetes Secret, and RKE2's secret
  encryption at rest was not examined here. The runner's shared secret is also in one.
- **`create-runner-file` is deprecated** upstream, so a later runner may remove it.
- **CI image.** `apk` resolves packages at build time, so its digest changes with each build, and those
  packages are not pinned. The result is signed and pinned per bundle only.
- **Bundle size.** The repository must fit a ConfigMap, about 1 MiB. It is now 66 KiB, and a larger one
  fails bootstrap visibly. The bundled history is the repository's full history.
- **Bootstrap runs once.** A reboot does not rerun it, and the test needs Forgejo running first: right
  after a reboot, `make test-gitops` failed with the pod not ready and passed once it was.
- **Argo CD's and Forgejo's versions are the latest at build time,** not chosen for long-term support.

## Measurements

Measured on 2026-10-31 on the NUC, in a VM limited to 8 GiB and 4 vCPUs, with these services added:

| What | Value |
|---|---|
| `make vm-up`, until the root application is created | 164 and 169 s across two runs |
| `make bundle`, with downloads cached | 70 and 72 s across two runs |
| Bundle ISO | 1320 MiB |
| Guest memory in use, after a reboot and test runs, excluding cache | 3067 MiB |
| `/var/lib/rancher/rke2` | 5.6 GiB |
| `/var/lib/airgapai/registry` | 431 MiB |
| `/var/lib/airgapai/forgejo`, after test runs | 6.6 MiB |
