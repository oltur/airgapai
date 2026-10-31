#!/usr/bin/env bash
# Secret guard: gitleaks plus the local denylist. Prints where a match is, never what matched.
#   guard.sh staged    the index, run by pre-commit
#   guard.sh history   every commit on every ref including messages, run by pre-push and make check
# A missing .denylist fails unless DENYLIST_OPTIONAL=1, which is for CI on a clean clone.
set -euo pipefail

export PATH="$HOME/.local/bin:$PATH"
mode="${1:-}"
cd "$(git rev-parse --show-toplevel)"
denylist=.denylist
failed=0

command -v gitleaks >/dev/null || { echo "guard: gitleaks not found, see make setup" >&2; exit 2; }

patterns() { grep -Ev '^[[:space:]]*(#|$)' "$denylist"; }

# Counts matching lines on stdin. Reads all input: an early exit would SIGPIPE the writer, and
# pipefail would then report a match as no match.
count_matches() { LC_ALL=C grep -a -c -i -E -f <(patterns) || true; }

have_denylist() {
  if [[ -f $denylist ]] && patterns >/dev/null; then
    local code=0
    grep -E -f <(patterns) </dev/null >/dev/null 2>&1 || code=$?
    if [[ $code -gt 1 ]]; then
      echo "guard: $denylist has an invalid pattern, refusing to continue" >&2
      exit 2
    fi
    return 0
  fi
  if [[ ${DENYLIST_OPTIONAL:-0} == 1 ]]; then
    echo "guard: no denylist patterns, skipped because DENYLIST_OPTIONAL=1" >&2
    return 1
  fi
  echo "guard: $denylist is missing or empty, refusing to continue" >&2
  exit 2
}

case "$mode" in
  staged)
    gitleaks git --staged --redact --no-banner --log-level warn --verbose . >&2 || failed=1
    if have_denylist; then
      code=0
      git grep --cached -c -i -E -f <(patterns) >&2 || code=$?
      if [[ $code == 0 ]]; then
        echo "guard: denylisted term in the files counted above" >&2
        failed=1
      elif [[ $code != 1 ]]; then
        echo "guard: denylist search failed" >&2
        exit 2
      fi
      if [[ $(git diff --cached --name-only | count_matches) != 0 ]]; then
        echo "guard: denylisted term in a staged file name" >&2
        failed=1
      fi
    fi
    ;;
  history)
    gitleaks git --redact --no-banner --log-level warn --verbose --log-opts=--all . >&2 || failed=1
    if have_denylist; then
      while read -r rev; do
        if [[ $(git log -1 -p -m --first-parent --text --no-textconv --no-ext-diff --format='%an%n%ae%n%B' "$rev" | count_matches) != 0 ]]; then
          echo "guard: denylisted term in commit ${rev:0:12}" >&2
          failed=1
        fi
      done < <(git rev-list --all)
    fi
    ;;
  *)
    echo "usage: guard.sh staged|history" >&2
    exit 2
    ;;
esac

if [[ $failed != 0 ]]; then
  echo "guard: blocked" >&2
  exit 1
fi
echo "guard: $mode clean"
