# 0009: Pull request checks outside the air gap

**Status:** accepted
**Date:** 2026-11-01

## Context

Changes now reach `main` through pull requests on GitHub. The Forgejo job from ADR 0007 runs inside
the air gap, on the lab VM, so it cannot report on a GitHub pull request.

## Decision

- **A GitHub Actions job checks each pull request, each push to `main`, and once a week:** the secret
  scan without the denylist, the guard tests, and `make lint`.
- **This does not replace Forgejo.** The job holds no secrets and no cluster, and nothing in the air gap
  depends on it. Only source code leaves the lab, the same code the repository publishes.
- **Actions are limited to GitHub-owned ones, pinned by SHA,** by a repository setting and in the
  workflow. Tools are pinned by version and hash in `scripts/install-tools.sh`.
- **The Go toolchain is pinned in each `go.mod`,** so local runs and CI scan the same standard library.

## Consequences

- The job shows that lint and the guard pass on a clean clone. **It does not test the air gap:** that
  takes `make rebuild` on the lab host, which pull requests do not run.
- The denylist is local, so GitHub's scan is weaker than the local one.
- The vulnerability database is live, so a new advisory can fail a change that did not cause it.
- ShellCheck fails at warning level only. Notes remain in VM test scripts, because changing those
  needs a VM rebuild to verify.
- The ShellCheck hash records the file as first downloaded, because no checksum is published for it.
