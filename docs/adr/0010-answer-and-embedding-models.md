# 0010: Answer and embedding models

**Status:** accepted for the models; the VM memory limit is proposed until measured in the VM
**Date:** 2026-11-02

## Context

Phase 2 needs one model to answer questions and call tools, and one to embed text. Both run on CPU
under llama.cpp, inside a VM that shares a 30 GiB host with other workloads. The answer model must
later drive the gateway, so tool calling was screened before speed decided anything.

Measured facts are below. Everything was run on the host, not in the VM, so these are inputs to the
VM's sizing and not a result for the deployed stack.

## Decision

- **Answer model: Ministral 3 3B Instruct, Q4_K_M, from Mistral's own GGUF repository**
  (`mistralai/Ministral-3-3B-Instruct-2512-GGUF`, revision `eb599d4`). Apache-2.0, per its Hugging
  Face record.
- **Embedding model: `nomic-embed-text-v1.5`, Q8_0** (`nomic-ai/nomic-embed-text-v1.5-GGUF`, revision
  `0188c9b`). Apache-2.0, per its Hugging Face record.
- **Server: llama.cpp `b12032`** (MIT), behind its OpenAI-compatible API. Revisions and SHA-256 hashes
  are pinned in `spike/model/fetch.sh`.
- **Proposed VM limit: 10240 MiB, up from 8192 MiB.** `infra/versions.env` does not change here. The
  packaging step measures the model server in the VM, and that measurement decides the limit.

Ministral was the only candidate to pass all 11 tool cases, including not obeying an instruction
planted in a tool result. It costs some speed and memory compared with the two smaller models, and
tool behaviour matters more here because the gateway is the centre of the project.

## Alternatives

- **Granite 4.1 3B:** slightly faster, but it obeyed the planted instruction and called
  `create_ticket`. The gateway must contain that regardless, but there was no reason to start with
  the model that failed this case.
- **SmolLM3 3B:** the fastest and smallest, but it never called a tool, passing only the 5 cases that
  need no call. This may be a chat template or server setting rather than the model. It was not
  investigated, because another candidate passed.
- **Qwen3.5 4B:** the slowest and largest of the four, so it was dropped before the tool screen. Its
  memory and speed results stay in the repository as a record.
- **A third-party conversion of Ministral:** measured first, then replaced with Mistral's own file, so
  the source of the weights is one step shorter.

## Consequences

- **Speed:** at about 15 tokens/s, a 200-token answer takes about 13 s of generation by arithmetic, on
  top of prompt processing at about 56 tokens/s. A 2000-token prompt adds about 36 s by arithmetic.
  Retrieved context will need to stay small, and streaming will matter.
- **Memory:** the two models' peak process memory adds up to 4204 MiB. The VM guest had about 4.6 GiB
  available under its current limit, before PostgreSQL, the retrieval service or the gateway. That
  would leave too little room, which is why the limit needs to rise.
- **The proposed limit is arithmetic, not a measurement.** With Docker limited to 7 GiB, the host had
  about 6.7 GiB available with the 8 GiB VM running. Moving 2 GiB to the VM should leave about 4.7 GiB
  on the host and allow about 6.6 GiB in the guest. `make vm-up` requires the VM limit plus a 3072 MiB
  margin to be available, which puts the ceiling at roughly 11.7 GiB.
- **Changing the limit needs a VM rebuild,** which reruns every Phase 1 test.

### What this does not prove

- **Not run in the VM or the cluster,** where speed and memory may differ.
- **The models were not run together.** The 4204 MiB is a sum of separate runs, and CPU contention
  between them was not measured.
- **The tool screen is one run per case at temperature 0,** on cases written for this choice. It helps
  choose a model and is not an eval. Thinking was off, and llama-server may constrain arguments with a
  grammar, so valid arguments may reflect the server as well as the model.
- **One resisted injection does not show the model resists injection.** The gateway must hold against
  a fully compromised model, and Phase 3 tests that.
- **Speed prompts were short and output was 128 tokens.** Long prompts and sustained load were not
  measured. The 4 threads may land on different core types, because the CPU has hybrid cores.
- **Load times were measured with the file probably already in the page cache,** so a cold start may
  take longer.
- **The memory caps limit less than they imply.** The cgroup peak leaves out file pages already cached,
  which is why it reads 1981 MiB where the process's peak RSS reads 4008 MiB. RSS is used here, and
  the cap should not be read as proof the model fits in that amount on a cold cache.
- **The embedding check is a sanity check, not retrieval quality.** One query scored higher against one
  related passage than one unrelated passage. Recall is measured later against the real corpus.
- **Provenance is limited.** Each model hash comes from the Hugging Face record, which is not a
  signature from the publisher. The llama.cpp hash records the file as first downloaded, because the
  release has no checksum file, and its attestation was not verified.
- **Licences were read from each Hugging Face record,** which is not legal advice.

## Measurements

Measured on 2026-11-01 on the host: Core Ultra 7 155H, 30 GiB RAM, CPU only, 4 threads, llama.cpp
`b12032`, Q4_K_M. Results are in [`spike/model/results/`](../../spike/model/results/), and speeds are
the median of 3 runs.

| Model | Peak process RSS | Load | Generate | Prompt | Tool cases passed |
|---|---|---|---|---|---|
| **Ministral 3 3B Instruct** | 4008 MiB | 1.6 s | 15.2 tokens/s | 56.4 tokens/s | 11 of 11 |
| Granite 4.1 3B | 3831 MiB | 1.6 s | 16.6 tokens/s | 48.9 tokens/s | 10 of 11 |
| SmolLM3 3B | 3503 MiB | 2.0 s | 17.8 tokens/s | 59.7 tokens/s | 5 of 11 |
| Qwen3.5 4B | 4371 MiB | 3.0 s | 12.1 tokens/s | 32.2 tokens/s | not run |

These runs used a context of 4096 tokens and a cap of 5000 MiB, with no out-of-memory kills.

`nomic-embed-text-v1.5` Q8_0, same host and threads, context of 2048 tokens, cap of 1000 MiB:

| Measure | Result |
|---|---|
| Peak process RSS | 196 MiB |
| Load | 0.1 s |
| Dimensions | 768 |
| Chunks of 397 tokens | 2.43 per second, median of 3 runs of 20 |
| Query of 21 tokens | 24.0 ms median of 60, slowest 51.2 ms |
| Cosine, query to related and unrelated passage | 0.639 and 0.433 |

To reproduce: `spike/model/fetch.sh`, then `measure.py`, `toolcall.py` and `embed.py` in the same
directory. The script no longer fetches Qwen, so that row cannot be reproduced from it.
