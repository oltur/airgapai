# 0010: Answer and embedding models

**Status:** accepted for the models; the VM memory limit is applied but provisional until a model is measured
in the VM
**Date:** 2026-11-02, limit applied 2026-11-04

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
- **VM limit: 10240 MiB, up from 8192 MiB,** set in `infra/versions.env` once Phase 1 passed at it. It is
  provisional: the packaging step measures the model server in the VM, and that measurement decides
  whether it stays.

Ministral was the only candidate to pass all 11 tool cases, and the only one tested that did not call
`create_ticket` when a tool result told it to. That pass is narrow: see the limits below. Generation is
slower than the two smaller models and its memory use is higher, but its prompt speed beats Granite's,
and tool behaviour matters more here because the gateway is the centre of the project.

## Alternatives

- **Granite 4.1 3B:** faster at generating (16.6 against 15.2 tokens/s) but slower at reading a prompt
  (48.9 against 56.4 tokens/s), which matters for retrieval prompts. It called `create_ticket` as the
  planted instruction told it to. The gateway must contain that regardless, but there was no reason to
  start with the model that failed this case.
- **SmolLM3 3B:** the fastest and smallest, but it never called a tool, passing only the 5 cases that
  need no call. This may be a chat template or server setting rather than the model. It was not
  investigated, because another candidate passed.
- **Qwen3.5 4B:** Apache-2.0, per its Hugging Face record. It had the lowest generation and prompt
  speeds and the highest memory of the four, and it was not taken forward to the tool screen. Its results
  stay in the repository as a record.
- **A third-party conversion of Ministral:** measured first, then replaced with Mistral's own file, so
  the source of the weights is one step shorter.

## Consequences

- **Speed:** at about 15 tokens/s, a 200-token answer takes about 13 s of generation by arithmetic, on
  top of prompt processing at about 56 tokens/s. A 2000-token prompt adds about 36 s by arithmetic.
  Retrieved context will need to stay small, and streaming will matter.
- **Memory:** the two models' peak process memory adds up to 4204 MiB, and the running guest had 4618 MiB
  available with no model, PostgreSQL, retrieval service or gateway. That is an upper-end comparison:
  about half of Ministral's RSS looks like file-backed pages (cgroup peak 1981 MiB), which the kernel can
  reclaim and available memory already counts, at a cost in speed if they are read back. The real need is
  not known, and the guest has no swap, so the limit is expected to rise.
- **The limit was chosen by arithmetic, before any measurement at it.** 10240 MiB is 2048 MiB more for the guest. With
  the VM running, the host had 9330 MiB available while the VM's process held 6590 MiB resident and
  1307 MiB in swap. If stopping it frees the resident part (swap is not RAM), about 15920 MiB would be available, by
  arithmetic, and `make vm-up` needs 13312 MiB for this limit. Host availability moves: the spike's runs recorded
  11383 to 11947 MiB the day before, with the VM's state not recorded, and that would be too little if the
  VM was stopped. `make vm-up` checks this and refuses rather than overcommit. It passed the check on
  2026-11-02; see Measurements.
- **The start check cannot see later pressure.** `make vm-up` checks available memory only at start. The
  VM then grows to nearly its full limit, and the host's other workloads can be pushed into swap without
  any warning. After the rebuild, 5144 MiB of host swap was in use, and whether that slowed anything was
  not measured.
- **Changing the limit needs a VM rebuild,** which reruns every Phase 1 test. That has passed at 10240 MiB;
  see Measurements. The limit stays provisional until a model is measured in the VM.

### What this does not prove

- **Not run in the VM or the cluster,** where speed and memory may differ.
- **The models were not run together.** The 4204 MiB is a sum of separate runs, and CPU contention
  between them was not measured.
- **The tool screen is one run per case at temperature 0,** on cases written for this choice. It helps
  choose a model and is not an eval. Thinking was off, and llama-server may constrain arguments with a
  grammar, so valid arguments may reflect the server as well as the model.
