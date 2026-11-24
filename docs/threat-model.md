# Threat model

This document covers the stack as built with Phase 4's eval gates (ADRs 0025 to 0028). Phase 4
changed one thing in what `deploy/` runs, the chat server's prompt cache (ADR 0026). It added the
gates, which run from the host, and signed candidate gateways in the registry, which run beside the
deployment during an eval. It lists what an attacker could want,
who could try it, and for each threat the control that stands in the way, the test that shows the
control works, and what is left over. Where a row says "residual", nothing in this repository stops
that threat, and the row says so.

Every control named here is described in an ADR, and every test can be run with `make`. Threats are
grouped by STRIDE, then by the LLM-specific risks in the OWASP Top 10 for LLM Applications 2025.
Failure modes come after that, then the residual risks in one list.

## The claim and its scope

> AI that can act on real systems safely, where data is not allowed to leave the building.

In this lab, "act" means filing a Forgejo issue in the `maintenance` organisation. "Safely" means three
things:

- no ticket is filed without a valid confirmation from an authenticated user
- a reader never gets text above their role, through an answer or through a ticket
- every decision is in an audit log whose rewriting is detectable

"Not leaving the building" means no path out of the cluster or the VM.

The lab is one VM on one host, with test users and public documents. The threats are judged for that
lab, and the gaps a real deployment would have to close are listed as residual risks.

## Assets

| Asset | Why it matters |
|---|---|
| The ability to file a ticket | The side effect the model must not cause on its own |
| The executor's Forgejo token | The only credential that files tickets for the gateway. It can also edit and close any issue in `maintenance` |
| Restricted passages | The synthetic stand-in for text above a technician's role |
| Users' sessions and roles | They decide who may confirm and what they may read |
| The audit log | The record of every decision, and the evidence if something goes wrong |
| Personal data typed into questions | It goes to the model, to tickets and possibly to logs |
| Everything inside the VM | It must not leave: the air gap |
| The images, models and corpus | What runs, and what the model reads |

## Actors

| Actor | Trusted? | What they can do |
|---|---|---|
| The model | **No.** Treated as fully compromised | Write any text and any tool call, in any order |
| Documents in the corpus | **No.** Treated as possibly poisoned | Put instructions in front of the model |
| A signed-in technician or engineer | For their own actions only | Ask, read what their role allows, confirm their own proposals |
| Anyone who reaches the gateway without signing in | No | Start a sign-in, nothing else |
| Another service on the operator's machine | No | Reach `127.0.0.1` ports, and with them the gateway's cookies |
| A pod in the cluster | No, beyond its own network rules | Reach only what its network rules allow |
| The cluster administrator, root on the VM, the database superuser | **Yes.** Out of scope, except for the audit log | Anything inside the VM |
| Root on the host | Yes. Out of scope | Anything, including the audit copy |

The model and the corpus are where the interesting attacks come from, so they get the worst-case
assumption. The administrator is trusted because the lab has one. The audit log is the one place where
this model asks what an insider could do, because its copy off the VM exists for that.

## Data flow and trust boundaries

```text
 browser ── port-forward ──> gateway ──> chat model (untrusted output)
    │                          │  ├────> retrieval ──> embedding model
    │                          │  │          └──────> PostgreSQL (row-level security)
    │                          │  ├────> PostgreSQL: proposals, confirmations, audit log
    │                          │  └────> executor ──> Forgejo: files the issue
    └── port-forward ──> Forgejo: sign-in, reading tickets

 Forgejo (Git) ──> Argo CD ──> cluster objects ──> Kyverno: signed images only ──> Zot (read-only)
 corpus in an image ──> ingestion Job ──> embedding model, PostgreSQL
 host ── SSH ──> VM: audit copy, tests          offline bundle ── ISO ──> VM
```

Every arrow inside the VM is plain HTTP or PostgreSQL without TLS, and limited by the cluster's default
deny network rules. Only the gateway may call retrieval and the executor. The test copies of the
gateway, the compromised-model stub's and the eval's candidates, carry its network label and so
the same paths, while a test runs them. Nothing in the cluster may call the gateway: the browser
reaches it through `kubectl port-forward`.

