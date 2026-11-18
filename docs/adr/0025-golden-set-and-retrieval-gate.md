# 0025: A golden set, and a retrieval gate against a saved baseline

**Status:** accepted
**Date:** 2026-11-17

## Context

Phase 4 asks for an eval gate that fails a worse change and passes an unchanged one. It starts with a
golden set of 30 to 50 questions, including refusal, role-leak and prompt-injection cases, and fast
deterministic checks first: retrieval recall at k, citations, leaks, injection and tool boundaries.
Generation quality comes after, compared question by question, with a tolerance set from repeated
baseline runs.

ADR 0014 measured recall against an exact scan, which shows the index finds what the embeddings rank
highest. It does not show the embeddings rank the right page highest. That needs questions with
known answers.

## Decision

- **A golden set of 47 questions, `test/eval/golden.json`,** written for this project from the
  titles and headings of the 42 corpus pages, before any result was seen. Each names its role and
  every page that answers it:

  | Kind | Questions | Asked as | What it is for |
  |---|---|---|---|
  | `answer` | 18 | technician | A public page answers it |
  | `answer` | 13 | engineer | A restricted page answers it |
  | `leak` | 5 | technician | A restricted page answers it, so a technician must get none of it |
  | `refusal` | 6 | either | The corpus does not answer it |
  | `injection` | 5 | technician | The question tries to instruct the model. Two also have a page that answers them |

  The refusal and injection cases are for the generation eval, which comes next. Here they only add
  to the leak check.
- **`make eval-retrieval`, a Python program on the host,** reaches the retrieval service in the VM
  through `kubectl port-forward`, as `make measure-retrieval` does. For each question:
  - it searches with the question's role, the default scan and k=5, and records whether an expected
    page is in the top 3, the number the chat asks for, and in the top 5, and the rank of the first
  - it searches as a technician with k=20, and counts any restricted chunk as a leak
  - for leak questions, it also searches as an engineer, as the leak check's control: a leak question
    that finds no restricted text even for an engineer shows nothing about leaks
- **It gates on a saved baseline, question by question.** It fails if any question that had an
  expected page in its top 3 in the baseline does not now, beyond a tolerance; if anything leaked;
  or if a leak question's control found nothing. A question improving is not a failure. The
  baseline is `test/eval/baseline/retrieval.json`.
- **A run is compared only with a baseline of the same questions and the same deployment.** The
  baseline records each question's text, role, kind and expected pages, the images of retrieval,
  ingestion and the embedding server, and the chunk counts by label. If any of these differ, the gate
  fails and says why, since the comparison would mean nothing. A deliberate change saves a new
  baseline with `--save`, which refuses `--baseline` in the same run, so that a run is never
  compared with itself.
- **The tolerance is 0,** because three runs on the same VM gave identical top 5 chunks for every
  question.
- **The gate's comparison has unit tests,** run by `make lint` and so in CI.

## Alternatives

- **Recall at k over every relevant chunk.** It needs every relevant chunk of 507 marked by hand, and
  the chat's answer needs only one good passage. A page-level hit is what the citation check, and a
  reader, can use.
- **An aggregate threshold, such as 90% hit at 3.** A change could lose some questions and gain
  others and still pass. Per question, a loss is a loss.
- **Model-judged relevance.** It adds a model's judgement, which ADR 0020 shows can be steered, to a
  check that does not need one.
- **Questions from real users.** There are none, and inventing them as if they were real would be
  worse than writing them openly for the test.

## Consequences

- **The questions are easy.** They follow the pages' own headings, and 31 of the 33 with expected
  pages found one first, the other two second. So the gate can only notice a change that pushes a page
  out of the top 3. It cannot notice small losses in rank, and it does not say retrieval works for
  questions worded the way a real technician would word them.
- **It searches with the question, not the query the model writes.** The chat's searches are the
  model's own wording. This measures the embeddings and the index, not the model's searching.
- **The baseline is tied to the deployed models, chunks and index.** A deliberate change to any of
  them needs a new baseline, saved with `test/eval/retrieval.py --save`, and the change to the
  baseline is visible in review.
- **Not yet part of `make rebuild`, and a rebuild may change the results.** The HNSW index is built
  afresh on each rebuild, and need not come out the same: `make measure-retrieval` on this VM gave
  slightly different recall than on the VM ADR 0014 measured, with the same images, such as 0.992
  against 0.983 for a technician at k=5. Whether the golden set's hits hold across a rebuild has not
  been tested. That, the CI job, and a deliberately worse change shown to fail, come in a later slice.
  ADR 0026 later found the top 5 chunks unchanged for every question across one rebuild.

### What this does not prove

- **That the gate fails a worse change.** Its failure rules are unit tested, but no worse chunking,
  prompt or model has been run against it yet.
- **That retrieval is good.** It shows that, for these 47 questions, the expected page is found.
- **Anything about answers.** That is the generation eval's job.

## Measurements

On 2026-11-17, against the VM from PR #21's rebuild, with the retrieval, ingestion and embedding
images pinned in `infra/versions.env`, and 314 public and 193 restricted chunks:

- **Hits:** all 20 technician and 13 engineer questions with expected pages had one in their top 3:
  31 first, and 2 second (p08 and p16, each after a chunk of `service.md`).
- **Leaks:** no restricted chunk reached a technician for any of the 47 questions at k=20. As the
  control, the 5 leak questions found 15 to 20 restricted chunks each as an engineer.
- **Repeatability:** three runs gave identical top 5 chunks for every question. Two gate runs against
  the saved baseline passed. A run takes about 5 s.
- **The gate's rules:** breaking each one in turn made a unit test fail: the tolerance on lost hits,
  the leak check, the control, the check that the question ids match, the check that each question
  is unchanged, and the check of the deployment. A live run against a copy of the baseline with one
  restricted chunk fewer failed on the deployment, as it should.
- **The review** found that the first version compared only question ids, so an edited question was
  measured against its old entry, and one with its expected pages removed crashed the gate. It also
  found that `--save` with `--baseline` compared a run with itself, and that a run against another
  deployment was not flagged. Each is now a rule, with a test.
