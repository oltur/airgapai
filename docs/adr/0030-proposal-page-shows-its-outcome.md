# 0030: A confirmed proposal's page shows what became of it

**Status:** accepted
**Date:** 2026-11-24

## Context

After a confirmation, the result page said what happened: filed, refused, not filed, or unknown
(ADRs 0017 and 0029). The proposal's own page did not. It offered "Confirm and file" again, and only
a second try said "already confirmed". A user who closed the result page, or lost it to a timeout,
had no way back to the result except the ticket repository, which a technician cannot read for a
restricted ticket. The threat model listed it as a gap.

## Decision

- **The store reads a proposal's status** (`approval.Store.Status`), in one query: whether it was
  confirmed, when, and until when the confirmation can be used; whether it was used; and the last
  `result` entry for it in the audit log, with the issue and repository for a success. It reads the
  user's own proposals only. No new grant is needed: the gateway's login may already read the audit
  log, though not change it. An index on the proposal a result names keeps the lookup from scanning
  the log.
- **The page shows it in place of the button,** once confirmed, and says it cannot be confirmed again:
  - **filed:** a link to the ticket, if the logged repository is a well-formed `org/repo`
  - **not filed:** the executor logged `failed`, which only a certain failure gives (ADR 0029)
  - **result unknown:** the executor logged `unknown`, used the confirmation without logging a result,
    or logged a success that names no ticket. The page gives the same message as ADR 0017's result
    page, naming the repository to check
  - **not used yet:** the confirmation is unused and has not expired. The executor's request may
    still be on its way after the gateway stopped waiting, so the page says when it expires and to
    reload after then before proposing again
  - **expired unused:** nothing was filed through it, such as after a refusal at execution, and the
    ticket can be proposed anew
- **A status that cannot be read hides the button,** and says so, rather than offering a proposal
  that may have been confirmed.

## Alternatives

- **A column on the proposal for its outcome.** The gateway's login has no UPDATE on proposals, by
  design, and the executor would need one. The audit log already holds the result, with its chain.
- **Showing the result's detail, such as Forgejo's status.** The result page says it once; the
  proposal page says only what matters for what to do next.

## Consequences

- **A user can come back to a proposal and see whether it was filed,** including a technician whose
  ticket went to the restricted repository: the page gives its number and link, which they still
  cannot open.
- **The page trusts the logged result.** It is an audit entry with the actor `executor`, checked for
  form before it becomes a link. The executor and the gateway connect to the database with the same
  login, `gateway_app`, and the actor is a value the writer gives, so anything holding that login
  could append a false result, as ADR 0015 says of the log as a whole. The page shows what was
  logged, not proof of what Forgejo did. Separate logins for the gateway and the executor would let
  the database tell them apart; that is not built.
- **The audit table has a new index,** on the proposal named by each `result` entry. It is in the
  schema the cluster's PostgreSQL creates, so it reaches the VM with the next `make rebuild`.

### What this does not prove

- **The pages in the VM.** The change is in the gateway's and executor's images, which reach the VM
  with the next `make rebuild`.
- **That every outcome is reachable through the real executor.** The test logs each result directly
  with the store, as the executor would, since the test's executor is a fake.

## Measurements

On 2026-11-24, `make test-db` passed, with the gateway's test confirming nine proposals and reading
each page: unused and unexpired; unused and expired; `succeeded` with a ticket; `failed`; `unknown`,
for a public and a restricted proposal; used with no result; a success whose repository is
`../../evil`; and a success with issue 0. Each page said "You confirmed this at" and "cannot be
confirmed again", showed the expected outcome, and had no "Confirm and file". With the audit log
made unreadable to the gateway, a proposal's page offered no confirmation and said its status could
not be read. Dropping the repository check made the `../../evil` case fail, with its page linking
`ticket #9`. Treating an unused, unexpired confirmation as nothing filed made the first case fail. `infra/build-go-image.sh` built the new executor and gateway digests,
now pinned. The executor's digest changes too, since it builds the same `approval` package.