The boundaries that matter:

- **The model's output and the gateway.** The gateway parses it into tool calls and an answer, and
  trusts neither.
- **The browser and the gateway.** A session, its CSRF token and the origin check guard every form.
- **The gateway and the executor.** The executor files only against a confirmation it checks itself, in
  the database, with a fresh role lookup in Forgejo.
- **The VM and the world.** There is no route out.

## STRIDE

### Spoofing

| Threat | Control | Evidence | Residual |
|---|---|---|---|
| Someone acts as another user at the gateway | Sign-in through Forgejo with PKCE, `state` and `nonce`; the ID token's issuer, audience, expiry and nonce are checked; sessions are random 256-bit cookies | `make test-gateway`, ADR 0017 | The ID token is not signature-checked, which is only safe over TLS. Inside the cluster it is plain HTTP, so anything that can tamper with in-cluster traffic could forge an identity |
| The model claims a user or a role | The tools have no field for a user, a role or a token, and such fields are refused; searches use the session's role | `make test-adversarial` (`escalate`, `forbidden`), `make test-db`, ADRs 0018 and 0019 | None found in the tested scenarios |
| A pod pretends to be the gateway to the executor | Only the gateway's label may reach the executor. The executor files only with a valid confirmation token, which it checks in the database, so a caller without one files nothing | `make test-executor`, `make test-gateway` network checks, ADR 0016 | Any pod given the gateway's label gets its network paths. A token taken in transit could be replayed until it is used or expires, since the channel has no TLS |
| Another local service reads or plants the gateway's cookies | Gateway-specific cookie names; `HttpOnly`, `SameSite=Lax` | ADR 0017 | Residual. Browsers do not separate cookies by port on `127.0.0.1`. On its own HTTPS host the gateway would use `Secure` and `__Host-` |

### Tampering

| Threat | Control | Evidence | Residual |
|---|---|---|---|
| The arguments change between what the user sees and what runs | The confirmation is bound to the user, the tool and the hash of the canonical arguments, and the page is rendered from those stored arguments. The executor sends only those | `make test-db` (altered arguments, key order, duplicate keys), ADR 0015 | None found in the tested cases |
| Hidden or reordering characters make the page say something other than what is filed | Every code point that hides, disguises or reorders text is shown as its code point | `make test-adversarial` (`lie`), `make test-db`, ADR 0017 | Lookalike letters from other scripts are visible characters and are not flagged |
| A planted document steers the model | The model's output is never trusted: see [LLM01](#llm-specific-risks) | `make test-poisoned`, ADR 0020 | The model was steered. See LLM01 |
| A source's text changes after it was cited | Each source is stored with the chunk's content hash. The confirmation page shows text only if the hash still matches | `make test-db`, ADR 0018 | None found in the tested cases |
| The audit log is rewritten | The gateway's login can only append. Each row hashes the previous one. A copy kept off the VM checks every copied row on each run | `make test-db`, `make test-audit-copy`, ADRs 0015 and 0024 | Rows written since the last copy can be rewritten without detection. Root on the host can change the copy |
| An image is swapped | Kyverno admits only images signed with the lab key, by digest, from a read-only registry. The guard webhook fails closed if Kyverno is down | `make test-signed-images`, `make test-egress`, ADR 0006 | The signature attests only that the digest was mirrored here, not who built it upstream. Exempt namespaces such as `kube-system` are not checked. The test images are signed with the same key and admitted like the deployed ones: the compromised-model stub and the candidate gateways, one with a prompt that drops the safety rules (ADRs 0019 and 0027). `deploy/` names none of them, but anyone who can push to Git or create a Deployment can run them |
| The deployed configuration is changed | Argo CD deploys only what Git names, and reverts manual changes | `make test-gitops`, ADR 0007 | Residual. Anyone who can push to the in-cluster repository controls the cluster. Commits are not signature-checked |
| A change to the prompt, the model or the chunks makes answers worse, or leaks | Each eval gate compares a run with a saved baseline, question by question, on the golden set's 47 questions: the retrieval gate scores hits on the 33 with an expected page, and leaks on all. `make rebuild` runs the retrieval gate every time, and the generation gate with `EVALS=generation`. A candidate prompt is gated as a copy beside the deployed gateway | `make eval-retrieval`, `make test-eval-gate`, ADRs 0025 to 0028 | Only a prompt can be gated before it is deployed. A model or chunking change is gated only after a rebuild has deployed it, and no such change has been run. The generation gate is opt-in, and nothing makes a prompt or model change run it. It has been shown failing one far worse prompt, not a subtle one. That prompt passed the refusal and leak checks only because every reply was withheld |
| The executor's token edits or closes existing issues | The token is scoped to issues and to reading the organisation. The executor's code only creates issues | ADR 0016 | Residual. The token's scope allows editing and closing any issue in `maintenance` |

