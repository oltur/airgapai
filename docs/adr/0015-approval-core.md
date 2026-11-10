# 0015: Approval core: proposals, confirmations and the audit log

**Status:** accepted
**Date:** 2026-11-10

## Context

Phase 3 must show that no side effect runs without a valid confirmation from an authenticated user,
and that every decision is in the audit log. The plan binds a confirmation to the user, the tool, a
hash of the canonical arguments and an expiry. It must be consumed atomically, it must never reach the
model, and the role must be re-checked at execution. Tool permissions are set by an administrator,
deny by default, and every change is logged. If the audit write fails, the action fails.

This is the first slice of the gateway: the part that decides. It has no network surface yet. The
login, the confirmation page, the executor that files Forgejo issues, and the model loop that calls
the tools come in later slices and build on it.

## Decision

- **A Go package, `images/gateway/approval`,** over a PostgreSQL schema in `images/gateway/schema.sql`,
  in a database of its own named `gateway`.
- **A model can only propose.** `Propose` checks that the tool exists and that the user's current role
  may use it, validates the arguments, and stores them in canonical form with their SHA-256 and the
  identities and hashes of the chunks they drew on. It returns no token.
- **Canonical arguments:** keys sorted, no insignificant whitespace, numbers as written. Duplicate keys
  are refused, so that the text shown to a person and the arguments executed cannot differ by parser.
  `create_ticket` allows exactly `title`, `body` and `priority`, with limits on each.
- **A person confirms.** `Confirm` checks that the proposal is the user's own, unexpired and unconfirmed,
  and that the role still permits the tool. It returns a random 256-bit token once and stores only its
  hash. It is meant to be called only from the authenticated confirmation session, which a later slice
  builds.
- **Execution checks every binding in one transaction:** the token exists, is unused and unexpired,
  was issued to this user for this tool, the arguments hash to the confirmed hash, and the role permits
  the tool now. It locks the confirmation row, marks it used and writes the audit entry. The executor
  gets back the canonical arguments, and runs those, not its own.
- **A refusal does not use up a confirmation.** Its work is rolled back, and the refusal is logged in a
  transaction of its own.
- **The audit entry is in the same transaction as the decision,** so if it cannot be written, the
  decision is rolled back and the call does not run. The outcome of a call that ran is logged after it,
  separately.
- **A hash chain.** Each audit row's hash covers the previous row's hash and all its own fields. Rows
  are numbered without gaps under one lock, and `Verify` names the first row that does not fit. The log
  holds identifiers and hashes, never document or ticket text.
- **Append only for the gateway.** Its login, `gateway_app`, may insert and read audit rows, and has no
  UPDATE, DELETE or TRUNCATE. It may set only `consumed_at` on a confirmation, and cannot change a
  proposal.
- **Tool permissions** are rows of role and tool. No row means no use. `Grant` and `Revoke` are logged
  as the administrator's decisions.
- **Expiry:** 15 minutes for a proposal to be confirmed, and 5 minutes for a confirmation to be used.

## Alternatives

- **A signed token, such as a JWT, checked without the database.** It cannot be consumed atomically
  without storing it anyway, and revoking a role would not stop it before it expired.
- **Hashing the raw arguments as the model wrote them.** The same request with different spacing or key
  order would then fail, which teaches people to work around the check. Canonical text gives one hash
  per meaning, and is what the confirmation page will show.
- **Logging to a file.** It is outside the transaction, so a decision could commit without its entry,
  which the plan rules out.
- **Locking the proposal row when confirming.** It needs UPDATE on proposals, which the gateway's login
  should not have. The unique proposal column on confirmations stops a second confirmation instead. The
  database tests found this when the narrow grants refused the lock.

## Consequences

- **The confirmation page must show the canonical arguments,** because those are what will run.
- **The role resolver is called at each step,** so its answer must be current. Until the login slice
  exists, it is an interface, and the tests change roles through a fake.
- **The audit lock serialises every decision.** One lock is simple and keeps the chain in order. It
  would limit throughput under load, which this lab does not have.
- **Whoever can write to the database can still rewrite the whole chain consistently.** The chain only
  shows changes against a last hash kept somewhere else, which the off-VM copy in a later slice
  provides. Until then it is tamper-evident only against partial changes, and against the gateway's
  own login, which cannot change rows at all.
- **The outcome entry can be lost** if the process dies after the call ran and before the entry is
  written. The decision entry before it still shows the call was allowed.

### What this does not prove

- **That the user is who they say.** Authentication comes in a later slice. Here the user is an
  argument.
- **That the token stays out of the model's context.** That depends on how the later slices carry it.
  Here it is only returned to the caller of `Confirm`.
- **That a misled user does not confirm a harmful proposal.** That is a residual risk the plan states.
- **That the administrator's grants are authorised.** `Grant` takes the administrator's name; who may
  be one comes with the login slice.

## Measurements

On 2026-11-10, `make test-db` ran the package's tests against a throwaway PostgreSQL 18.6 container
from the pinned pgvector image, with the gateway schema applied, connected as `gateway_app`. All passed:

| Case | Result |
|---|---|
| A confirmed call, arguments in another order and spacing | Ran once and returned the canonical arguments |
| The same token again | Refused: already used |
| Altered arguments, another user, another tool, a forged token, duplicate keys | Each refused, and the genuine call still ran after |
| Expired confirmation | Refused, and still refused for the genuine call |
| Role removed, or permission revoked, after confirmation | Refused, and the genuine call ran once the role or grant was back |
| Proposals for an unknown tool, an extra field, no role, an ungranted role | Each refused |
| Confirming twice, another user's proposal, an unknown or expired proposal | Each refused |
| 20 concurrent executions of one token | 1 ran, 19 refused, and the chain verified |
| Audit insert revoked from the gateway's login | The call failed and the confirmation stayed unused. With the grant back, it ran |
| UPDATE, DELETE and TRUNCATE of the log by the gateway's login | Each refused by the database |
| A superuser changing, removing, rehashing or renumbering a row | Each found by `Verify` |
| Ticket text in the log | None |

Every case above was logged, and the chain verified after each test. As controls, each check was
removed from the code in turn: the argument hash, the used flag, the user, the role re-check, the
expiry and the row lock. Each removal made the test for it fail.
