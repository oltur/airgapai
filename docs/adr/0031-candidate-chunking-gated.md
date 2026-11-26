# 0031: Candidate chunkings, gated beside the deployed index

**Status:** accepted
**Date:** 2026-11-26

## Context

Phase 4 is done when a deliberately worse prompt, chunking change or model fails the gate, and an
unchanged one passes. ADR 0027 showed it for a prompt. The retrieval gate of ADR 0025 compares the
deployed index with its baseline, so a chunking change could only be gated by deploying it: a new
ingest image in `deploy/`, the shared `rag.chunks` table re-embedded, and the gateway answering from
the changed index until it is reverted.

## Decision

- **A chunking candidate is the deployed ingest source with only its chunk sizes changed.**
  `test/eval/candidates/NAME.chunk-sizes` holds `maxChars`, the most characters of a page in a chunk,
  and `minChars`, the size at which a heading starts a new chunk. `infra/build-registry.sh` copies
  `images/ingest`, replaces both with `swap-chunk-sizes.py`, which fails unless it finds each exactly
  once and minChars is below maxChars, and builds it with the pinned corpus, through the same script
  as the deployed image, as the signed test image `ingest-candidate:NAME`, labelled with the file's
  hash. `worse-chunking.chunk-sizes` is 200 and 50, against 1500 and 400 deployed. Both are changed,
  in about the same ratio, because a maxChars below minChars would also turn off the heading rule,
  a second change.
- **It runs into a database of its own.** `test/eval/retrieval-candidate.sh` creates `rag_candidate`
  in the same PostgreSQL and copies the deployed `rag` database's schema into it with `pg_dump
  --schema-only`, so its table, forced row-level security, policies, grants and index are the
  deployed ones by construction, and checks that security is forced and the table is empty. A copy of
  the deployed ingest Job fills it, and a copy of the deployed retrieval service reads it. Each copy
  differs only in its name, `PGDATABASE`, a `component: candidate` label the retrieval Service does
  not select and, for the Job, the candidate's image. Both keep their `app` labels, so the same network
  rules apply. Argo CD does not track them. Everything is removed, and the database dropped, when the
  run ends, passed or not. A run first refuses if another's copies are there, or its port is in use,
  so that it cannot remove the index of a run in progress, and it removes only what it set up. A Job
  that fails ends the run when it fails, not at the 30-minute limit.
- **`test/eval/retrieval.py --candidate NAME` gates it against the saved baseline,** with the same
  questions, rules and tolerance as the deployed index. `deployed` is the control: the deployed ingest
  image into the same fresh database. The deployment is compared with the baseline's, except, for a
  chunking candidate, the ingest image, which must be an `ingest-candidate` image, and the chunk
  counts, which are what it changes. The retrieval and embedding images must still match, so the
  chunking is the only difference measured. The control must match in everything. An image built from
  another file than the host's is refused, as for prompts.
- **`make test-retrieval-gate` runs both:** the control must pass, and the worse chunking's result
  is reported, failing the target only if its run does not complete. It was first written to require
  the worse chunking to fail, which it did not; see the measurements.
  `make eval-retrieval-candidate CANDIDATE=NAME` runs one.
- **The run prints how many questions found the same top 5 chunks as the baseline.** It is not
  gated: an index built again need not rank the same, as ADR 0025 found for HNSW.

## Alternatives

- **Deploying the candidate and gating the deployed index.** It is what `make eval-retrieval` already
  does, but the gateway would answer from the candidate while it ran, and reverting means
  re-embedding.
- **A second table in the same database.** It needs the table's name to be a setting in both Go
  programs and their tests, and copying its policies by hand, where a separate database needs neither:
  both programs already take `PGDATABASE`.
- **`CREATE DATABASE ... TEMPLATE rag`.** It would copy the schema too, but PostgreSQL refuses while
  another session is connected to `rag`, and the retrieval service always is.
- **A worse model instead.** A second chat or embedding model means another model image in the
  bundle and memory in a VM limited to 10 GiB. Chunk sizes are two constants.

## Consequences

- **The retrieval gate does not see chunk size.** It asks whether a chunk from an expected page is
  in the top 3. Chunks of 200 characters still come from the same pages, so the worse chunking kept
  every hit, though none of its top 5 matched the baseline's. What a smaller chunk loses, the text
  around a fact, is what the model answers from, and only the generation gate would see that.
- **A chunking change can be gated without changing what the gateway answers from,** but it passes
  this gate unless it moves pages out of the top 3.
- **The candidate is two settings.** Other chunking changes, such as dropping the title or ignoring
  headings, would need their own swap.
- **A run re-embeds the whole corpus,** about two minutes for the control, so it is not in CI, and not
  in `make rebuild`.
- **The superuser runs the schema copy,** inside the PostgreSQL pod, as the tests already do for
  counts. The candidate's own programs log in with the deployed logins.

### What this does not prove

- **That the worse chunking is harmless.** It passed a gate that cannot see what it changed.
- **That the generation gate would fail it.** It runs only on the deployed index; asking it through a
  copy of the gateway that searches the candidate's index has not been built.
- **That the gate sees a subtler chunking change,** such as 1200 characters instead of 1500.
- **That no two runs overlap.** The check before a run and its set-up are seconds apart, and a run
  started in between is not refused.
- **Anything about the model's answers with the candidate's chunks.** Only retrieval is gated here;
  the generation gate runs on the deployed index.
- **That the copied schema stays the deployed one** after a change to `postgres.yaml`, on a VM not
  rebuilt since: `pg_dump` copies what the database has, not what Git says.

## Measurements

On 2026-11-26, on the VM of the `make rebuild` of `3d70755`, with the images pinned in
`infra/versions.env`:

- **The control passed, twice,** in 138 and 133 s, each with 507 chunks ingested afresh, 314 public
  and 193 restricted, in 129 and 125 s. All 33 questions with expected pages met hit@3, and no
  restricted chunk reached a technician at k=20. The second run, the first to print it, found the same
  top 5 chunks as the baseline for 46 of the 47 questions.
- **The deployed index, gated after,** passed with the same top 5 for all 47, so the copies left it
  as it was.
- **Torn down:** after the first run, no candidate pod or Job was left, and `rag_candidate` did not
  exist.
- **After the `make rebuild` of `834a6ac`** on 2026-11-26, which passed, built in 278 s and tested in
  1007 s, `make test-retrieval-gate` ran in 317 s. The control passed again, with 507 chunks in
  128 s and the same top 5 as the baseline for 46 of 47 questions. **The worse chunking passed the
  gate:** 3136 chunks, 1990 public and 1146 restricted, ingested in 175 s; all 33 questions with
  expected pages met hit@3, 20 of 20 for technicians and 13 of 13 for engineers; no restricted chunk
  reached a technician at k=20; and the same top 5 as the baseline for 0 of the 47 questions. The
  target, then written to require exit 1, failed on that. After the run no candidate Job or
  Deployment was left, and `rag_candidate` did not exist.
- **A dry run of `worse-chunking`,** on the host over the pinned corpus: 3136 chunks, 1990 public and
  1146 restricted, the largest 289 characters with its title. The deployed chunker gave 507, as in
  ADR 0013. With minChars left at 400, it gave 3005, which is how the second change showed.
- **The unit tests** cover the candidate's rules: the control must match the baseline, chunk counts
  included; a chunking candidate may change only the ingest image and the counts; a different
  retrieval image fails as another deployment; a chunking candidate running the deployed ingest image
  is refused; and the baseline is not changed in place.