### Repudiation

| Threat | Control | Evidence | Residual |
|---|---|---|---|
| A user denies confirming a ticket | Every proposal, confirmation, execution, refusal and grant is logged with its actor, in the hash chain. If the audit write fails, the action fails | `make test-db` (audit insert revoked), every VM test's chain check, ADR 0015 | The outcome entry can be lost if the process dies after the call and before the entry. The decision entry before it remains |
| An administrator denies a grant | `grant` and `revoke` are logged as the administrator's decisions | `make test-db`, ADR 0015 | Who may run them is whoever can exec into the executor's pod. The name logged is the one they give |
| False history is written | None | ADR 0024 | Residual. The gateway's login can append false entries that chain correctly. The copy shows that history was not changed later, not that each entry was true |

### Information disclosure

| Threat | Control | Evidence | Residual |
|---|---|---|---|
| A technician reads a restricted passage | Row-level security, forced, on a non-owner login; the role set with `SET LOCAL` from the session; no role returns nothing; the confirmation page reads sources with the viewer's role | `make test-rls`, `make test-retrieval` (leak test), `make test-poisoned` (restricted canary), ADRs 0012, 0014 and 0020 | The leak test covers 24 queries. The database superuser can read everything |
| Restricted text reaches a ticket technicians can read | A ticket drawn on any restricted source goes to `tickets-restricted`, which only engineers read. The visibility comes from the gateway's own searches, not the model | `make test-adversarial`, `make test-db`, ADR 0021 | The routing depends on Forgejo's team setup, which the test checks but the gateway cannot. Over-routing keeps some harmless tickets from technicians |
| A user sees another user's proposal | Another user's proposal returns the same 404 as one that does not exist | `make test-gateway`, ADR 0017 | None found in the tested cases |
| The confirmation token reaches the model or the browser | The token exists only in the confirming request's memory, and goes straight to the executor | `make test-gateway`, `make test-db`, ADRs 0017 and 0018 | None found in the tested cases |
| The CSRF token or system details reach the model | The model is sent the question, the passages and tool results only | `make test-db`, ADR 0018 | The system prompt is public in this repository, and holds nothing secret |
| Personal data reaches the model, a ticket or a log | Pattern-based redaction of email addresses, phone numbers, and IBANs and card numbers that pass their checksums, on the question, the ticket fields and every gateway log line | `make test-db`, `make test-adversarial` (`echo`), ADR 0023 | Names, postal addresses and other forms without a fixed pattern are not found. Other services' logs are not redacted |
| Data leaves the VM | No route out of the VM, a host firewall, cluster-wide default deny, and CoreDNS without a forwarder, each with an open-path control | `make test-egress`, `make test-hardening`, ADRs 0003, 0004, 0005 and 0008 | The canary is on the host, not on a remote network. Only TCP and DNS were tested |
| Traffic inside the cluster is read | None | ADR 0017 | Residual. No TLS inside the cluster |
| Secrets in the cluster are read | Kubernetes Secrets; credentials never printed by bootstrap | ADR 0007 | Residual. Anyone who can read Secrets in a namespace has them. Encryption at rest was not examined |

