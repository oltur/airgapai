# 0029: The executor says when nothing was filed

**Status:** accepted
**Date:** 2026-11-24

## Context

When a confirmed ticket was not filed, the executor answered the gateway the same way, `500
execution failed`, whether Forgejo had refused the create call or its answer was lost. The gateway
could only say "Result unknown": the ticket may have been filed, check the repository before
proposing it again (ADR 0017). That is right when the answer was lost, and the threat model listed the
rest as a gap. When Forgejo refused, the user was sent to look for a ticket that cannot exist, and a
technician whose ticket would be restricted was told to ask an engineer to look.

ADR 0016 logged a Forgejo refusal as `failed` and a lost answer as `unknown`, but counted any status
other than 201 as a refusal. A 5xx can come after Forgejo has written the issue, so it does not show
the issue does not exist.

## Decision

- **The executor answers `502` with `{"failed": REASON}` only when nothing was filed, for certain:**
  - the confirmation could not be checked, so no call reached Forgejo
  - the stored arguments could not be read, so no call reached Forgejo
  - Forgejo answered the create call with a 4xx, a request it rejected
- **A 5xx from Forgejo is an unknown result,** logged as `unknown` with its status, and answered as
  before: `500 execution failed`. So is no answer, or a 201 whose body cannot be read.
- **The gateway shows "Not filed"** for a `502` with a `failed` reason: the reason, that nothing was
  filed, and that the proposal has to be proposed anew, since the token was used. It accepts only the
  executor's own reasons, with a 4xx status in Forgejo's; any other text is an unknown result, so a
  wrong or forged answer cannot make the page say more than it knows. Any other answer
  that is not a result or a refusal stays "Result unknown", as ADR 0017 has it.
- **A refusal before execution stays `403 denied`,** as before.

## Alternatives

- **Every refusal from Forgejo counted as certain,** as ADR 0016 logged it. Simpler, but a 5xx after
  the write would tell a user that a filed ticket was not filed, and they would file it again.
- **The executor checking Forgejo for the issue after a lost answer.** It would turn some unknown
  results into known ones, but the search needs a marker in the issue to look for, and can itself
  fail. Not needed for the case this fixes.
- **Retrying the create call.** A retry after a lost answer can file the ticket twice. The
  confirmation is single-use so that a call runs once.

## Consequences

- **A user told "Not filed" can propose again without looking first.** Only the executor's
  certainty lets the page say it, and only these causes give it.
- **Fewer outcomes are logged as `failed`:** a Forgejo 5xx is now `unknown`, with its status.
- **The generation gate's baseline names the old gateway image,** so until it is recorded again, the
  generation gate fails every run, including the control in `make rebuild EVALS=generation`, on the
  changed deployment (ADR 0026), and exits 2. The change does not touch the chat loop, so the answers
  should be unchanged. That has to be shown by running it, after the next `make rebuild`, before a
  new baseline is saved. `make rebuild` without `EVALS` does not check the gateway image. It was
  shown: after ADR 0030, the gate exited 2 on the gateway image alone, and the baseline saved again
  was the same as before but for the turns' times (ADR 0026).
- **The gateway trusts the executor's `failed`,** as it trusts its `denied` and its results. Both are
  inside the cluster's network rules, and neither has TLS (residual risk 2).

- **ADR 0030 changes the gateway image again,** and the same holds for its baseline.

### What this does not prove

- **The stored-arguments cause, in a test.** The arguments reach that step only after they are
  canonicalised and checked against the proposal's hash, so no test makes them fail to read there.

- **That Forgejo never writes an issue before answering 4xx.** It is how its API is meant to work, and
  not tested against Forgejo itself: the tests use a fake Forgejo.
- **The "Not filed" page in the VM.** The change was deployed by the `make rebuild` of `3d70755` on
  2026-11-24, which passed, but no VM test makes Forgejo refuse, so that page has not been seen there.

## Measurements

On 2026-11-24, `make test-db` passed, with:

- the executor's test: a check that cannot be made, for a cancelled request, is not filed for
  certain; a 422 from Forgejo gives `502` with `Forgejo refused the ticket (422)` and the
  confirmation used up; a 500 gives `500` with no `failed`; the results logged are
  `succeeded,failed,unknown,succeeded,succeeded`
- the gateway's test: `502` with a `failed` reason shows "Not filed" and not "may have been filed";
  `502` without one, `502` with any other reason or a 5xx in it, and `500` with one, show "Result
  unknown"
- breaking the rule, so that every Forgejo refusal counted as certain, made the executor's test fail
  on the 500
- `infra/build-go-image.sh` built main's executor and gateway sources to their pinned digests, and
  this change's to the new ones now pinned in `infra/versions.env` and `deploy/gateway/`
