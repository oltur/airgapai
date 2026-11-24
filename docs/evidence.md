# Evidence index

Each claim this repository makes, the test that shows it, the latest run of that test, and what the
test does not prove. The ADR named in each row holds the measurements and the longer list of limits.
The [threat model](threat-model.md) maps the same tests to threats, and the [demo](demo.md) shows
the claim end to end in one command.

## The runs

| Run | What | Result |
|---|---|---|
| **R** | `make rebuild EVALS=generation` of `889c131` on 2026-11-23: the VM destroyed, the bundle and the VM built again, then every VM test, the audit copy, the retrieval gate, the generation gate's pair and a second audit copy | Passed. Built in 276 s, tested in 4173 s |
| **R2** | `make rebuild` of `3d70755` on 2026-11-24, with ADRs 0029 and 0030 deployed: every VM test, the audit copy and the retrieval gate. Then `make eval-generation` and `make demo` | Passed, built in 316 s, tested in 1041 s; the retrieval gate 33 of 33 at hit@3, no leak. `make eval-generation` exited 2, as ADR 0029 expected: the gateway image differs from the baseline's, and nothing else. Its answers matched the baseline's, failing only p11 and i03. `make demo` held every rule |
| **CI** | The `checks` job on GitHub for `889c131`, on 2026-11-23: the secret scan without the local denylist, the guard tests, `make lint` with the gates' unit tests, and `make test-db` in a PostgreSQL container | Passed |

R ran on the lab machine, a single-node VM behind a simulated air gap, with the images pinned in
`infra/versions.env`. CI has no VM, so it tests no deployment.

## Claims

