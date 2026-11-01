#!/usr/bin/env bash
# Download the model spike's inputs on the online host, pinned by revision and SHA-256.
# Each input gets its own directory. Nothing here is executed or loaded.
set -euo pipefail

root="${SPIKE_DIR:-/mnt/data/airgapai/spike}"

# Published by the llama.cpp release, which has no checksum file. This hash records the file as
# first downloaded on 2026-11-01. Its attestation was not verified.
llama_url=https://github.com/ggml-org/llama.cpp/releases/download/b12032/llama-b12032-bin-ubuntu-x64.tar.gz
llama_sha=9f10432ca3f66ef003a7a540aaffb9da0acadaefd05404521b9ab7a2436af304

# name repository revision file sha256 (from the Hugging Face LFS record)
models=(
  "granite ibm-granite/granite-4.1-3b-GGUF ab4701481089b58a082ef63cc1cee738887293ff granite-4.1-3b-Q4_K_M.gguf 662b0626cd58f443baea23559b469df6576a81d349649c59413b36a9fb32eb29"
  "smollm3 ggml-org/SmolLM3-3B-GGUF 4965cb60b150737b68a0408c36aeefb65078f894 SmolLM3-Q4_K_M.gguf 8334b850b7bd46238c16b0c550df2138f0889bf433809008cc17a8b05761863e"
  "qwen unsloth/Qwen3.5-4B-GGUF e87f176479d0855a907a41277aca2f8ee7a09523 Qwen3.5-4B-Q4_K_M.gguf 00fe7986ff5f6b463e62455821146049db6f9313603938a70800d1fb69ef11a4"
  "ministral unsloth/Ministral-3-3B-Instruct-2512-GGUF 7564922f37fa5bbb62b87f09a55c12f1f91d7a6a Ministral-3-3B-Instruct-2512-Q4_K_M.gguf fd46fc371ff0509bfa8657ac956b7de8534d7d9baaa4947975c0648c3aa397f4"
  "nomic nomic-ai/nomic-embed-text-v1.5-GGUF 0188c9bf409793f810680a5a431e7b899c46104c nomic-embed-text-v1.5.Q8_0.gguf 3e24342164b3d94991ba9692fdc0dd08e3fd7362e0aacc396a9a5c54a544c3b7"
)

# fetch URL DESTINATION SHA256: resume a partial file, and keep it only if the hash matches.
fetch() {
  local url="$1" dest="$2" sha="$3"
  mkdir -p "$(dirname "$dest")"
  if [[ -f "$dest" ]] && sha256sum --status -c <<<"$sha  $dest"; then
    echo "ok, present: $dest"
    return
  fi
  curl -fsSL --retry 3 -C - -o "$dest.part" "$url"
  sha256sum --status -c <<<"$sha  $dest.part" || { rm -f "$dest.part"; echo "hash mismatch: $url" >&2; exit 1; }
  mv "$dest.part" "$dest"
  echo "ok, fetched: $dest"
}

fetch "$llama_url" "$root/llama.cpp/${llama_url##*/}" "$llama_sha"
for entry in "${models[@]}"; do
  read -r name repo revision file sha <<<"$entry"
  fetch "https://huggingface.co/$repo/resolve/$revision/$file" "$root/$name/$file" "$sha"
done
