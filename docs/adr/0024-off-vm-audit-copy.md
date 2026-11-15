# 0024: A copy of the audit log kept off the VM

**Status:** accepted
**Date:** 2026-11-15

## Context

ADR 0015 made the audit log a hash chain that the gateway's login can only append to. It also stated
the chain's limit: anyone with the database superuser, or root on the VM, can rewrite an old row and
recompute every hash after it, and the chain still verifies. A chain shows tampering only if its later
state is kept somewhere the database cannot reach. The plan asks for an off-VM copy, described as
tamper-evident only.

## Decision

- **`infra/auditcopy`, a program that runs on the host,** copies the VM's audit rows to a file under
  `DATA_DIR/audit`, outside the VM. On each run, as `make audit-copy`, it:
  1. reads every row from the VM, over SSH, as the database superuser inside a read-only transaction
  2. checks that every row already copied is still in the VM exactly as copied
  3. checks each new row: its sequence number follows on, it links to the previous row's hash, and its
     own hash matches its fields, computed by the approval core's own code, now exported as
     `approval.RowHash`
  4. appends the new rows to the copy and syncs it to disk
  5. prints the number of rows and the last hash

  If any check fails, nothing is appended, and the program fails, naming the row.
- **One copy per database.** The file is named after PostgreSQL's system identifier, which every new
  database gets. A rebuilt VM, with a new audit log starting from its first row, gets its own copy,
  and the old copy stays as the record of the old one.
- **No separate read-only login.** The copier reaches the database as root on the VM, through SSH, so
  it holds more power there than any login it could use. A read-only role would add nothing against
  an attacker. The read-only transaction guards against the copier itself writing.
- **`make rebuild` copies each run's log at the end,** after every test, and runs `make
  test-audit-copy` before that.
- **`make test-audit-copy` plays the attacker.** It copies the log, then copies two new honest rows. It
  then rewrites row 2 as the superuser, with every hash after it recomputed by the same code, so the
  VM's own check still passes. It checks that the copy catches the change. Then it puts the rows back
  from the copy, and checks that the VM's log verifies and matches the copy again. If the test stops
  partway, its exit trap puts the rows back too. Its copies go to a temporary directory, not the lab's.
  The program's `forge` and `restore` commands exist for this test.

## Alternatives

- **Keeping only the last hash off the VM.** It catches a rewrite, but cannot say which row changed,
  and gives no record to compare against. Every row is small, and there are few.
- **Pushing rows out from the VM as they are written.** The VM has no route out, by design (ADR 0004).
  The host pulls instead, over the SSH path it already uses for every test.
- **A write-once store, such as object storage with retention.** It is the real answer for a
  deployment. The lab has none, and the host's disk is the off-VM place it has.

## Consequences

- **Only what has been copied is protected.** Rows written since the last copy can be rewritten
  without detection. How long that is depends on how often `make audit-copy` runs. It runs when it is
  called, and at the end of a rebuild. A timer on the host would shorten the gap, and is not set up.
- **The copy is on the host.** Someone with root on the host can change the copy too. The point is that
  the VM, and so a compromised gateway or database, cannot reach it.
- **False entries can still be appended.** The gateway's login can add rows, and a new row that chains
  correctly is copied like any other. The copy shows that history was not changed afterwards, not that
  each entry was true when it was written.
- **The test changes the lab's audit log while it runs,** and puts it back. If both the test and its
  exit trap are stopped, for example by the host losing power, the rewritten rows stay, and the copy
  shows where.
- **The approval core exports two names**, `RowHash` and `Genesis`, for the copier, and the gateway's
  and the executor's images changed with them.

### What this does not prove

- **That the log is tamper-proof.** It is tamper-evident for rows already copied, against anyone who
  cannot reach the host.
- **That entries are true.** See above.

## Measurements

On 2026-11-15, against the VM from ADR 0023's rebuild:

- **The copier's tests:**
  - honest growth was copied
  - a first copy took every row
  - a rewritten row with every hash after it recomputed was caught at that row, after a check that
    the forged chain verified on its own
  - rows removed from the end, a new row that did not chain, one whose hash did not match its fields,
    and a gap in the sequence were each refused
- **Mutation checks:** breaking each check in turn made a test fail: comparing copied rows, the
  removal check, the chain link, the hash and the sequence. A first version of the tests' own forger
  assigned the previous hash and the new hash in one statement, so the hash was computed before the
  link changed, and the forged chain did not verify. The test's check that the forged chain verifies
  caught it.
- **`make test-audit-copy`:**
  - the first copy took 34 rows
  - two new rows, from a grant and a revoke, were copied, and a run with nothing new copied 0
  - after row 2 was rewritten with the chain recomputed, the VM's own check still found no bad row,
    and the copy failed with "row 2 in the VM differs from the copy: it was changed after it was
    copied"
  - after the rows were put back, the VM's chain verified and matched the copy again
