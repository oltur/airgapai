# 0014: Retrieval service

**Status:** accepted
**Date:** 2026-11-10

## Context

Phase 2 is done when the same question under two roles retrieves different chunks, a restricted
chunk does not reach the lower role under the tested mechanism, a request with no role returns
nothing, and recall for each role is measured. ADR 0012 put the decision in the database, with one
limit the database cannot remove: a plain `SET ROLE` lasts for the session, so a pooled connection
could carry one request's role into the next. ADR 0013 filled the table. The service sits between,
and the Phase 3 gateway will wrap it.

## Decision

- **A small Go service, `POST /search`,** taking a query, a role, a result count `k` from 1 to 20 and a
  scan. It embeds the query with Nomic's `search_query: ` prefix and returns the nearest chunks with
  their path, label and distance. It connects as the `retrieval` login from ADR 0012.
- **One transaction per request,** with the role set by `SET LOCAL ROLE`. The two role statements are
  constants, picked from the role name, so no input reaches the SQL. An unknown role, including a
  different case, is refused with 400.
- **No role means no role statement.** The login's own privileges apply, and row-level security
  returns nothing. The service does not special-case it.
- **A guard against a leftover role.** Each transaction first checks that the current user is the
  login. If not, the request fails and the connection is closed, so the pool does not reuse it.
- **A test that the program sends no plain `SET ROLE`.** It parses the source and checks every string
  literal, with a control showing the pattern catches `SET ROLE` and passes `SET LOCAL ROLE`.
- **Iterative HNSW scans by default** (`relaxed_order`), with the results sorted again after the limit.
  Two other scans exist to measure against: the index without iterative scans, and an exact scan.
- **The index scans turn sequential scans off.** On 507 rows the planner chose a sequential scan, which
  is exact, so every scan gave recall 1.0 and nothing about the index was measured. A test checks the
  plan of each scan.
- **The caller names the role.** Nothing here authenticates the caller. Mapping a user to a role is the
  gateway's job in Phase 3.
- **Network:** the service reaches PostgreSQL and the embedding server through the rules from ADR
  0013, now matching `app=retrieval` too. Nothing may call it yet. The gateway gets its own rule.
- **Built like the ingester,** by a shared `infra/build-go-image.sh`, reproducible and pinned.

## Alternatives

- **A plain `SET ROLE` and `RESET ROLE` around each request.** It works until an error or a cancelled
  request skips the reset, and then the next request runs with the old role.
- **One pool per role.** The role would be a login, so it could not be unset, which the plan needs.
- **The default planner on this small table.** It is exact and fast here, but it would leave the index
  untested until the corpus grew, and the role filter's effect on the index is what needs measuring.
- **Strict ordering for iterative scans.** `relaxed_order` with a sort after the limit gives the same
  final order, and pgvector recommends it.

## Consequences

- **A request with no role costs as much as one with a role,** and returns an empty list, not an error.
  A caller cannot tell "no role" from "nothing matched" by the status alone.
- **The scan option is open to every caller.** An exact scan reads every row the role can see. That is
  cheap at 507 rows. The gateway should not pass it through.
- **Turning sequential scans off is a setting for this corpus size.** On a larger table the planner
  would choose the index itself, and the setting should then be reviewed.
- **The guard costs one round trip per request,** and stops only a role left on the connection. It
  does not check the role that the request sets.

### What this does not prove

- **That the right role is chosen for a user.** The caller names it.
- **That retrieval answers questions well.** Recall here compares the index with an exact scan over the
  same embeddings, not with what a reader needed. The queries were written for this project.
- **That a restricted chunk can never reach a technician.** The leak test covers 24 queries, three
  scans and the largest k, through this service and these policies. Other paths to the table are
  covered by ADR 0012's tests, not this one.
- **Latency under load.** Requests were sequential, and include embedding the query.

## Measurements

On 2026-11-10 on the host, against a PostgreSQL container started from the same image and schema with
the corpus ingested, and the Nomic model under llama.cpp `b12032` with the cluster's settings. Mean
recall over 24 queries, then the lowest, then how many queries returned fewer than k chunks:

| Role, k | HNSW, iterative | HNSW, not iterative | Exact |
|---|---|---|---|
| Technician, 5 | 0.983, 0.8, 0 | 0.900, 0.0, 3 | 1.0, 1.0, 0 |
| Technician, 10 | 0.988, 0.9, 0 | 0.846, 0.0, 6 | 1.0, 1.0, 0 |
| Technician, 20 | 1.000, 1.0, 0 | 0.744, 0.0, 12 | 1.0, 1.0, 0 |
| Engineer, 5 and 10 | 1.000, 1.0, 0 | 1.000, 1.0, 0 | 1.0, 1.0, 0 |
| Engineer, 20 | 0.996, 0.95, 0 | 0.996, 0.95, 0 | 1.0, 1.0, 0 |

Without iterative scans, the technician's policy discards restricted rows after the index has
already chosen its candidates, so some queries got fewer chunks than asked for, and some none. The
engineer, whose policy discards nothing, was not affected. The two roles' top five differed for 16
of the 24 queries. Median latency was 24 to 26 ms for every scan, including the query's embedding.

In the VM: not yet measured.
