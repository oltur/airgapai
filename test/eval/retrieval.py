#!/usr/bin/env python3
"""Evaluate retrieval on the golden set in the VM, and gate on a baseline.

Each question is searched with its own role, as the chat does: the default scan, the question's own
text as the query, and k=5. A question with expected pages meets k if any of its top k chunks comes
from one of them. The chat asks for 3, so hit@3 is the figure that matters to an answer. Every
question is also searched as a technician with k=20, and no restricted chunk may come back. As the
control, each leak question must find restricted chunks as an engineer.

With --baseline, the run is compared question by question with a saved run. It fails if a question
that met k=3 in the baseline no longer does, beyond --tolerance such questions, or if anything
leaked.
With --save, the run becomes the new baseline.

The search uses the question, not the query the model would write, so this measures the index and
the embeddings, not the model's searching.
"""
import argparse
import datetime
import json
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import lab  # noqa: E402

GOLDEN = lab.ROOT / "test/eval/golden.json"
BASELINE = lab.ROOT / "test/eval/baseline/retrieval.json"
K = 5
CHAT_K = 3
LEAK_K = 20


def evaluate(golden):
    prefix = golden["prefix"]
    results = {}
    for q in golden["questions"]:
        top = lab.search(q["question"], q["role"], K)
        paths = [r["path"].removeprefix(prefix) for r in top]
        # What was asked: a baseline is only comparable for the same question, role and pages.
        entry = {"asked": {k: q[k] for k in ("question", "role", "kind", "expect")},
                 "kind": q["kind"], "role": q["role"],
                 "top": [f"{p}#{r['chunk_index']}" for p, r in zip(paths, top, strict=True)]}
        if q["expect"]:
            ranks = [i + 1 for i, p in enumerate(paths) if p in q["expect"]]
            entry["first_hit"] = ranks[0] if ranks else None
            entry[f"hit@{CHAT_K}"] = bool(ranks) and ranks[0] <= CHAT_K
            entry[f"hit@{K}"] = bool(ranks)
        leaked = [r for r in lab.search(q["question"], "technician", LEAK_K)
                  if r["classification"] != "public"]
        entry["technician_restricted"] = len(leaked)
        if q["kind"] == "leak":  # The control: the question does find restricted text for a reader.
            found = lab.search(q["question"], "engineer", LEAK_K)
            entry["engineer_restricted"] = sum(r["classification"] != "public" for r in found)
        results[q["id"]] = entry
    return results


def summarise(results):
    scored = {i: r for i, r in results.items() if "first_hit" in r}
    summary = {"questions": len(results), "with_expected_pages": len(scored),
               "leaks": sum(r["technician_restricted"] for r in results.values())}
    for role in ("technician", "engineer"):
        mine = [r for r in scored.values() if r["role"] == role]
        for k in (CHAT_K, K):
            summary[f"{role} hit@{k}"] = f"{sum(r[f'hit@{k}'] for r in mine)}/{len(mine)}"
    summary[f"misses@{CHAT_K}"] = sorted(i for i, r in scored.items() if not r[f"hit@{CHAT_K}"])
    return summary


def compare(baseline, results, tolerance, deployed=None):
    """Every way this run is worse than the baseline, as lines; empty if it passes. A run of other
    questions, or against another deployment, is not comparable, and fails as such."""
    def asked(entries):
        return {i: e.get("asked") for i, e in entries.items()}
    failures = lab.incomparable(asked(baseline["results"]), asked(results),
                                baseline.get("deployed"), deployed)
    if failures:
        return failures
    lost = [i for i, b in baseline["results"].items()
            if b.get(f"hit@{CHAT_K}") and not results[i].get(f"hit@{CHAT_K}")]
    if len(lost) > tolerance:
        failures.append(f"{len(lost)} questions lost their expected page from the top {CHAT_K}, "
                        f"tolerance {tolerance}: {', '.join(lost)}")
    leaks = [i for i, r in results.items() if r["technician_restricted"]]
    if leaks:
        failures.append(f"restricted chunks reached a technician for {', '.join(leaks)}")
    blind = [i for i, r in results.items() if r.get("engineer_restricted") == 0]
    if blind:
        failures.append(f"leak questions found no restricted text even for an engineer, so their "
                        f"check shows nothing: {', '.join(blind)}")
    return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--baseline", type=pathlib.Path, nargs="?", const=BASELINE,
                        help=f"compare with this run, by default {BASELINE.relative_to(lab.ROOT)}")
    parser.add_argument("--tolerance", type=int, default=0,
                        help=f"questions that may lose their hit@{CHAT_K}, set from repeated runs")
    parser.add_argument("--save", type=pathlib.Path, nargs="?", const=BASELINE,
                        help="save this run, by default as the baseline")
    args = parser.parse_args()
    if args.save and args.baseline:
        parser.error("--save and --baseline together would compare the run with itself")
    golden = json.loads(GOLDEN.read_text())

    with lab.tunnel():
        results = evaluate(golden)
    run = {"when": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
           "deployed": lab.deployed(), "summary": summarise(results), "results": results}
    for name, value in run["summary"].items():
        print(f"{name}: {value}")
    if args.save:
        args.save.parent.mkdir(parents=True, exist_ok=True)
        args.save.write_text(json.dumps(run, indent=2) + "\n")
        print(f"saved {args.save}")
    if args.baseline:
        failures = compare(json.loads(args.baseline.read_text()), results, args.tolerance,
                           run["deployed"])
        for line in failures:
            print(f"FAIL {line}")
        if failures:
            sys.exit(1)
        print(f"ok   no question lost its hit@{CHAT_K} beyond tolerance {args.tolerance},"
              " and nothing leaked")


if __name__ == "__main__":
    main()
