#!/usr/bin/env bash
# Runs on the online host, by hand, when the corpus should change. Writes images/ingest/corpus.lock:
# the Kubernetes documentation commit, then the SHA-256 and path of every Markdown file under the
# given directories at that commit. The bundle build fetches exactly these files and checks each hash.
# Usage: lock-corpus.sh COMMIT DIRECTORY...
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
commit="$1"
shift
[[ $commit =~ ^[0-9a-f]{40}$ ]] || { echo "the commit must be a full SHA"; exit 1; }
lock="$here/../images/ingest/corpus.lock"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "https://api.github.com/repos/kubernetes/website/git/trees/$commit?recursive=1" >"$tmp/tree.json"
python3 -I - "$tmp/tree.json" "$@" >"$tmp/paths" <<'PY'
import json, sys
tree = json.load(open(sys.argv[1]))
assert not tree["truncated"], "the tree listing was truncated"
dirs = [d.rstrip("/") + "/" for d in sys.argv[2:]]
for item in sorted(tree["tree"], key=lambda i: i["path"]):
    if item["type"] == "blob" and item["path"].endswith(".md") and any(item["path"].startswith(d) for d in dirs):
        print(item["path"])
PY
[[ -s $tmp/paths ]] || { echo "no Markdown files under: $*"; exit 1; }
{
  echo "# Kubernetes documentation, CC BY 4.0, from github.com/kubernetes/website. Written by infra/lock-corpus.sh."
  echo "commit $commit"
  while read -r path; do
    curl -fsSL -o "$tmp/file" "https://raw.githubusercontent.com/kubernetes/website/$commit/$path"
    echo "$(sha256sum <"$tmp/file" | cut -d' ' -f1)  $path"
  done <"$tmp/paths"
} >"$tmp/lock"
mv "$tmp/lock" "$lock"
echo "$(grep -c '  ' "$lock") files locked at $commit"
