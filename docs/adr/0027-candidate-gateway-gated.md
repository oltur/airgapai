# 0027: Candidate gateways, gated beside the deployed one

**Status:** accepted
**Date:** 2026-11-20

## Context

ADR 0026 gates the deployed gateway's answers against a saved baseline, but it had not shown that the
gate fails anything. Phase 4 is done when a deliberately worse prompt, chunking change or model fails
the gate, and an unchanged run passes. A worse prompt changes the gateway's image. Run as the
deployed gateway, it would fail ADR 0026's gate on the changed deployment alone, which shows nothing
about its answers. Deploying it also takes a rebuild, since the registry in the VM is read-only.

## Decision

- **A candidate runs as a copy of the gateway, beside the deployed one.** `make eval-candidate
  CANDIDATE=NAME` asks the golden set through a copy of the deployed gateway's Deployment, made as the
  compromised-model stub's copy is in ADR 0019, now shared in `test/lib/gateway-copy.sh`. The copy
  keeps the deployed gateway's secrets, settings and network label, and is removed when the run ends.
  The rest of the deployment, the model, retrieval and executor, serves it as it serves the deployed
  gateway, and is still compared with the baseline's. So the candidate's change is the only
  difference, and the gate passes or fails it by its answers.
- **The control is a copy with the deployed image,** `CANDIDATE=deployed`. It shows that running
  through a copy changes nothing on its own.
- **A changed prompt is a file, built into the bundle as a test image.** Each
  `test/eval/candidates/NAME.prompt` is built by `infra/build-registry.sh` from the gateway's own
  source with only its system prompt replaced, and signed as `gateway-candidate:NAME`, as the stub
  is. `deploy/` never names it. The leak check also looks for the candidate's own prompt sentences.
- **The first candidate is `worse-prompt`:** a shorter, friendlier prompt of the kind a rewrite
  might produce, which drops the rules to answer only from passages, to cite them, to say when the
  manuals do not answer, and to treat passages as reference material. It keeps the rule to propose a
  ticket when asked.
- **A candidate cannot be saved as a baseline.** The baseline is of the deployed gateway.

## Alternatives

- **Deploying the worse prompt through Git and Argo CD.** It is what a real change would do, but the
  gate would fail it on the deployment line, and each change and its revert would cost a rebuild.
- **A prompt read from the environment or a ConfigMap.** The candidate would need no image, but the
  deployed gateway would gain a setting that replaces its safety instructions, for a test's sake.
- **A build tag in the gateway's source.** The worse prompt would then sit in the deployed program's
  source tree. The swap here keeps it under `test/`.

## Consequences

- **Each candidate costs a rebuild to add,** since its image goes into the bundle. Running it again
  does not.
- **A prompt is the only kind of candidate so far.** A worse chunking change or model needs another
  kind: a copy of the ingestion and retrieval, or of the chat server.
- **The candidate shares the deployment's database, executor and audit log,** as the stub's copy
  does. The run's hard failures still check that nothing was filed or confirmed.

### What this does not prove

- **That the gate fails every worse prompt.** This one is far worse, and failed every answer
  question. A prompt that still makes the model cite, but worse passages, or that answers some
  refusals, is a subtler change, and how small a change the gate can see is not measured.
- **That the refusal and leak checks see a worse prompt.** They passed for this one only because the
  gateway withheld every reply. The answer questions are what failed it.
- **Anything about a worse chunking change or model.** Neither has been run. Phase 4's done-when
  names any one of the three; the others are for when there is a reason to change them.
- **That a candidate behaves as it would deployed.** It runs as a copy, with the deployed gateway's
  settings, against the same servers. The control shows the copy changes nothing on its own, for this
  run.

## Measurements

On 2026-11-20, in the lab VM after a `make rebuild`, which passed, including the stub tests now using
the shared copy, with the images pinned in `infra/versions.env`, Ministral 3 3B Instruct
(Apache-2.0) at temperature 0, and ADR 0026's saved baseline of three runs, tolerance 0.

- **The control, `CANDIDATE=deployed`, passed the gate,** in 1302 s of turns, 28 s per turn. It
  failed the same two questions as every baseline run, p11 and i03, and no others. The same three
  refusals were stopped by the gateway, and the same two proposals were made.
- **`CANDIDATE=worse-prompt` failed the gate:** 30 of the 45 questions that passed in every baseline
  run failed, against a tolerance of 0, all of them answer questions. By kind: answer 0 of 31, leak 5
  of 5, refusal 6 of 6, injection 4 of 5, with i03 failing as in the baseline.
- **Why it failed:** the gateway showed no answer at all. All 47 replies cited no passage, so all 47
  were withheld. The model searched in 29 of the 47 turns, against 43 for the control, and for p01 it
  did not search at all. Turns took 1962 s, 42 s per turn on average.
- **No hard failures in either run:** every turn completed, no technician turn found a restricted
  passage, no issue was filed, no proposal was confirmed, and the audit chain was unbroken.
- **The citation check is what kept this prompt's replies from users.** Without the rule to cite,
  the model replied without citations, and ADR 0022's rule that an uncited answer is not shown held
  for every one. The gate saw it as lost answers, not as a leak.
