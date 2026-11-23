# 0028: Where the eval gates run

**Status:** accepted
**Date:** 2026-11-23

## Context

ADRs 0025 to 0027 built three gates: retrieval against its baseline, the deployed gateway's answers
against theirs, and a candidate gateway beside the deployed one. Each was run by hand. The plan asks
for the gate in a plain CI job, with the fast, deterministic checks on every change, and the
generation evals on a model or prompt change, or nightly.

Two facts constrain where they can run. The GitHub CI job has no VM, so it cannot reach the
deployment the gates measure. And the generation gates are slow on this CPU: about 22 minutes for one
run of the deployed gateway, and about 55 for the candidate pair.

## Decision

- **CI runs what needs no VM, on every change:** the gates' own rules, through their unit tests in
  `make lint`, which the `checks` job runs on every pull request and push to main. These cover the
  page reading, the scoring, the tolerance, the hard failures and the exit status, and fail if a rule
  is broken, as ADR 0026's mutation checks showed.
- **`make rebuild` runs the retrieval gate every time,** after `make test-retrieval`. It takes about 5
  s. ADR 0025's baseline passed after both rebuilds it was run on, so a rebuilt index has not yet
  made it fail with nothing changed.
- **`make rebuild EVALS=generation` adds the generation gates:** `make eval-generation`, the deployed
  gateway against its baseline, then `make test-eval-gate`. They are opt-in, for a change to the
  model, the prompt or the gateway, as the plan asks, since they add about 80 minutes to a 21-minute
  rebuild.
- **`make test-eval-gate` shows the gate still works:** the control copy must pass, and the
  worse-prompt candidate must fail by its answers alone. For that, `test/eval/generation.py` now
  exits 1 only when the run failed because more stable questions failed than the tolerance, and 2
  for anything else: a hard failure, a run that did not finish, a changed deployment, the wrong
  candidate, or a crash. A worse prompt that broke the run some other way would not count as the gate
  failing it.

## Alternatives

- **A job in the in-cluster Forgejo CI on each push to the deployed repository** (ADR 0007). It would
  run inside the air gap, against the deployment, on every change. The runner would need a path
  through the default-deny rules to retrieval and the gateway, and the harness's Python, which its
  image does not have. That widens what a CI job can reach, for a gate that `make rebuild` already
  runs. Worth revisiting if changes start to reach the cluster other than by a rebuild.
- **The generation gate in every rebuild.** Every rebuild would take about five times as long, and
  most rebuilds change neither the model nor the prompt.
- **A nightly run.** The lab is a home machine with no scheduler running the stack; a timer would
  run a VM build unattended. The opt-in flag covers the changes that matter here.

## Consequences

- **A rebuild fails if retrieval regresses or leaks,** with no extra step.
- **A prompt or model change can reach main without the generation gate,** if `EVALS=generation` is
  not given. Nothing enforces it. The pull request is where to say it was run.
- **The exit status is part of the gate's contract,** and has its own unit tests.

### What this does not prove

- **That CI catches a worse answer.** CI tests the gate's rules, not the deployment's answers.
- **That the opt-in path was run as part of a rebuild.** `make test-eval-gate` was run on its own on a
  rebuilt VM, below; `make rebuild EVALS=generation` as a whole has not been run.

## Measurements

On 2026-11-23, in the lab VM built by ADR 0027's last `make rebuild`, before the rebuild included the
retrieval gate, with the images pinned in `infra/versions.env` and the baselines of ADRs 0025 and
0026.

- **`make eval-retrieval` passed in 5 s,** all 33 questions with expected pages at hit@3, nothing
  leaked.
- **`make test-eval-gate` passed in 3270 s.** The control passed, failing only the baseline's p11 and
  i03, in 1275 s of turns. The worse prompt exited 1, with 30 of the 45 stable questions lost and no
  answer shown, in 1976 s of turns, the same result as its two runs in ADR 0027.
- **Not measured:** a full `make rebuild` with the retrieval gate in it, or with `EVALS=generation`.
  The pieces were each run on their own, as above.
