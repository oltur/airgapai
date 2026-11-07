# 0011: Model images and serving in the cluster

**Status:** accepted
**Date:** 2026-11-04

## Context

ADR 0010 chose the models and measured them on the host. Phase 2 serves them inside the air gap:
images by digest from the read-only registry, signed with the lab key, and deployed from Git by
Argo CD. Two constraints shaped the choice of how:

- **The signature policy from ADR 0006 checks containers, init containers and ephemeral
  containers.** It does not check images mounted as volumes.
- **Argo CD deploys what Git names.** An image whose digest changes on every build, like the CI
  image, cannot be named in Git.

## Decision

- **Each model is an image holding one GGUF file and `model-copy`,** a small static Go program, on an
  empty base. Owners, modes and times are fixed, and the copier is built with the exact Go toolchain its
  `go.mod` names, so the digest depends only on those inputs: two builds of the same file gave the same
  digest. `infra/versions.env` pins each digest, and the bundle build
  fails if a build does not produce it, or if the images `deploy/inference` names are not exactly the
  pinned ones.
- **An init container runs the model image and copies the file into an `emptyDir`,** which the server
  reads. The policy checks the init container's image like any other.
- **The server is the official llama.cpp image, build `b12030`, pinned by its amd64 digest** and
  copied into the bundle like the other outside images. The project publishes no image for `b12032`,
  the build measured in ADR 0010.
- **One Deployment per model in the `inference` namespace,** deployed by Argo CD from `deploy/inference`.
  Settings match the host runs: 4 threads, one request at a time, context of 4096 tokens for chat and
  2048 for embeddings. Pods run as user 65534 with a read-only root, no service account token, and
  memory limits of 5000 MiB for chat and 1000 MiB for embeddings.
- **No network path to the servers yet.** The default deny applies. The retrieval service gets its own
  allow rule when it exists.
- **The VM limit from ADR 0010 stays at 10240 MiB,** on the measurement below.

## Alternatives

- **Mount each model image as a volume.** The cluster supports it and it avoids the copy, but the
  signature policy does not check volume images, so an unsigned model would be admitted. Extending
  the policy would be new admission logic, needing its own tests with working controls.
- **Build the server image here from the `b12032` release.** That keeps the measured build, but a base
  image and its packages resolve at build time, so the digest would change on every build, as the CI
  image's does, and Git could not name it.
- **Put the model in the server image.** Then a model change rebuilds the server image, and a server
  change copies 2 GB again.

## Consequences

- **Each pod start copies its model.** It took 2.3 and 2.4 s for the 2 GB file. The copy uses as much disk again,
  and its pages count against the pod's memory.
- **A model or copier change means a new digest** in `infra/versions.env` and in `deploy/inference`. The
  bundle build names the mismatch.
- **The bundle grows:** `du` reported 3.8 GB for the ISO.
- **The two models slow each other badly when they run at once.** Across two runs, generation fell
  from 14.4 and 14.9 to 8.2 and 11.0 tokens/s, and embedding from 1.92 and 2.26 to 0.63 and 0.76 chunks
  per second. Both use 4 threads on 4 vCPUs. Ingestion and answering should not overlap, or the thread
  counts need to change, which would need measuring.
- **Speed varies between runs by more than the gap to the host.** Embedding speed differed by 18%
  between the two runs, so a single run cannot show a small change.

### What this does not prove

- **The signature check on model images is not tested separately.** The model pods were admitted, which
  shows signed images pass. That an unsigned model image is refused rests on the same policy and its
  tests in ADR 0006, which use a probe image.
- **The measurements use kubectl port-forward,** which does not pass through the network policy. They
  show nothing about who can reach the servers.
- **Two measurement runs, each after a fresh rebuild,** with different copier digests in between. Start-up times come from Kubernetes timestamps, which have a resolution of
  one second, and the images and some file pages were probably already cached in the guest.
- **The tool screen passing in the VM repeats a narrow screen,** with the limits in ADR 0010. Ministral
  gave the same reply to the planted instruction as on the host.
- **The guest headroom was measured with no PostgreSQL, retrieval service or gateway.**

## Measurements

Measured on the NUC, in a VM limited to 10240 MiB and 4 vCPUs, with Docker limited to 7 GiB. Each run
followed a `make rebuild`, and `make measure-inference` restarted both pods, then measured. Run 1 was on
2026-11-04, and run 2 on 2026-11-07, after the review fixes changed the copier and so both model digests.
[`test/inference/results/`](../../test/inference/results/) holds run 2; run 1 is in that file's Git
history. Each cell gives run 1, then run 2.

| Measure | Chat: Ministral 3 3B | Embeddings: Nomic | Host, ADR 0010 |
|---|---|---|---|
| Model copy | 2.3 and 2.4 s for 2048 MiB | 0.2 and 0.2 s for 139 MiB | |
| Pod created to ready | 10 and 10 s | 2 and 3 s | load 1.6 s and 0.1 s |
| Peak process RSS | 4114 and 4112 MiB | 206 and 206 MiB | 4008 and 196 MiB |
| Of which file-backed, at the end | 2042 and 2041 MiB | 155 and 155 MiB | |
| Generate, median of 3 | 14.4 and 14.9 tokens/s | | 15.2 tokens/s |
| Prompt, median of 3 | 51.4 and 55.4 tokens/s | | 56.4 tokens/s |
| Chunks of 397 tokens, median of 3 runs | | 1.92 and 2.26 per second | 2.43 per second |
| Query of 21 tokens, median of 60 | | 35.1 and 25.2 ms | 24.0 ms |
| Slowest query of 60 | | 60.7 and 38.7 ms | 51.2 ms |
| Both at once | 8.2 and 11.0 tokens/s | 0.63 and 0.76 chunks per second | not measured |
| Tool cases passed | 11 and 11 of 11 | | 11 of 11 |
| Out-of-memory kills | 0 and 0 | 0 and 0 | 0 |

In both runs the embedding sanity check gave the same cosines as on the host: 0.639 for the related passage and
0.433 for the unrelated one. The server reported build `b12030-a91eab67d`.

| Memory with both models loaded | Run 1 | Run 2 |
|---|---|---|
| Guest available | 4359 MiB | 4233 MiB |
| VM process resident and in swap | 10285 and 26 MiB | 10198 and 36 MiB |
| Host available | 10672 MiB | 11651 MiB |
| Host memory pressure, 60 and 300 s averages | 0.00, read by hand after the run | 0.00, recorded |

Ministral gave the same reply to the planted instruction in both runs. Both rebuilds passed every
Phase 1 test: built in 256 and 253 s, and tested in 534 and 544 s.
