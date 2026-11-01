#!/usr/bin/env bash
# Install the pinned guard and lint tools into ~/.local/bin, for developers and for CI.
# Each download is checked against a hash. Gitleaks and Ruff publish theirs. ShellCheck publishes
# none, so its hash records the file as first downloaded on 2026-11-01.
set -euo pipefail

command -v go >/dev/null || { echo "Go is required, see test/egress/go.mod" >&2; exit 1; }
# shellcheck source=../infra/versions.env
source "$(dirname "$0")/../infra/versions.env"

bin="$HOME/.local/bin"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$bin"

# install URL SHA256 PATH_IN_ARCHIVE
install_tool() {
  local url="$1" sha="$2" member="$3" archive
  archive="$work/${url##*/}"
  curl -fsSL --retry 3 -o "$archive" "$url"
  sha256sum --status -c <<<"$sha  $archive" || { echo "hash mismatch: $url" >&2; exit 1; }
  tar xzf "$archive" -C "$bin" --strip-components="$(tr -cd / <<<"$member" | wc -c)" "$member"
}

install_tool "https://github.com/gitleaks/gitleaks/releases/download/v$GITLEAKS_VERSION/gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz" \
  551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb gitleaks
install_tool https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.x86_64.tar.gz \
  b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6 shellcheck-v0.11.0/shellcheck
install_tool https://github.com/astral-sh/ruff/releases/download/0.17.3/ruff-x86_64-unknown-linux-gnu.tar.gz \
  2b0890f5a55900af741f133918c2656919bbf18c2ab7741b4ca8403a90e647a9 ruff-x86_64-unknown-linux-gnu/ruff
GOBIN="$bin" go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
