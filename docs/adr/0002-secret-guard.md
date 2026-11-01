# 0002: Local secret guard

**Status:** accepted
**Date:** 2026-10-31

## Context

The repository is meant to become public, and no remote exists yet, so CI cannot be the first
line of defence. Gitleaks finds secrets by pattern and entropy, but it cannot know project-specific
terms that must stay out of a public repository.

## Decision

`scripts/guard.sh` is the single implementation, called by the pre-commit hook, the pre-push hook
and `make check`. It runs gitleaks and a local, gitignored `.denylist` of extended regular
expressions.

- **Fails closed.** A missing, empty or malformed denylist stops the commit or push. CI on a clean
  clone has to opt out explicitly with `DENYLIST_OPTIONAL=1`.
- **Never prints what matched.** Gitleaks runs with `--redact`, and the denylist reports only the
  file or commit. A match shown in terminal output or a CI log would be a new leak.
- **Pre-commit checks the whole index**, not only the staged diff, so a term added to the denylist
  later is caught in files already tracked.
- **Pre-push and `make check` scan every commit on every ref**, including messages and author
  fields. The pre-commit hook cannot see the commit message, so this is where a message is checked.

## Alternatives

- **pre-commit framework:** an extra dependency for two short hooks.
- **Separate logic in each hook:** would drift. One script is also what the tests exercise.
- **`grep -q` in the history scan:** rejected after a test showed it missing matches. It exits early,
  the writer gets SIGPIPE, and `pipefail` turns a match into a non-match on large diffs.

## Consequences

- `make test-secrets` proves the behaviour in a temporary repo with fake values generated on each
  run. Real secrets are never fixtures.
- Hooks are local and can be bypassed with `--no-verify`. Pre-push and `make check` catch that
  afterwards, and only if they are run.
- This guard does not prove a repository is free of secrets. It catches known patterns and listed
  terms only.
- **Gitleaks misses some random secrets.** Its generic rule skips a value containing a listed common
  word. On 2026-11-01 it missed 57 of 5000 random 32-character values in `api_key = "..."` lines,
  using gitleaks 8.30.1 and its default rules. `make test-secrets` therefore screens its generic
  fake value first: it proves the guard blocks what gitleaks detects, not gitleaks' recall.
- A value in the denylist is not removed from anywhere by being listed. Leaked credentials still
  need rotating.

## Measurements

None. This decision has no performance dependency.