### Denial of service

| Threat | Control | Evidence | Residual |
|---|---|---|---|
| The model loops or floods tool calls | Per turn: at most 4 steps, 6 tool calls, 2 searches, 1 proposal, 8 KiB of tool arguments | `make test-adversarial` (`loop`, `flood`, `huge`), `make test-db`, ADRs 0018 and 0019 | None found in the tested scenarios |
| Oversized requests | Size limits on the question and every form | `make test-db`, ADR 0018 | None found in the tested cases |
| Many unfinished sign-ins | At most 1,000 in progress, each for ten minutes, pruned once a minute | `make test-db`, ADR 0017 | At the limit, new sign-ins are refused until old ones expire |
| One user holds the model | One turn at a time; another user is told to try again | `make test-db`, ADR 0018 | Residual. One signed-in user can keep the model busy, since a turn takes most of a minute on CPU. Per-user rate limits are not built |

### Elevation of privilege

| Threat | Control | Evidence | Residual |
|---|---|---|---|
| The model files a ticket | `create_ticket` only proposes. No tool confirms, and a confirmation needs the user's session, CSRF token and origin | `make test-adversarial`, `make test-poisoned`, `make test-gateway`, ADRs 0018 to 0020 | A misled user can still confirm. See [residual risks](#residual-risks) |
| A user keeps reading after losing their role | None. Searches, and the source text on the confirmation page, use the role from sign-in | ADR 0017 | Residual. The role is fixed in the session until it ends, after an hour at most. Only execution asks Forgejo again |
| A user confirms after losing their role | The executor asks Forgejo for the user's current teams at execution, and checks the grant | `make test-gateway` (team change after sign-in), `make test-db` (role and grant revoked), ADRs 0015 to 0017 | A team change that lands after the lookup and before the commit applies to the next decision |
| A role without the grant proposes | Tool permissions are rows of role and tool, and no row means no use | `make test-db`, ADR 0015 | None found in the tested cases |
| The model replays, alters or borrows a confirmation | Single-use, atomic, bound to user, tool, argument hash and a 5-minute expiry | `make test-db` (replay, 20 concurrent uses, another user, expiry), ADR 0015 | Not tested through the page, which never reveals the token |
| A pod runs privileged or on the host network | Pod Security `restricted` outside the exempt namespaces | `make test-hardening`, `make test-egress`, ADR 0005 | Residual. Anyone who can create pods in `kube-system` bypasses Pod Security, default deny and the image policy. RBAC was not tested |
| A role stays on a pooled database connection | Roles are set with `SET LOCAL`; the service refuses a connection with a role still set | `make test-retrieval`, ADRs 0012 and 0014 | The database cannot require `SET LOCAL`. The service must avoid plain `SET ROLE` |

## LLM-specific risks

