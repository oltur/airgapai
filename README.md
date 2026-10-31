# Air-gapped RAG and agent stack

A reference repository, built on a single-node lab, working towards testable evidence for one
claim:

> AI that can act on real systems safely, where data is not allowed to leave the building.

The scenario is a maintenance-manual assistant. Technicians ask questions and get answers cited
from technical documentation. They can file a maintenance ticket only after confirming it. All
documents are public.

The stack is designed for air-gapped environments and runs in a simulated air gap. It is a
private, non-commercial project, written independently from public specifications.

## Status

Phase 1 of 4: the air gap, in progress. **The claim above is not proven yet.** The repository
guard is done, and a [default-deny spike](docs/adr/0003-default-deny-with-calico-gnp.md) has passed
on a throwaway VM with internet access. RKE2 now
[installs from an offline bundle](docs/adr/0004-offline-install-and-black-hole-route.md) on an
isolated network, with [baseline hardening](docs/adr/0005-baseline-hardening.md) but no registry,
image signing or formal egress test yet. No model, retrieval or gateway code exists so far.

| Phase | What it should demonstrate |
|---|---|
| 0: repository basics | A commit guard with tests |
| 1: the air gap | A one-command offline rebuild, with layered egress tests that have working controls |
| 2: light retrieval | Role-filtered retrieval with a leak test |
| 3: agent gateway | No execution path for a side effect without a valid confirmation from an authenticated user, with an audit log and a threat model |
| 4: evals and write-up | An eval gate that fails a worse change and passes an unchanged one, plus an evidence index |

## What is and is not proven

| Claim | Test | What it does not prove |
|---|---|---|
| The commit guard blocks a generated fake secret or denylisted term, and lets a clean file through | `make test-secrets` | That every secret is caught. Gitleaks works from known patterns, and the denylist holds only terms listed in advance |
| No commit on any ref contains a detected secret or a denylisted term | `make check` | The same limit. Without a local `.denylist` this check refuses to run |

## Running the checks

You need Git, Make, and [gitleaks](https://github.com/gitleaks/gitleaks) 8.30.1 in
`~/.local/bin`. The guard also needs a local, gitignored `.denylist` with one extended regular
expression per line.

```sh
make help          # list the targets
make setup         # check the tools and enable the git hooks
make test-secrets  # prove the guard in a temporary repo, using generated fake values
make check         # scan the full history
```

For CI on a clean clone, `DENYLIST_OPTIONAL=1` runs the gitleaks part alone.

## Using the VM

```sh
make bundle   # build the offline bundle, on the online host
make vm-up    # create the isolated network and VM, and install RKE2 from the bundle
make vm-ssh   # log in as alma, who has passwordless sudo
make vm-down  # remove the VM and anything changed inside it
make test-hardening  # test Pod Security, default deny and DNS on the VM, with controls
```

The VM is at `10.77.0.10` and can be reached only from the host. Its SSH key is
`/mnt/data/airgapai/vm/id_ed25519`, regenerated on each `make vm-up`. The VM has no internet or
DNS, so `dnf install` and image pulls do not work there. Inside it, `kubectl` and the shorter `k`
are set up as aliases:

```sh
k get pods -A
```

## Documentation

[`docs/`](docs/README.md) holds the architecture decision records and the hours log.

## Licence

[Apache-2.0](LICENSE).
