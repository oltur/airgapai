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

Phases 1 and 2, the air gap and role-filtered retrieval, are complete. Phase 3's agent gateway, its
tests and its threat model are built, and its write-up is still to come. The models are served in
the cluster, a pinned slice of the Kubernetes documentation (CC BY 4.0) is ingested into a row-level
secured table, and a gateway lets signed-in users ask the manuals and confirm the tickets the model
proposes. The deployed gateway is tested against a stub that acts as a fully compromised model, and
the real model against planted prompt-injection passages. **The claim above is not proven in full:**
in the tested scenarios, no ticket is filed without a user's confirmation, but the tests do not show
that answers are right: the eval gates check that they cite the expected pages, not that they are
true. A misled user who confirms a harmful proposal stays a residual risk. The
[architecture](docs/architecture.md) shows the parts and where each rule is enforced. The [threat
model](docs/threat-model.md) maps each threat to its control and test, or names it as a residual
risk. The write-up of the air gap is [A blocked connection proves
nothing](https://turevskiy.com/blog/a-blocked-connection-proves-nothing/).

| Phase | What it should demonstrate |
|---|---|
| 0: repository basics | A commit guard with tests |
| 1: the air gap | A one-command offline rebuild, with layered egress tests that have working controls |
| 2: light retrieval | Role-filtered retrieval with a leak test |
| 3: agent gateway | No execution path for a side effect without a valid confirmation from an authenticated user, with an audit log and a threat model |
| 4: evals and write-up | An eval gate that fails a worse change and passes an unchanged one, plus an evidence index |

## What is and is not proven

The [evidence index](docs/evidence.md) lists each claim with its test, its latest run and what the
test does not prove, and what none of it shows: one node, one low-consequence action, no real users,
no GPU, no independent review and no day-2 evidence. The latest full run, `make rebuild
EVALS=generation` on 2026-11-23, passed every VM test and the retrieval gate, and the generation
gate passed an unchanged copy of the deployed gateway and failed the worse prompt, as it should.
The [demo](docs/demo.md) shows the claim end to end in about 3 minutes, with a recorded run.

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
| [0014](docs/adr/0014-retrieval-service.md) | Retrieval service, with recall per role |
| [0015](docs/adr/0015-approval-core.md) | Approval core: proposals, confirmations and the audit log |
| [0016](docs/adr/0016-executor.md) | The executor, and Forgejo as the directory and ticket system |
| [0017](docs/adr/0017-sign-in-and-confirmation-page.md) | Sign-in through Forgejo, and the confirmation page |
| [0018](docs/adr/0018-chat-loop-and-tools.md) | The chat loop and its two tools |
| [0019](docs/adr/0019-compromised-model-tests.md) | Testing the gateway against a fully compromised model |
| [0020](docs/adr/0020-poisoned-documents.md) | Testing against poisoned documents |
| [0021](docs/adr/0021-restricted-tickets.md) | Tickets drawn on restricted passages go where only restricted readers are |
| [0022](docs/adr/0022-cited-answers.md) | Answers cite the passages found, or are not shown |
| [0023](docs/adr/0023-pii-redaction.md) | Pattern-based redaction of personal data |
| [0024](docs/adr/0024-off-vm-audit-copy.md) | A copy of the audit log kept off the VM |
| [0025](docs/adr/0025-golden-set-and-retrieval-gate.md) | A golden set, and a retrieval gate against a saved baseline |
| [0026](docs/adr/0026-generation-eval-and-gate.md) | A generation eval through the gateway, gated on repeated runs |
| [0027](docs/adr/0027-candidate-gateway-gated.md) | Candidate gateways, gated beside the deployed one |
| [0028](docs/adr/0028-where-the-eval-gates-run.md) | Where the eval gates run |

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
make test-db       # run the retrieval and gateway database tests in a PostgreSQL container, needs Docker
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
make test-retrieval  # test retrieval by role, a leak test and the service's network paths, with controls
make test-executor   # test that the executor refuses unconfirmed calls, with audit and network checks
make test-gateway    # test sign-in, the confirmation page and the real model, with audit and network checks
make test-adversarial  # test the deployed gateway against a stub that acts as a fully compromised model
make test-poisoned   # test the deployed gateway and the real model against planted prompt-injection passages
make test-audit-copy # test that the off-VM audit copy catches a rewritten, rechained row
make audit-copy      # copy new audit rows off the VM, after checking the copied ones are unchanged
make measure-retrieval  # measure recall per role against an exact scan, in the VM
make eval-retrieval  # gate retrieval on the golden set against its saved baseline, in about 5 s
make eval-generation # gate the model's answers on the golden set through the gateway, against its baseline
make eval-candidate CANDIDATE=worse-prompt # gate a copy of the gateway with a changed prompt; deployed is the control
make test-eval-gate  # show the gate passes the control and fails the worse prompt by its answers
make demo         # show the claim end to end with the real model, in about 3 minutes
make test-egress  # test every egress path and the layer matrix from the host, with controls
make rebuild      # destroy everything, rebuild from the bundle and run the tests and the retrieval gate;
                  # EVALS=generation adds test-eval-gate, about 55 minutes more
```

The VM is at `10.77.0.10` and can be reached only from the host. Its SSH key is
`/mnt/data/airgapai/vm/id_ed25519`, regenerated on each `make vm-up`. The VM has no internet or
DNS, so `dnf install` and image pulls do not work there. Inside it, `kubectl` and the shorter `k`
are set up as aliases:

```sh
k get pods -A
```

### Trying the confirmation page

The browser reaches Forgejo and the gateway through two tunnels, on the addresses the gateway's OAuth
client is registered with. Forgejo's address needs one line in `/etc/hosts`:

```sh
echo '127.0.0.1 forgejo.forgejo.svc' | sudo tee -a /etc/hosts
k8s='sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml'  # in each terminal
infra/vm-ssh.sh -tt -L 13000:127.0.0.1:13000 "$k8s -n forgejo port-forward svc/forgejo 13000:3000"  # one terminal
infra/vm-ssh.sh -tt -L 18092:127.0.0.1:18092 "$k8s -n gateway port-forward svc/gateway 18092:8080"  # another
```

Forgejo's administrator is `airgapai`. Its password exists only in the cluster:

```sh
infra/vm-ssh.sh "$k8s -n forgejo get secret forgejo-admin -o jsonpath='{.data.password}'" | base64 -d; echo
```

Sign in at <http://forgejo.forgejo.svc:13000> as the administrator to create a user, add them to the
`technicians` team of the `maintenance` organisation, and grant the role the tool once:

```sh
infra/vm-ssh.sh "$k8s -n gateway exec deploy/executor -- /executor grant technician create_ticket"
```

Then open <http://127.0.0.1:18092/chat>, sign in as that user, and ask the manuals a question, or
describe a fault and ask for a ticket. The page waits while the model answers, on CPU. A proposed ticket is linked
from the page.

Confirming files the ticket in `maintenance/tickets`, which you can open from the result page.

## Documentation

[`docs/`](docs/README.md) holds the [architecture](docs/architecture.md), the
[threat model](docs/threat-model.md), the [evidence index](docs/evidence.md), the
[demo](docs/demo.md), the architecture decision records and the hours log.

## Licence

[Apache-2.0](LICENSE).
