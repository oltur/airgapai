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
run of the deployed gateway in ADR 0026, and 3270 s for the candidate pair below. The last
`make rebuild` before this took 1240 s: 260 s to build and 980 s of tests.

## Decision

- **CI runs what needs no VM, on every change:** the gates' own rules, through their unit tests in
  `make lint`, which the `checks` job runs on every pull request and push to main. These cover the
  page reading, the scoring, the tolerance, the hard failures and the exit status, and fail if a rule
  is broken, as ADR 0026's mutation checks showed.
- **`make rebuild` runs the retrieval gate every time,** after all the tests and the audit copy, so
  that a gate failure never skips a security test or loses the run's audit rows. It takes about 5 s.
  ADR 0025's baseline passed after the rebuild in ADR 0026 and after the one measured below, so a
  rebuilt index has not yet made it fail with nothing changed, in two rebuilds.
- **`make rebuild EVALS=generation` adds `make test-eval-gate`,** then copies the audit log again,
  whether the gate passed or not, and only then fails if it did.
  It is opt-in, for a change to the model, the prompt or the gateway, as the plan asks, since it
  more than triples a rebuild. Its control is the deployed image, through a copy, against the
  baseline, so it gates the unchanged gateway as well, and `make eval-generation` is not run
  separately: that would ask the same gateway the golden set twice.
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
- **The generation gates in every rebuild.** Every rebuild would take more than three times as long,
  and most rebuilds change neither the model nor the prompt.
- **A nightly run.** The lab is a home machine with no scheduler running the stack; a timer would
  run a VM build unattended. The opt-in flag covers the changes that matter here.

## Consequences

- **A rebuild fails if retrieval regresses or leaks,** with no extra step.
- **A prompt or model change can reach main without the generation gate,** if `EVALS=generation` is
  not given. Nothing enforces it. The pull request is where to say it was run.
- **The exit status is part of both gates' contract,** and has its own unit tests. Each gate keeps
  the failures that make a run mean something else apart from those for its results, and the exit
  status reads them apart, not from the text of the lines. The retrieval gate exits the same way: 1
  for worse results or a leak, 2 for other questions, another deployment or a crash.

### What this does not prove

- **That CI catches a worse answer.** CI tests the gate's rules, not the deployment's answers.
- **That the rebuild runs them as described.** `make eval-retrieval` and `make test-eval-gate` were
  each run on their own on a rebuilt VM, below. Neither `make rebuild` with the retrieval gate in it
  nor `make rebuild EVALS=generation` has been run as a whole.

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
