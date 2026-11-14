# 0022: Answers cite the passages found, or are not shown

**Status:** accepted
**Date:** 2026-11-14

## Context

The plan asks that answers cite retrieved chunks or refuse, and that output be validated against a
schema. Until now the model's answer was shown as it came, escaped and marked, but unchecked: an answer
from the model's own knowledge, or one naming a source that does not exist, looked the same as a
grounded one. ADR 0020 showed that planted passages can steer the model, so its prose is not evidence of
anything.

## Decision

- **The gateway checks every answer's citations before showing it.** An answer is shown only if it
  cites at least one passage, and every number it cites is a passage the gateway's own searches
  returned in that turn. Otherwise the model's reply is withheld entirely, and the gateway says why:
  - the reply cites no passage, so the manuals may not answer the question, or the model answered
    without them; or
  - it cites a passage the turn did not find, so it named a source it was not given.

  The page lists the passages a shown answer cites. A withheld reply is logged with its reason, never
  its text.
- **What counts as a citation:**
  - the forms the prompt asks for, `[1]` and `[1, 3]`
  - forms models also write: ranges, `[1-2]` and `[1–3]`, and `[source 1]` or `[Source: 2]`
  - only numbers of at most four digits, so no long string of the model's reaches the page in a
    refusal
  - not inside code, fenced or inline, and not straight after a name, a dot or another bracket, so
    `{.items[0]}` and `list[2]` are indexes, not citations
- **A refusal is the gateway's, not the model's.** A model that says "the manuals do not answer this"
  cites nothing, so its reply is withheld the same way. The gateway's message covers both readings:
  the manuals may not answer the question, or the model answered without them.
- **No retry.** A turn already takes most of a minute on CPU.
- **The system prompt says an answer without a citation is not shown.** This gives the model a reason
  to cite. It is not what enforces it.
- **The stub gets two scenarios:** `uncited`, an answer from its own knowledge, and `fakecite`, an
  answer citing passage 9 after a search that found 3. Its `lie` scenario now searches first and cites
  a passage, so its markup still reaches the page and its escaping is still tested.

## Alternatives

- **The answer as a tool call, `answer(text, citations)` or `cannot_answer(reason)`, with
  `tool_choice: "required"`.** This is the schema the plan names, and it was tried against the real
  model before this decision:
  - **Never finished:** with all tools offered, the model kept searching on all three questions that
    were not ticket requests, and gave no answer.
  - **Citations not marked in the text:** with search withdrawn after two calls, it listed citations
    but never put them in the text. On the Secrets question, it failed the check three times and ran
    out of steps.
  - **Unasked tickets:** forced to call a tool, it called `create_ticket` for two questions that asked
    for none, including "What is the capital of France?".
  - **Slow:** across both runs, turns took 35 to 152 s.

  Making the answer a tool call made this model's behaviour worse in every way that mattered here.
- **JSON output with a schema** (`response_format`) for the final reply. llama.cpp can constrain output
  to a grammar, but the final reply comes from the same call that offers the tools, and a schema there
  would constrain the tool calls too. It was not tried.
- **A second call that only checks grounding.** This would add a model's judgement, which ADR 0020
  shows can be steered, and double the time.

## Consequences

- **The check is about the citations' form and range, not their truth.** A shown answer cites real
  passages of this turn, but nothing checks that those passages say what the answer claims. That is
  what Phase 4's evals measure.
- **Correct uncited answers are withheld.** A model that answers well without marking its sources gets
  the refusal. The prompt asks it to cite, and in the replays below it did.
- **Answers to questions the manuals do not cover always end as the gateway's refusal,** whatever the
  model writes.
- **A YAML or JSON list written outside code is read as citations.** `ports: [80, 443]` in plain text
  cites passages 80 and 443, which do not exist, so a correct answer written that way is withheld. In
  backticks or a code block, it is not read.
- **Two citations written together, `[1][2]`, count as one,** because a bracket after a bracket is read
  as an index.

### What this does not prove

- **That shown answers are right, or that they use what they cite.** See above.
- **That the model cites often enough to be useful.** That is measured on the evals' questions, not
  here.

## Measurements

On 2026-11-14, by script against the real model and retrieval service in the VM, with the gateway's
system prompt and tools at the time:

- **The `answer`/`cannot_answer` variant:** the results above.
- **Plain answers, checked as the gateway now checks them:** two questions answered in full, about
  Secrets and RBAC, and about CPU and memory limits. Both cited `[1, 2]` of 3 passages, and so would be
  shown. The run ended with a dropped port-forward on the third question. These turns took 118 and
  149 s, slower than before, possibly because the model was still busy with an aborted earlier
  request.

`make test-db` passed with the new tests:

- These answers were shown: `[1]`, `[2] and [1]`, `[1,2]`, the ranges `[1-3]` and `[2–3]`,
  `[source 2]` with `[Source: 1]`, and a citation at the very start. So were answers citing `[1]`
  alongside a jsonpath with `items[0]` in backticks, a YAML block with `[80, 443]`, and the
  indexes `items[0]` and `list[2][3]` in plain text.
- These were withheld: no citation; a citation of 0, of 4 with 3 passages, or of 1 with none; a real
  citation alongside a made-up one; brackets around something that is not a number, or around a
  five-digit number; a backwards range; a range past the last passage; and `[80, 443]` in plain text.
- In a turn, the page gave the whole sentence for each reason, and showed none of the withheld text.
- In a turn, an uncited answer and one citing passage 7 of 2 were withheld, with none of their text
  on the page. A cited answer's citations were listed.

Each rule was broken in turn, and a test failed every time: the range check, the need for at least
one citation, withholding an answer that fails, and after the review, ignoring code, ignoring a
bracket after a name, the four-digit limit, and the order of a range. The first version wrote the
reason into the sentence as "the model's reply it cites…". The test had matched only part of it, and
now matches the whole sentence.

The VM results follow.