Mapped to the [OWASP Top 10 for LLM Applications 2025](https://genai.owasp.org/llm-top-10/).

| Risk | How it applies here | Control | Evidence | Residual |
|---|---|---|---|---|
| LLM01 Prompt injection | A planted passage, or a user's question, instructs the model | Nothing tries to stop the injection. The gateway limits what an injected model can cause: searches with the session's role, proposals only, and a page that shows the source to the person who decides | `make test-poisoned`, `make test-adversarial`, ADRs 0019 and 0020 | **The model was steered.** In every run, the liveness passage made it propose a ticket nobody asked for. Since ADR 0022's prompt change, that was the destructive ticket the passage named. In the generation eval, the golden set's i03, which claims a proposal was already confirmed, got a ticket proposed in every run (ADR 0026). Each stayed a proposal |
| LLM02 Sensitive information disclosure | Restricted text or personal data in answers, tickets or logs | Row-level security, restricted routing, redaction | See [Information disclosure](#information-disclosure) | An engineer's turn gives the model restricted passages, and its answer can repeat them to that engineer, as intended |
| LLM03 Supply chain | A tampered image, model file or corpus | Pinned digests, reproducible Go images, signed admission, model hashes from each Hugging Face record, a pinned corpus commit | `make test-signed-images`, ADRs 0006, 0010, 0011 and 0013 | Provenance is limited: hashes from the publisher's record are not signatures, and the llama.cpp file was hashed as first downloaded |
| LLM04 Data and model poisoning | A document added to the corpus with instructions | The corpus comes from Git through the ingestion Job, labelled by path | `make test-poisoned`, ADR 0020 | A document that reaches the corpus is read by the model. See LLM01 |
| LLM05 Improper output handling | The model's text is rendered as HTML, or read as a form or link | Escaped by `html/template`; a Content Security Policy with no scripts and forms only to the gateway; marked as the model's | `make test-adversarial` (`lie`), ADRs 0017 and 0019 | A convincing text can still persuade a reader |
| LLM06 Excessive agency | The model acts beyond what the user asked | Two tools only, one read-only and one that proposes; per-turn limits; the user confirms each ticket | `make test-adversarial`, ADR 0019 | The user's confirmation is the last control. See [residual risks](#residual-risks) |
| LLM07 System prompt leakage | The prompt reveals secrets or controls | The prompt is public and holds no secret. No control depends on the model obeying it: the rules that matter, such as roles, proposals only and citations, are enforced by the gateway | ADRs 0018 and 0022 | None |
| LLM08 Vector and embedding weaknesses | A role's search returns another role's chunks, or too few | Row-level security applies to the vector search; iterative HNSW scans; recall per role measured against an exact scan | `make test-retrieval`, `make measure-retrieval`, `make eval-retrieval`, ADRs 0014 and 0025 | Recall against an exact scan was measured for k of 5 and more. At the chat's k=3, the golden set's 33 questions with an expected page found one, but they follow the pages' own headings, so they are easy |
| LLM09 Misinformation | A wrong answer, or one with made-up sources | An answer is shown only if it cites passages that the turn found. The generation gate checks that each answer question cites an expected page | `make test-db`, `make test-adversarial` (`uncited`, `fakecite`), `make test-eval-gate`, ADRs 0022, 0026 and 0027 | **Citations are checked for form and range, not truth,** by the gateway and by the gate. Citing the expected page does not show the answer is right. Four of the five leak questions were answered from the model's general knowledge, citing unrelated public pages (ADR 0026) |
| LLM10 Unbounded consumption | The model or a user exhausts the CPU | Per-turn limits; one turn at a time; size limits | See [Denial of service](#denial-of-service) | One user can hold the model |

## Failure modes

The rule is to fail closed: when a part the decision depends on is missing, nothing is filed.

| What fails | What happens | Evidence |
|---|---|---|
| The audit write | The action fails, and the confirmation stays unused | `make test-db`, ADR 0015 |
| Forgejo rejects the create call, with a 4xx | Logged as `failed`. The executor tells the gateway nothing was filed, and the page, titled "Not filed", says so | `make test-db`, ADRs 0016 and 0029 |
| Forgejo gives no answer to the create call, or a 5xx | The outcome is logged as `unknown`, since the issue may exist | `make test-db`, ADRs 0016 and 0029 |
| Forgejo, at sign-in | The sign-in fails | The gateway's code; not tested |
| The executor refuses | The page says nothing was filed. The proposal cannot be confirmed again, and has to be proposed again | `make test-db`, ADR 0017 |
| The executor gives no answer, or one that is not a result | The page, titled "Result unknown", says the ticket may have been filed, and names the repository to check before proposing again. For a restricted ticket it says only engineers can read it. Until PR #21 the title said "Not filed", which could have led to a duplicate ticket. The proposal's page then says the same, in place of the button | `make test-db`, ADRs 0017, 0029 and 0030 |
| The model server | The turn ends with an error page. A proposal made earlier in the turn stays a proposal. In the first generation eval, the chat server was killed for memory under sustained use, and ten turns failed until it restarted; its prompt cache is now off | `make test-db`, ADR 0018, ADR 0026 |
| Retrieval, during a search | The model is told the search failed, and gets no passages | The gateway's code; not tested |
| No role reaches the database | The query returns nothing | `make test-rls`, `make test-retrieval`, ADR 0012 |
| Kyverno stops, cleanly or by force | Signed images are denied too, until it returns | `make test-signed-images`, ADR 0006 |
| The registry | Running pods continue, and containers restart from cache. New pods cannot start, signed ones too, since the verifier needs the registry | `make test-egress`, ADR 0008 |
| The gateway restarts | Every session ends, and sign-ins in progress fail | ADR 0017 |
| A citation check fails | The model's reply is withheld, and the gateway says why | `make test-db`, `make test-adversarial`, ADR 0022 |
| An executor reply names no repository | It is not taken for success | `make test-db`, ADR 0021 |
| An eval gate, in `make rebuild` | The rebuild fails. The gates run after every test and the audit copy, and the generation gate's turns are copied whether it passes or not, so no security test is skipped and no audit row is left uncopied. The retrieval gate exits 1 for worse results or a leak; the generation gate exits 1 for worse answers only, and 2 for a leak, which is one of its hard failures. Both exit 2 for anything else | Exit codes: unit tests in `make lint`. The order: the Makefile, ADR 0028. Neither rebuild has yet been run as a whole |

## Residual risks

These are the risks left after the controls above, roughly from most to least important in this lab.

1. **A misled user confirms a harmful proposal.** This is the plan's stated residual risk. The model
   was steered by a planted passage into proposing a destructive ticket. The gateway kept it a
   proposal, and the page showed what would be filed and the passage behind it. But the page cannot
   make anyone read it. The confirmation is the last control, and it rests on a person.
2. **No TLS inside the cluster.** The ID token, the confirmation token, the database traffic and
   Forgejo's API cross plain HTTP. Anything that can tamper with or read in-cluster traffic could
   forge an identity or reuse a token.
3. **The cluster administrator and root on the VM are trusted.** They can read Secrets, run as the
   database superuser, push to Git, create pods in exempt namespaces, and run grants under any name.
   Except for rewriting copied audit history, nothing here would notice.
4. **The executor's token is broader than its job.** It can edit and close any issue in `maintenance`.
5. **The audit log records what the gateway says happened.** False entries that chain correctly are
   copied like true ones. Rows since the last copy are unprotected, and the copy is on the host.
6. **Personal data without a fixed pattern,** such as names and postal addresses, reaches the model and
   tickets.
7. **Shown answers can be wrong.** They cite passages the turn found, but nothing checks that those
   passages support them. In the generation eval, four of five leak questions got answers from the
   model's general knowledge, citing unrelated public pages (ADR 0026). The generation gate checks
   that answers cite an expected page, not that they are right.
8. **Provenance of upstream images and models** rests on digests and published hashes, not publishers'
   signatures.
9. **A role removed in Forgejo still reads for up to an hour,** through searches and the confirmation
   page, until the session ends. Execution asks Forgejo again.
10. **A prompt or model change can reach main without the generation gate,** which runs only on
    request (ADR 0028). The gate has been shown to fail one far worse prompt, and how small a change it
    can see is not measured (ADR 0027).
11. **Signed test images, including a gateway without its safety prompt,** sit in the registry and
    pass admission. Running one takes the access that already controls the cluster.
12. **One user can hold the model,** since there are no per-user rate limits.
13. **Cookies on `127.0.0.1` are shared with other local services,** which follows from the lab's
    port-forward access.

## What this does not prove

- **That the list of threats is complete.** It covers the threats found by walking STRIDE over each
  boundary, and the OWASP list over the model's paths. Others may exist.
- **That the tests cover every input.** The compromised-model scenarios, the four planted
  passages, the redaction values and the golden set's 47 questions are the ones written. The
  controls they hit are general, but the tests show only those cases.
- **That the stack is secure for a real deployment.** It is a single-node lab with one administrator,
  test users and public documents. The residual risks above are the minimum a deployment would have to
  close.
