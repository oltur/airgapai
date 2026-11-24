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

- **The store reads a proposal's status** (`approval.Store.Status`): whether it was confirmed and
  when, whether the executor used the confirmation, and the last result the executor logged for it in
  the audit log, with the issue and repository for a success. It reads the user's own proposals only.
  No new grant is needed: the gateway's login may already read the audit log, though not change it.
- **The page shows it in place of the button,** once confirmed, and says it cannot be confirmed again:
  - **filed:** a link to the ticket, if the logged repository is a well-formed `org/repo`
  - **not filed:** the executor logged `failed`, which only a certain failure gives (ADR 0029)
  - **result unknown:** the executor logged `unknown`, used the confirmation without logging a result,
    or logged a success that names no ticket. The page gives the same message as ADR 0017's result
    page, naming the repository to check
  - **not used:** the confirmation was not used by the executor, so nothing was filed through it,
    such as after a refusal at execution
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
- **The page trusts the executor's logged result.** It is in the audit chain, written by the
  executor's login, and checked for form before it becomes a link.
- **Each proposal page reads the audit log** for its result, by scanning it, which is fine at a lab's
  size but would need an index on the proposal for a large log.

### What this does not prove

- **The pages in the VM.** The change is in the gateway's and executor's images, which reach the VM
  with the next `make rebuild`.
- **That every outcome is reachable through the real executor.** The test logs each result directly
  with the store, as the executor would, since the test's executor is a fake.

## Measurements

On 2026-11-24, `make test-db` passed, with the gateway's test confirming six proposals and reading
each page: a refusal, so the confirmation was never used; `succeeded` with a ticket; `failed`;
`unknown`; used with no result; and a success whose repository is `../../evil`. Each page said
"You confirmed this at" and "cannot be confirmed again", showed the expected outcome, and had no
"Confirm and file". Dropping the repository check made the last case fail: its page linked
`ticket #9` at `../../evil`. `infra/build-go-image.sh` built the new executor and gateway digests,
now pinned. The executor's digest changes too, since it builds the same `approval` package.
