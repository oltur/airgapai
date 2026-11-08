# 0012: PostgreSQL, pgvector and row-level security for retrieval

**Status:** accepted
**Date:** 2026-11-07

## Context

Phase 2 must show that the same question under two roles retrieves different chunks, that a
restricted chunk does not reach the lower role, and that a request with no role returns nothing. The
plan names the mechanism: pgvector 0.8 or later, forced row-level security, a login that is not the
owner and cannot bypass it, and the role set per transaction with `SET LOCAL`.

The classification labels are synthetic. The corpus is public documentation, so nothing in it is
secret; the labels exist to test the mechanism.

## Decision

- **PostgreSQL 18.6 with pgvector 0.8.7,** from the pgvector project's image, pinned by its amd64 digest
  and signed into the bundle like the other outside images. Both are under the PostgreSQL licence.
- **One table, `rag.chunks`,** with a `classification` of `public` or `restricted`, a 768-dimension
  embedding and an HNSW index for cosine distance. Its owner, `airgapai_owner`, cannot log in, and
  row-level security is enabled and forced, so it binds the owner too.
- **Two logins, neither the owner and neither able to bypass row-level security:**
  - `ingest` writes, through a policy that admits every row.
  - `retrieval` reads. It is a member of the reader roles `technician` and `engineer` without inheriting
    them, so it can switch to one, but gets none of their rows without switching. With no policy of its
    own, it sees nothing.
- **Readers switch with `SET LOCAL ROLE`,** which ends with the transaction. `technician` sees `public`
  rows, and `engineer` sees both.
- **Passwords on every connection.** The image's default trusts loopback connections, which a check
  showed let a wrong password in. The database is initialised with SCRAM for local and host
  connections instead.
- **Passwords exist only in the cluster.** Bootstrap generates them into a Secret, and the init script
  reads them from the pod's environment.
- **Data on a static local volume,** as Forgejo's is. The schema is created once, when the volume is
  empty.
- **No network path to the database yet.** The default deny applies, and the ingestion Job and
  retrieval service get their own allow rules.

## Alternatives

- **A clearance setting read by the policies,** such as `current_setting('app.clearance')`. Any client
  can set any custom setting, so the login could raise its own clearance. Role membership is checked by
  PostgreSQL itself.
- **One login per reader role.** It has the same effect, but a request with no role would then need a
  separate login, or a default, which is what the plan rules out.
- **Inherited role membership.** With inheritance, the reader roles' policies apply without switching,
  so an unset role would see `public` rows.
- **`hostPath` for the data.** Restricted Pod Security forbids it. A local PersistentVolume is allowed.

## Consequences

- **The mechanism trusts the retrieval service to pick the role.** The login can switch to either
  reader role. Mapping an authenticated user to a role is the retrieval service's job, and later the
  gateway's, and this ADR does not test that.
- **Queries must run in a transaction** for the role to apply. Outside one, `SET LOCAL` has no effect,
  so the query sees nothing. That is the safe way to fail.
- **A schema change on an existing volume needs a migration,** because the init script runs only on
  an empty volume. A rebuild starts empty.
- **`make test-rls` is part of `make rebuild`.**

### What this does not prove

- **That retrieval results respect the roles.** This tests the table under direct SQL. The retrieval
  service, its queries and HNSW's iterative scans are tested when they exist, with recall for each role.
- **That superusers or the database itself are contained.** The `postgres` superuser can read everything
  and turn security off. Its password is in the cluster's Secret, and anyone who can exec into the pod
  or read Secrets in `retrieval` has it.
- **That no other role can read the table.** The test checks the named roles, not every role.
- **The labels are synthetic,** and two levels do not show how this scales to many.

## Measurements

`test/rls.sh` was run on 2026-11-07 against a local container started from the same image, schema and
settings as the manifest, with `kubectl exec` replaced by `docker exec`. All 26 checks passed. That
run found two bugs before the cluster saw them: revoking access to the `public` schema hid pgvector's
type from the logins, and loopback connections were trusted.

In the VM, on 2026-11-08, `make rebuild` passed every suite, including all 26 checks of `make test-rls`:
built in 255 s and tested in 535 s. `make test-rls` then passed twice more on the same cluster, which
shows its test rows are cleaned up. With PostgreSQL and both model servers running, the guest had
4613 MiB available.
