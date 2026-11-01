#!/usr/bin/env python3
"""Measure one GGUF embedding model under llama-server: memory, speed and a sanity check.

Uses the same capped scope and 4 threads as measure.py. The sanity check compares a query with one
related and one unrelated passage. It shows the vectors carry some meaning, not retrieval quality:
recall is measured in Phase 2 against a real corpus. Nomic's task prefixes are applied.
"""
import argparse
import datetime
import json
import math
import pathlib
import statistics
import time
import urllib.request

from measure import BIN, THREADS, URL, available_mib, capped_server, find_gguf, memory_figures

LICENCE = "Apache-2.0"
CAP_MIB = 1000
CONTEXT = 2048
RUNS = 3
CHUNKS_PER_RUN = 20
QUERIES_PER_RUN = 20
RELATED = (
    "Before maintenance, cordon the node so no new pods are scheduled on it, then drain it. "
    "Draining evicts the running pods safely and respects their disruption budgets."
)
UNRELATED = (
    "Grease the conveyor bearings every three months. Use a lithium-based grease, and wipe away "
    "any excess so that dust does not collect on the housing."
)
QUERY = "How do I take a Kubernetes node out of service?"
# About the size of a retrieval chunk. Each input gets its own number, so each one differs.
CHUNK = " ".join([RELATED, UNRELATED] * 6)


def embed(text):
    body = json.dumps({"input": text, "model": "embedding", "cache_prompt": False}).encode()
    request = urllib.request.Request(
        f"{URL}/v1/embeddings", body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=300) as response:
        reply = json.load(response)
    return reply["data"][0]["embedding"], reply["usage"]["prompt_tokens"]


def cosine(a, b):
    dot = sum(x * y for x, y in zip(a, b))
    return dot / (math.sqrt(sum(x * x for x in a)) * math.sqrt(sum(y * y for y in b)))


def timed_run(texts):
    """Embed the texts one at a time. Return items/s, tokens/s, tokens per item and each latency."""
    latencies = []
    tokens = 0
    for text in texts:
        start = time.monotonic()
        tokens += embed(text)[1]
        latencies.append(time.monotonic() - start)
    elapsed = sum(latencies)
    return len(texts) / elapsed, tokens / elapsed, tokens // len(texts), latencies


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    # Only Nomic: the task prefixes and mean pooling below would misrepresent another model.
    parser.add_argument("name", choices=["nomic"], help="directory under the spike root")
    parser.add_argument("--out", type=pathlib.Path, help="directory for the result JSON")
    args = parser.parse_args()

    gguf = find_gguf(args.name)
    before = available_mib()
    with capped_server(gguf, "-c", str(CONTEXT), "-b", str(CONTEXT), "-ub", str(CONTEXT),
                       "--embedding", "--pooling", "mean", cap_mib=CAP_MIB) as (server, load_s):
        query, _ = embed(f"search_query: {QUERY}")
        related, _ = embed(f"search_document: {RELATED}")
        unrelated, _ = embed(f"search_document: {UNRELATED}")
        chunks = [timed_run([f"search_document: {i} {CHUNK}" for i in range(CHUNKS_PER_RUN)])
                  for _ in range(RUNS)]
        queries = [timed_run([f"search_query: {i} {QUERY}" for i in range(QUERIES_PER_RUN)])
                   for _ in range(RUNS)]
        latencies = [seconds * 1000 for run in queries for seconds in run[3]]
        result = {
            "measured": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
            "llama_cpp": BIN.name,
            "model": gguf.name,
            "licence": LICENCE,
            "threads": THREADS,
            "context": CONTEXT,
            "host_available_before_mib": before,
            "dimensions": len(query),
            "load_s": round(load_s, 1),
            **memory_figures(server.pid),
            "tokens_per_chunk": chunks[0][2],
            "chunks_per_s": [round(c[0], 2) for c in chunks],
            "chunks_per_s_median": round(statistics.median(c[0] for c in chunks), 2),
            "chunk_tokens_per_s": [round(c[1], 1) for c in chunks],
            "tokens_per_query": queries[0][2],
            "queries_measured": len(latencies),
            "query_ms_median": round(statistics.median(latencies), 1),
            "query_ms_slowest": round(max(latencies), 1),
            "cosine_related": round(cosine(query, related), 3),
            "cosine_unrelated": round(cosine(query, unrelated), 3),
        }

    print(json.dumps(result, indent=2))
    if result["cosine_related"] <= result["cosine_unrelated"]:
        raise SystemExit("sanity check failed: the unrelated passage scored at least as high")
    if args.out:
        args.out.mkdir(parents=True, exist_ok=True)
        (args.out / f"embedding-{args.name}.json").write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
