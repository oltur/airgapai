#!/usr/bin/env bash
# Proves the guard in a throwaway repo, using fake values generated on each run.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
failures=0
output="$tmp/output"

random() { LC_ALL=C tr -dc "$1" </dev/urandom | head -c "$2" || true; }
fake_key="AKIA$(random 'A-Z2-7' 16)"
fake_term="denied$(random 'a-z' 12)"
fake_secret="$(random 'a-zA-Z0-9' 32)"

repo="$tmp/repo"
git init -q "$repo"
cd "$repo"
git config user.name test
git config user.email test@example.invalid
git config commit.gpgsign false
git config core.hooksPath "$root/.githooks"
printf '# fake\n%s\n' "$fake_term" >.denylist
printf '.denylist\n' >.gitignore
git add .gitignore
git commit -q -m "initial" >/dev/null 2>&1 || { echo "FAIL setup: clean first commit was blocked"; exit 1; }

report() { # name, wanted (0 pass, 1 block), actual exit code
  if [[ $2 == 0 && $3 == 0 ]] || [[ $2 == 1 && $3 != 0 ]]; then
    echo "ok   $1"
  else
    echo "FAIL $1"
    failures=$((failures + 1))
  fi
}

attempt() { # name, wanted, file, content
  local code=0
  printf '%s\n' "$4" >"$3"
  git add "$3"
  git commit -q -m "$1" >"$output" 2>&1 || code=$?
  report "$1" "$2" "$code"
  if grep -qF -e "$fake_key" -e "$fake_term" -e "$fake_secret" "$output"; then
    echo "FAIL $1: output revealed the matched value"
    failures=$((failures + 1))
  fi
  if [[ $code != 0 ]]; then
    git rm -q --cached -f "$3"
    rm -f "$3"
  fi
}

attempt "fake key is blocked" 1 key.txt "aws_access_key_id = $fake_key"
attempt "generic secret is blocked" 1 conf.txt "api_key = \"$fake_secret\""
attempt "denylisted term is blocked" 1 note.txt "written for ${fake_term^^}"
attempt "denylisted file name is blocked" 1 "$fake_term.txt" "harmless"
attempt "clean file commits" 0 clean.txt "nothing to see"

mv .denylist .denylist.away
attempt "missing denylist fails closed" 1 other.txt "harmless"
mv .denylist.away .denylist

code=0; "$root/scripts/guard.sh" history >"$output" 2>&1 || code=$?
report "history scan passes on clean history" 0 "$code"

git commit -q --allow-empty --no-verify -m "mentions $fake_term"
code=0; "$root/scripts/guard.sh" history >"$output" 2>&1 || code=$?
report "history scan finds a term in a commit message" 1 "$code"
git reset -q --hard HEAD~1

{ printf '%s\n' "$fake_term"; head -c 2000000 /dev/zero | tr '\0' 'x' | fold -w 80; } >large.txt
git add large.txt
git commit -q --no-verify -m "large"
code=0; "$root/scripts/guard.sh" history >"$output" 2>&1 || code=$?
report "history scan finds an early term in a large diff" 1 "$code"
git reset -q --hard HEAD~1

cp .denylist .denylist.good
printf '(unclosed\n' >>.denylist
attempt "malformed denylist fails closed" 1 other.txt "harmless"
code=0; "$root/scripts/guard.sh" history >"$output" 2>&1 || code=$?
report "malformed denylist fails the history scan" 1 "$code"
mv .denylist.good .denylist

printf '%s\n' "aws_access_key_id = $fake_key" >bypass.txt
git add bypass.txt
git commit -q --no-verify -m "bypass"
code=0; "$root/scripts/guard.sh" history >"$output" 2>&1 || code=$?
report "history scan finds a key committed with --no-verify" 1 "$code"

git remote add origin "$tmp/remote.git"
git init -q --bare "$tmp/remote.git"
code=0; git push -q origin HEAD >"$output" 2>&1 || code=$?
report "pre-push blocks the bad history" 1 "$code"

if [[ $failures != 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all guard checks passed"
