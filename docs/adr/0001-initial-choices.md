# 0001: Initial stack and scope choices

**Status:** accepted
**Date:** 2026-10-31

## Context

The project has to produce testable evidence on one machine: a NUC with a Core Ultra 7 155H,
30 GiB RAM and no discrete GPU, which also keeps running other workloads. Time is limited to 10–20
hours a week, so each choice has to serve the claim in the [README](../../README.md).

These choices are planned and not yet tested. Each is open to change when a phase measures
something different, and that change gets its own ADR.

## Decision

| Area | Choice |
|---|---|
| Isolation | One AlmaLinux KVM VM on an isolated libvirt network, with a fixed memory limit |
| Cluster | Single-node RKE2 from the air-gap tarball, with SELinux and time-boxed CIS hardening |
| Default deny | Cluster-wide network policy, proven by a 4-hour spike before anything else in Phase 1 |
| Supply chain | Zot registry, cosign signatures, one minimal Kyverno signed-image policy |
| Delivery | Forgejo and Argo CD inside the air gap |
| Inference | llama.cpp on CPU behind an OpenAI-compatible API boundary, GGUF models in scratch images |
| Retrieval | pgvector with forced row-level security |
| Gateway and services | Go |
| Eval harness | Python |
| Licence | Apache-2.0 |

## Alternatives

- **OpenShift or OKD:** not a goal of this project.
- **Reusing the host's existing cluster:** the air gap has to be tested, and a separate VM on an
  isolated network gives an egress boundary that can be tested in layers.
- **vLLM:** it needs a GPU, and an overlay that has never run would prove nothing. It stays out
  until a real GPU run happens.

## Consequences

- Everything runs on a single node, so this gives no high-availability or day-2 evidence.
- The air gap is simulated. "From nothing" excludes the pre-built bundle.
- Signing mirrored images attests only that a digest was mirrored here.
- CPU inference limits model size and speed.

## Measurements

None yet. Memory and speed get measured in the phases that depend on them, and recorded in ADRs.