| Claim | Test | Latest run | What it does not prove |
|---|---|---|---|
| No commit holds a detected secret or a denylisted term, and the commit guard blocks a planted one | `make check`, `make test-secrets` | CI, without the denylist. Locally, with it: the guard on the staged changes of each commit, and `make check`'s full history on each push | That every secret is caught: Gitleaks knows patterns, the denylist only terms listed in advance. CI runs without the denylist. [0002](adr/0002-secret-guard.md) |
| The VM has SELinux enforcing, default deny from boot, CoreDNS with no forwarder, and restricted Pod Security outside the exempt namespaces | `make test-hardening` | R | That the exempt namespaces are safe: anyone who can create pods in `kube-system` bypasses all of it. RBAC was not tested. [0005](adr/0005-baseline-hardening.md) |
| Outside the exempt namespaces, only images signed by the lab key, by digest, from the read-only registry run, and admission fails closed while the verifier is down | `make test-signed-images` | R | Who built an upstream image: the signature says only that its digest was mirrored here. Signed test images pass too. [0006](adr/0006-registry-and-signed-image-admission.md) |
| Git inside the air gap drives the cluster: a commit reaches it, drift is reverted, and Forgejo's CI fails a commit with a secret | `make test-gitops` | R | That pushes are trusted: anyone who can push controls the cluster, and commits are not signature-checked. [0007](adr/0007-git-ci-and-argo-inside-the-air-gap.md) |
| Pods reach a canary outside the cluster only with both the network policy and the VM's path removed, each layer shown working by its control | `make test-egress` | R | That no other path exists. The canary is on the host, and only TCP and DNS were tested. [0008](adr/0008-egress-test-and-layer-matrix.md) |
| Under row-level security, the retrieval login sees nothing without a role, a technician only public chunks, a role set with `SET LOCAL` ends with its transaction, and the login cannot become a writer or owner | `make test-rls` | R | That the right role is chosen for a user. The database cannot require `SET LOCAL`, and the superuser reads everything. [0012](adr/0012-row-level-security-for-retrieval.md) |
| The ingestion Job fills the table from the pinned corpus, a second run changes nothing, labels follow the path rule, and its network label reaches PostgreSQL and the embedding server, but not the chat model, Forgejo or the registry | `make test-ingest` | R | That the chunks are good, or that the synthetic labels fit the content. Any pod given the Job's label gets its paths. [0013](adr/0013-corpus-and-ingestion.md) |
| Retrieval returns different chunks by role, refuses unknown roles and a role left on a pooled connection, and gave a technician no restricted chunk over 24 queries | `make test-retrieval` | R | That other queries leak nothing: these are the queries written. [0014](adr/0014-retrieval-service.md) |
| A tool call runs only with an unused, unexpired confirmation for the same user, tool and arguments, by a role that still permits it, and every decision is in a hash chain the gateway cannot rewrite | `make test-db` | CI | That entries are true: the gateway can append false ones that chain correctly. [0015](adr/0015-approval-core.md) |
| The executor files nothing without a valid confirmation, has no confirm or propose command, and reaches only PostgreSQL and Forgejo. It tells the gateway "not filed" only when that is certain | `make test-executor`; `make test-db` for "not filed" (ADR 0029) | R; R2; `make test-db` for "not filed", which no VM test reaches | That its token is narrow: it can edit and close any issue in `maintenance`. [0016](adr/0016-executor.md) |
| Users sign in through Forgejo, see only their own proposals, rendered from the stored arguments with hidden characters shown, and a confirmation needs their session, CSRF token and origin. A team change after sign-in is refused at execution. Once confirmed, the page shows what became of the proposal instead of the button (ADR 0030) | `make test-gateway`, `make test-db` | R, CI; R2, with ADR 0030's filed outcome checked by `make demo`, the others by `make test-db` | That identity holds against tampering inside the cluster, which has no TLS, or that a misled user does not confirm. [0017](adr/0017-sign-in-and-confirmation-page.md) |
| In a chat turn, searches use the session's role, a ticket the model asks for is only a proposal, and nothing in the loop can confirm | `make test-db`, `make test-gateway` with the real model | R, CI | That the model follows its instructions. [0018](adr/0018-chat-loop-and-tools.md) |
| A stub acting as a fully compromised model, through the deployed gateway's image, secrets and network rules, files, confirms and executes nothing | `make test-adversarial` | R | That no other output gets through: the scenarios are the ones written. [0019](adr/0019-compromised-model-tests.md) |
| With planted passages instructing the real model, nothing was filed or confirmed, and any proposal showed the planted passage | `make test-poisoned` | R | That the model resists injection: one planted passage made it propose a destructive ticket in every run. It stayed a proposal. [0020](adr/0020-poisoned-documents.md) |
| A ticket drawn on any restricted passage goes where only engineers can read it, decided by the gateway's own searches | `make test-adversarial`, `make test-db` | R, CI | That no restricted fact reaches a technician another way. [0021](adr/0021-restricted-tickets.md) |
| An answer is shown only if it cites passages found in its turn | `make test-db`, `make test-adversarial` | R, CI | That a shown answer is right: citations are checked for form and range, not truth. [0022](adr/0022-cited-answers.md) |
| Email addresses, phone numbers, and IBANs and card numbers that pass their checksums are removed from questions, tickets and gateway logs | `make test-db`, `make test-adversarial` | R, CI | That no personal data gets through: names and postal addresses are not found. Tested on made-up data. [0023](adr/0023-pii-redaction.md) |
| A copy of the audit log on the host catches a row rewritten in the VM with every later hash recomputed | `make test-audit-copy`; `make audit-copy` copies each rebuild's log after the tests, and again after the generation gate's pair with `EVALS=generation` | R | That rows since the last copy are protected, or that the copy is safe from root on the host. [0024](adr/0024-off-vm-audit-copy.md) |
| On the golden set's 47 questions, all 33 with an expected page find one in the top 3, no restricted chunk reaches a technician at k=20, and the gate fails a lost hit or a leak against the baseline | `make eval-retrieval`, in every rebuild; the failing rules in its unit tests | R: passed, 33 of 33 at hit@3, no leak. CI: the unit tests, including a lost hit and a leak failing the gate | That retrieval is good for real wording: the questions follow the pages' headings. [0025](adr/0025-golden-set-and-retrieval-gate.md) |
| Through the deployed gateway and the real model, 45 of the 47 questions passed in each of three baseline runs, with nothing filed, confirmed or leaked, so the tolerance is 0; an unchanged gateway then passes the gate | `test/eval/generation.py --save --runs 3` for the baseline; the deployed image, through a copy, as the control in `make test-eval-gate` | Baseline: ADR 0026's three runs, 2026-11-18. R: the control passed, failing only p11 and i03 as the baseline does, 27 s per turn | That answers are right: four leak questions were answered from the model's general knowledge, citing unrelated pages. [0026](adr/0026-generation-eval-and-gate.md) |
| A deliberately worse system prompt fails the generation gate by its answers, and an unchanged copy passes | `make test-eval-gate`, in `make rebuild EVALS=generation` | R: the worse prompt lost 30 of the 45 stable questions and showed no answer, 41 s per turn | That the gate sees a subtler change, or a worse chunking change or model: none has been run. [0027](adr/0027-candidate-gateway-gated.md) |
| The gates' rules, and their exit codes, fail when broken | The unit tests in `make lint` | CI | That the deployment's answers are good: CI has no VM. [0028](adr/0028-where-the-eval-gates-run.md) |

## What none of this shows

A sceptical reader can say each of these, and would be right:

- **One node.** One VM on one home machine, with no high availability.
- **One low-consequence action.** The only thing the model can cause is a proposed ticket. An action
  that changes a real system would need more than one person's confirmation.
- **No real users.** Test accounts and written questions only.
- **No GPU.** Everything runs on CPU, and nothing here is measured on a GPU.
- **No independent review.** The tests and this index were written by the project, and reviewed by
  the same project's tooling.
- **No day-2 evidence.** No upgrades, backups, monitoring or time in use.
- **No TLS inside the cluster.** Identity between services rests on the network rules.
- **A misled user can confirm a harmful proposal.** The page shows what will be filed and why, but a
  person decides.

The threat model's [residual risks](threat-model.md#residual-risks) list the rest.
