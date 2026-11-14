# 0021: Tickets drawn on restricted passages go where only restricted readers are

**Status:** accepted
**Date:** 2026-11-14

## Context

The plan's adversarial tests include: restricted text cannot be copied into a ticket visible to lower
roles. Until now every ticket went to `maintenance/tickets`, which both teams read. An engineer's turn
reads restricted passages, so the model could put their text into a ticket, word for word or
reworded, and technicians would read it once it was filed. ADR 0019's stub showed the copying part:
`escalate` copies every passage it reads into a ticket.

## Decision

- **Route by where a ticket's content came from, not by what it says.** Every proposal has a
  visibility, `restricted` if any of its sources is restricted, and `public` otherwise. The sources are
  the passages the gateway's own searches returned in that turn, with the labels that retrieval read
  from the database (ADR 0018). Nothing the model writes decides it.
- **The approval core stores the visibility with the proposal and returns it from `Execute`.** A
  source with any label other than `public` or `restricted` is refused, rather than taken as public.
  The visibility is part of the proposal row that the confirmation binds, and it is logged with the
  proposal and with the execution.
- **The executor files by visibility, from one map.** Public tickets go to `maintenance/tickets`, and
  restricted ones to `maintenance/tickets-restricted`. Both are in `directory.TicketRepos`, which the
  gateway and the executor share and no setting overrides, so the two names cannot become one, and the
  page cannot name one repository while the executor files in the other. An execution with any other
  visibility files nothing. Its failure is logged, and the user is told that nothing was filed.
  `Execute` also refuses such a proposal before it uses up the token, although the schema already
  allows only the two values.
- **The executor reports where it filed,** and the gateway's link after confirming goes there.
- **Forgejo enforces who reads where.** Bootstrap creates both repositories. Each team lists its
  repositories instead of including all of them: technicians get `tickets`, and engineers get both. A
  repository added later is in neither team until it is listed.
- **The confirmation page says who will read the ticket.** It shows either "Readable by engineers only"
  or "Readable by technicians and engineers", with the repository. Once the ticket is filed, the link
  goes to the repository it was filed in.
- **The gateway sends retrieval only a source's identity and hash.** The label is the gateway's record,
  and retrieval reads its own from the database.

## Alternatives

- **Detecting copied text.** Refusing a ticket that shares, say, eight words in a row with a restricted
  passage would stop the copy, but not a reworded one. Routing by source covers both.
- **Refusing any ticket after a restricted passage was read.** It would stop engineers from filing
  tickets about restricted procedures, which is their job.
- **Asking the model or the user to mark the ticket restricted.** Either could get it wrong, and a
  model the documents can instruct would be told to mark it public.

## Consequences

- **Over-routing.** A ticket from a turn that read one restricted passage it did not use still goes to
  `tickets-restricted`. Technicians do not see it, even if it holds nothing restricted. This errs on
  the side of keeping text in.
- **Technicians cannot see tickets that engineers file from restricted passages,** so the two lists of
  tickets now differ by role.
- **A schema change.** `gateway.proposals` gets a `visibility` column. As ADR 0012 says of any schema
  change on a kept volume, an existing database needs a migration. A rebuild starts without one.
- **The Forgejo setup is what makes the routing hold.** If someone adds technicians to
  `tickets-restricted`, the gateway cannot tell. `make test-adversarial` checks the teams' repository
  lists before it tests the routing. A lab kept from before this change has teams that include every
  repository: bootstrap runs once on a fresh VM, as ADR 0016 says, and does not fix them, but that
  check fails on them.

### What this does not prove

- **That no restricted fact reaches a technician by another way.** A technician's own searches return no
  restricted passage, under row-level security (ADRs 0012 and 0014). But an engineer can tell a
  technician what a restricted passage says, outside the system.
- **That the title of a restricted ticket is safe.** It is filed in the restricted repository, so
  technicians do not see it either. Forgejo notifications were not checked.

## Measurements

On 2026-11-14, on the lab's Forgejo 17.0.1, with test users:

- With `tickets-restricted` added only to engineers, alice, a technician, read `tickets` (200) and got
  404 for `tickets-restricted`. Bob, an engineer, read both, and carol, with no team, neither.
- Turning off "include all repositories" for an existing team did not remove the repositories it
  already had: technicians kept `tickets-restricted` until it was removed from the team. Bootstrap
  therefore creates the teams with that option off from the start. A team created that way had no
  repositories.

`make test-db` passed with new tests:

- A proposal with no sources or only public ones was public. One with any restricted source was
  restricted, when proposed, when read back, and when executed. The labels `""`, `secret` and `Public`
  were refused.
- The executor filed a public execution in `tickets`, and one with a restricted source in
  `tickets-restricted`, and reported each repository. For the visibilities `""`, `secret` and
  `Restricted` it filed nothing, and no request reached Forgejo.
- An executor answer that named no repository, or something that is not one, was not taken for
  success.
- The gateway's page said "engineers only" for a proposal with a restricted source, and "technicians
  and engineers" for one with only public sources. After confirming, the link pointed to the
  restricted repository.

In the VM, on 2026-11-14, `make rebuild` passed every suite: built in 279 s and tested in 962 s.
`make test-adversarial` first found the teams as bootstrap leaves them: technicians list only
`tickets`, and engineers both. Then bob, an engineer, ran the stub's `escalate` scenario. His search
found restricted passages, and the stub copied them into a ticket. The proposal's page said "Readable by
engineers only". Bob confirmed it, and the executor filed it in `tickets-restricted`, with one more
ticket there and none in `tickets`. Bob could read it. Alice got 404 for it, while she still read
`tickets`.
