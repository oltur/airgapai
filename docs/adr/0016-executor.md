# 0016: The executor, and Forgejo as the directory and ticket system

**Status:** accepted
**Date:** 2026-11-10

## Context

ADR 0015 built the decision: a call runs only with a matching, unused confirmation and a role that
still permits it. Something must then do the side effect, and the plan says only that part holds the
ticket credential. It also says tickets are real Forgejo issues, and that the role is re-checked at
execution, so the roles need a source the executor can ask at that moment. Phase 3's login will use
Forgejo as its identity provider, so its teams are the natural source.

## Decision

- **A separate service, `executor`, in its own `gateway` namespace,** built from `images/executor` with
  the approval core. `POST /execute` takes a token, a user, a tool and arguments, calls
  `approval.Execute`, and only if that allows it, files an issue from the canonical arguments it gets
  back, then logs the outcome. A refusal is 403 and files nothing.
- **Roles come from Forgejo teams, asked at every decision:** `engineers` means engineer and
  `technicians` means technician, in a private `maintenance` organisation. Leaving a team takes effect
  on the next decision.
- **Tickets are issues in the private `maintenance/tickets` repository.** The body is the confirmed body,
  then the priority, the user and the proposal it came from.
- **The executor's Forgejo account owns that organisation only, with a token scoped to `write:issue`
  and `read:organization`.** Owner is the least that lets a member check team membership. A local
  Forgejo from the pinned image showed what the token can and cannot do; see Measurements.
- **The gateway database lives in the same PostgreSQL server as the chunks, as a separate database,**
  created by a second init script in the same ConfigMap. Its schema moved there from
  `images/gateway/schema.sql`, and `make test-db` reads both init scripts from the ConfigMap, so the
  tests get exactly the cluster's schema.
- **Secrets:** bootstrap generates the gateway database password with the others, creates the
  executor's Forgejo account and token, and puts both in the `executor` Secret in `gateway`.
- **Network:** the executor reaches PostgreSQL on 5432 and Forgejo on 3000. Nothing may call it yet; the
  gateway gets its own rule.
- **Administrator commands through `kubectl exec`:** `grant`, `revoke` and `verify`. Until the gateway
  and its confirmation page exist, `propose` and `confirm` stand in for them, so the test can make a
  real confirmation. Using them needs `kubectl exec` into the executor, which already means control of
  the cluster.

## Alternatives

- **Roles in the gateway database.** No identity source would back them, and the login in the next slice
  would need a second source anyway.
- **The bootstrap administrator's token for the executor.** It can do anything in Forgejo. An owner of
  one organisation, with a narrow token, cannot change teams, repositories or other organisations.
- **A team member that is not an owner.** Forgejo refused it the membership check (403).
- **A separate PostgreSQL for the gateway.** It isolates the data better, at the cost of another server,
  volume and password set in a single-node lab. The two databases share only the server; the gateway's
  login has no access to the chunks, and the readers none to the gateway.

## Consequences

- **The token can still edit and close any issue in `maintenance`.** Its scope is the issue API, not
  just creating issues.
- **Forgejo must be reachable for every decision.** If it is not, the decision fails and is logged as
  failed, and nothing runs.
- **Team names are the role mapping.** Renaming a team removes the role.
- **`propose` and `confirm` in the executor are temporary.** They go when the gateway and its
  confirmation page take over, and the threat model must list them until then.
- **Restricted text in a ticket.** A ticket is visible to everyone in both teams. Stopping restricted
  text from reaching it is a later slice, and this one does not try.

### What this does not prove

- **That the user is authenticated.** The user is a field in the request, and only the confirmation
  binds it. The login comes next.
- **That only the executor can file issues.** The administrator and the organisation's other owners can
  too. The claim is that the gateway's path to a ticket goes through the executor and a confirmation.
- **Expiry in the cluster.** The 5-minute expiry is tested in `make test-db`, not in the VM.

## Measurements

On 2026-11-10, against a local Forgejo from the pinned image, with the teams and the executor set up
as bootstrap does. With the executor in a non-owner team, team membership checks returned 403. As an
owner with the narrow token:

| Request | Status |
|---|---|
| Is alice in technicians, in engineers; is bob in engineers | 200, 404, 200 |
| File an issue in `maintenance/tickets` | 201 |
| Delete the repository, create one, edit the organisation | 403 each |
| Add a team member, change a team's permission | 403 each |
| List the site's users | 403 |
| File an issue in another user's repository | 404 |

The executor binary then ran against that Forgejo and a PostgreSQL container with the cluster's init
scripts. Grant, propose and confirm worked. Carol, with no team, and bob, an engineer with no grant,
could not propose. Bob was refused alice's confirmation, alice's call filed one issue, and its replay
was refused. The audit chain verified.

`make test-db` ran the executor's tests against a fake Forgejo and passed. Five calls without a valid
confirmation got 403 and filed nothing. The confirmed call filed one issue with the confirmed text.
Leaving the team after confirming was refused. When Forgejo failed, the token was still used up, the
failure was logged, and a retry was refused.

In the VM: not yet measured.
