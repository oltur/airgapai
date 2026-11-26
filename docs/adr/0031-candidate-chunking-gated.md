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

- **A chunking candidate is the deployed ingest source with only its chunk size changed.**
  `test/eval/candidates/NAME.max-chars` holds the size. `infra/build-registry.sh` copies
  `images/ingest`, replaces `maxChars` with `swap-max-chars.py`, which fails unless it finds it
  exactly once, and builds it with the pinned corpus, through the same script as the deployed image,
  as the signed test image `ingest-candidate:NAME`, labelled with the file's hash.
  `worse-chunking.max-chars` is 200, against 1500 deployed.
- **It runs into a database of its own.** `test/eval/retrieval-candidate.sh` creates `rag_candidate`
  in the same PostgreSQL and copies the deployed `rag` database's schema into it with `pg_dump
  --schema-only`, so its table, forced row-level security, policies, grants and index are the
  deployed ones by construction, and checks that security is forced and the table is empty. A copy of
  the deployed ingest Job fills it, and a copy of the deployed retrieval service reads it. Each copy
  differs only in its name, `PGDATABASE`, a `component: candidate` label the retrieval Service does
  not select and, for the Job, the candidate's image. Both keep their `app` labels, so the same network
  rules apply. Argo CD does not track them. Everything is removed, and the database dropped, when the
  run ends, passed or not.
- **`test/eval/retrieval.py --candidate NAME` gates it against the saved baseline,** with the same
  questions, rules and tolerance as the deployed index. `deployed` is the control: the deployed ingest
  image into the same fresh database. The deployment is compared with the baseline's, except, for a
  chunking candidate, the ingest image, which must be an `ingest-candidate` image, and the chunk
  counts, which are what it changes. The retrieval and embedding images must still match, so the
  chunking is the only difference measured. The control must match in everything. An image built from
  another file than the host's is refused, as for prompts.
- **`make test-retrieval-gate` shows both:** the control must pass and the worse chunking must fail
  by its results, exit 1. `make eval-retrieval-candidate CANDIDATE=NAME` runs one.
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
  bundle and memory in a VM limited to 10 GiB. A chunk size changes one constant.

## Consequences

- **Phase 4's deliverable can be shown for a chunking change,** without changing what the gateway
  answers from.
- **The candidate is one setting.** Other chunking changes, such as dropping the title or ignoring
  headings, would need their own swap.
- **A run re-embeds the whole corpus,** about two minutes for the control, so it is not in CI, and not
  in `make rebuild`.
- **The superuser runs the schema copy,** inside the PostgreSQL pod, as the tests already do for
  counts. The candidate's own programs log in with the deployed logins.

### What this does not prove

- **That the gate sees a subtler chunking change,** such as 1200 characters instead of 1500.
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
- **A dry run of `worse-chunking`,** on the host over the pinned corpus: 3005 chunks, 1914 public and
  1091 restricted, the largest 307 characters with its title. The deployed chunker gave 507, as in
  ADR 0013.
- **The unit tests** cover the candidate's rules: the control must match the baseline, chunk counts
  included; a chunking candidate may change only the ingest image and the counts; a different
  retrieval image fails as another deployment; a chunking candidate running the deployed ingest image
  is refused; and the baseline is not changed in place.
