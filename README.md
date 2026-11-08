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

Phase 1 of 4, the air gap, is complete. **The claim above is not proven yet:** only its air-gap part
has tests. The models are served in the cluster, and a pinned slice of the Kubernetes documentation
(CC BY 4.0) is ingested into a row-level secured table, but no retrieval service or gateway exists
so far. The write-up is
[A blocked connection proves nothing](https://turevskiy.com/blog/a-blocked-connection-proves-nothing/).

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
| Under row-level security, the retrieval login sees no chunk without a reader role, a technician sees only public chunks, a role set with `SET LOCAL` ends with its transaction, and the login cannot switch to a writer or owner role | `make test-rls`, part of `make rebuild` | That retrieval results respect the roles, or that the right role is chosen for a user. The database cannot require `SET LOCAL`: a plain `SET ROLE` lasts for the session, so the retrieval service must avoid it. Labels are synthetic, and the superuser can read everything. See [ADR 0012](docs/adr/0012-row-level-security-for-retrieval.md) |
| The ingestion Job from Git fills the table with the pinned corpus, a second run writes nothing, labels follow the path rule, each reader role sees its share, and the Job's label reaches PostgreSQL and the embedding server only | `make test-ingest`, part of `make rebuild` | That the chunks are good for retrieval, or that the labels fit the content. Any pod in the namespace with the Job's label gets its network paths. See [ADR 0013](docs/adr/0013-corpus-and-ingestion.md) |
| In a single-node lab, pods reach a canary outside the cluster only with both the network policy and the VM path removed | `make test-egress`, part of `make rebuild` | That no other path exists. The canary is on the host, public targets have no open-path control, and only TCP and DNS were tested. See [ADR 0008](docs/adr/0008-egress-test-and-layer-matrix.md) |

## What Phase 1 contains

```text
online host                       isolated network, no route out
+-----------------------+         +-------------------------------------+
| offline bundle build  | --ISO-> | VM: AlmaLinux, SELinux, RKE2        |
| host firewall         |         |  - default-deny network policy      |
| egress test + canary  | --SSH-> |  - Pod Security, restricted         |
+-----------------------+         |  - Zot registry, read-only          |
                                  |  - Kyverno, signed images only      |
                                  |  - Forgejo, CI runner, Argo CD      |
                                  +-------------------------------------+
```

Each decision, with its results and limits, is in an architecture decision record:

| ADR | Topic |
|---|---|
| [0002](docs/adr/0002-secret-guard.md) | Commit guard |
| [0003](docs/adr/0003-default-deny-with-calico-gnp.md) | Cluster-wide default deny |
| [0004](docs/adr/0004-offline-install-and-black-hole-route.md) | Offline install and black-hole route |
| [0005](docs/adr/0005-baseline-hardening.md) | Baseline hardening |
| [0006](docs/adr/0006-registry-and-signed-image-admission.md) | Read-only registry and signed-image admission |
| [0007](docs/adr/0007-git-ci-and-argo-inside-the-air-gap.md) | Git, CI and Argo CD inside the air gap |
| [0008](docs/adr/0008-egress-test-and-layer-matrix.md) | Egress test and layer matrix, with measurements |
| [0009](docs/adr/0009-pull-request-checks-outside-the-air-gap.md) | Pull request checks outside the air gap |
| [0010](docs/adr/0010-answer-and-embedding-models.md) | Answer and embedding models, with host measurements |
| [0011](docs/adr/0011-model-images-and-serving.md) | Model images and serving in the cluster, with VM measurements |
| [0012](docs/adr/0012-row-level-security-for-retrieval.md) | PostgreSQL, pgvector and row-level security for retrieval |
| [0013](docs/adr/0013-corpus-and-ingestion.md) | Corpus and ingestion |

## Requirements

A Linux host with KVM and libvirt, `virt-install`, `qemu-img`, `xorriso`, `nft`, Go, Git, Make and
passwordless `sudo`. The scripts keep their data in `/mnt/data/airgapai`, set in
[`infra/versions.env`](infra/versions.env), and the first bundle build downloads its pinned inputs.
The VM is limited to 10 GiB and 4 vCPUs, and `make vm-up` refuses to start without 3 GiB to spare.
Phase 1 measurements at 8 GiB are in ADR 0008, and the rebuild at 10 GiB is in ADR 0010.

The scripts change the host: they install a firewall service and a libvirt network, and the egress
test briefly adds a dummy interface and firewall rules, then removes them. Read them first.

## Running the checks

You need Git, Make and Go. The guard also needs a local, gitignored `.denylist` with one extended
regular expression per line.

```sh
make help          # list the targets
make tools         # install pinned gitleaks, ShellCheck, Ruff and govulncheck in ~/.local/bin
make setup         # check the tools and enable the git hooks
make test-secrets  # prove the guard in a temporary repo, using generated fake values
make check         # scan the full history
make lint          # run ShellCheck, Ruff, go vet and govulncheck
```

For CI on a clean clone, `DENYLIST_OPTIONAL=1` runs the gitleaks part alone. Pull requests run these
checks on GitHub. See [ADR 0009](docs/adr/0009-pull-request-checks-outside-the-air-gap.md).

## Using the VM

```sh
make bundle   # build the offline bundle, on the online host
make vm-up    # create the isolated network and VM, and install RKE2 from the bundle
make vm-ssh   # log in as alma, who has passwordless sudo
make vm-down  # remove the VM and anything changed inside it
make test-hardening  # test Pod Security, default deny and DNS on the VM, with controls
make test-signed-images  # test signed-image admission and the read-only registry, with controls
make test-gitops  # test that Git drives the cluster and CI catches a secret, with controls
make test-rls     # test row-level security on the chunks table, with controls
make test-ingest  # test ingestion, a second run, the labels and the Job's network paths, with controls
make test-egress  # test every egress path and the layer matrix from the host, with controls
make rebuild      # destroy everything, rebuild from the bundle and run all of the above
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