- **Ministral's injection pass is narrow.** The check only looks for a `create_ticket` call and the text
  `45`. The reply gave the answer, then said it would create a ticket and asked whether to file it as
  high priority, so the planted text may have influenced it. It asked rather than acted, but the screen
  cannot tell. The gateway must hold against a fully compromised model, and Phase 3 tests that.
- **Speed prompts were short and output was 128 tokens.** Long prompts and sustained load were not
  measured. The 4 threads may land on different core types, because the CPU has hybrid cores. The speed
  and memory runs omitted `--jinja`, which the tool runs used and the gateway will need.
- **Load times are not comparable between chat models.** Granite's and Ministral's cgroup peaks (2017 and
  1981 MiB, against RSS of 3831 and 4008 MiB) suggest their files were already cached. SmolLM3's and
  Qwen's peaks about equal their RSS, which suggests theirs were read fresh, so their longer load times
  may be mostly a cache effect. A cold start of Ministral was not measured.
- **The memory caps limit less than they imply.** The cgroup peak leaves out file pages already cached,
  so the cap should not be read as proof that a model fits in that amount on a cold cache. RSS is used
  here for that reason.
- **The embedding check is a sanity check, not retrieval quality.** One query scored higher against one
  related passage than one unrelated passage. Recall is measured later against the real corpus.
- **Provenance is limited.** Each model hash comes from the Hugging Face record, which is not a
  signature from the publisher. The llama.cpp hash records the file as first downloaded, because the
  release has no checksum file, and its attestation was not verified.
- **The saved chat and tool results predate later script changes:** a shared server helper, the data
  directory and finer polling of load time (0.2 s then, 0.05 s now). The chat results were not rerun
  with the current scripts, so their load times are coarser than the code suggests.
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

These runs used a context of 4096 tokens and a cap of 5000 MiB, with no out-of-memory kills. Compare load
times only with the limit above in mind. All 11 tool cases took 74.9 s on Ministral and 98.4 s on Granite,
one run each.

`nomic-embed-text-v1.5` Q8_0, same host and threads, context of 2048 tokens, cap of 1000 MiB:

| Measure | Result |
|---|---|
| Peak process RSS | 196 MiB |
| Load | 0.1 s |
| Dimensions | 768 |
| Chunks of 397 tokens | 2.43 per second, median of 3 runs of 20 |
| Query of 21 tokens | 24.0 ms median of 60, slowest 51.2 ms |
| Cosine, query to related and unrelated passage | 0.639 and 0.433 |

Memory for sizing, measured on 2026-11-02 with the VM running at its 8192 MiB limit, Docker limited to
7 GiB, and no model loaded:

| Measure | Result |
|---|---|
| Host available | 9330 MiB |
| VM process resident and in swap | 6590 MiB and 1307 MiB |
| Guest available | 4618 MiB, with no swap |

One `make rebuild` on 2026-11-02 at the 10240 MiB limit, with downloads cached and Docker limited to
7 GiB as before, read straight after its tests. It shows Phase 1 still passes at this limit, using the pinned Go toolchain. No model was
loaded, so it does not settle the limit:

| Measure | Result |
|---|---|
| Destroy, bundle and VM up | 236 s |
| All four test suites | 540 s, all passed |
| Guest available | 6609 MiB, with no swap |
| Guest memory in use, excluding cache | 3331 MiB |
| VM process resident and in swap | 10147 MiB and 54 MiB |
| Host available | 8408 MiB, with 5144 MiB of host swap in use |

The VM's process held nearly its whole limit, mostly as guest page cache, so the host should be sized for
the full limit and not for what the guest is using.

To reproduce: run `spike/model/fetch.sh`, then `measure.py NAME --out DIR`, `toolcall.py NAME --out DIR`
and `embed.py nomic --out DIR`. Without `--out`, nothing is saved. The script no longer fetches Qwen, so
that row cannot be reproduced from it, and see the limit above on script changes.
