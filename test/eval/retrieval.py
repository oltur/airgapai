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

With --candidate, the questions go instead to an index built beside the deployed one, in a
database of its own, and a copy of the retrieval service reading it: "deployed" for the deployed
ingest image, the control, or a name in test/eval/candidates with a .max-chars file, such as
"worse-chunking", for an ingest image built with that chunk size. The deployment is still
compared with the baseline's, except for the ingest image and the chunk counts a candidate
changes, so its chunking is the only difference measured. The control must match the baseline in
those too.

Exit status: 0 if the run passed, 1 if its results were worse or something leaked, 2 for anything
else, such as other questions, a changed deployment, the wrong candidate or a crash.

The search uses the question, not the query the model would write, so this measures the index and
the embeddings, not the model's searching.
"""
import argparse
import copy
import datetime
import hashlib
import json
import pathlib
import re
import sys
import traceback

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import lab  # noqa: E402

GOLDEN = lab.ROOT / "test/eval/golden.json"
BASELINE = lab.ROOT / "test/eval/baseline/retrieval.json"
CANDIDATES = lab.ROOT / "test/eval/candidates"
CANDIDATE_NAME = re.compile(r"[a-z0-9-]+")
CANDIDATE_SCRIPTS = ["test/lib/gateway-copy.sh", "test/eval/retrieval-candidate.sh"]
CANDIDATE_PORT = 18192
CANDIDATE_INGEST = "10.43.0.50:5000/ingest-candidate@sha256:"
K = 5
CHAT_K = 3
LEAK_K = 20


def evaluate(golden, port=lab.PORT):
    prefix = golden["prefix"]
    results = {}

    def search(query, role, k):
        return lab.search(query, role, k, port=port)
    for q in golden["questions"]:
        top = search(q["question"], q["role"], K)
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
        leaked = [r for r in search(q["question"], "technician", LEAK_K)
                  if r["classification"] != "public"]
        entry["technician_restricted"] = len(leaked)
        if q["kind"] == "leak":  # The control: the question does find restricted text for a reader.
            found = search(q["question"], "engineer", LEAK_K)
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


def gate(baseline, results, tolerance, deployed=None):
    """How this run is worse than the baseline: the lines that make the comparison mean something
    else, and the lines for its results, kept apart for the exit status."""
    def asked(entries):
        return {i: e.get("asked") for i, e in entries.items()}
    other = lab.incomparable(asked(baseline["results"]), asked(results))
    if other:
        return other, []
    other = lab.redeployed(baseline.get("deployed"), deployed)
    worse = []
    lost = [i for i, b in baseline["results"].items()
            if b.get(f"hit@{CHAT_K}") and not results[i].get(f"hit@{CHAT_K}")]
    if len(lost) > tolerance:
        worse.append(f"{len(lost)} questions lost their expected page from the top {CHAT_K}, "
                     f"tolerance {tolerance}: {', '.join(lost)}")
    leaks = [i for i, r in results.items() if r["technician_restricted"]]
    if leaks:
        worse.append(f"restricted chunks reached a technician for {', '.join(leaks)}")
    blind = [i for i, r in results.items() if r.get("engineer_restricted") == 0]
    if blind:
        worse.append(f"leak questions found no restricted text even for an engineer, so their "
                     f"check shows nothing: {', '.join(blind)}")
    return other, worse


def same_top(baseline, results):
    """How many questions found the same chunks, in the same order, as in the baseline. Not gated:
    an index built again need not come out the same, and chunks of another size never do."""
    return sum(b.get("top") == results.get(i, {}).get("top")
               for i, b in baseline["results"].items())


def compare(baseline, results, tolerance, deployed=None):
    """Every way this run is worse than the baseline, as lines; empty if it passes. A run of other
    questions, or against another deployment, is not comparable, and fails as such."""
    other, worse = gate(baseline, results, tolerance, deployed)
    return other + worse


def candidates():
    """The candidates that can be asked for: the control, and each chunk size in
    test/eval/candidates."""
    return ["deployed"] + sorted(p.stem for p in CANDIDATES.glob("*.max-chars")
                                 if CANDIDATE_NAME.fullmatch(p.stem))


def candidate_env(candidate):
    """What retrieval-candidate.sh needs to set a candidate up: its name and, for a chunk size, the
    hash of its file, which the image must carry as its label."""
    env = {"ACTION": "up", "CANDIDATE": candidate}
    if candidate != "deployed":
        env["CHUNKING_SHA256"] = hashlib.sha256(
            (CANDIDATES / f"{candidate}.max-chars").read_bytes()).hexdigest()
    return env


def candidate_deployed(deployed, setup):
    """What a candidate's run measured: the deployed embedding server, and the copies' images and
    chunk counts, as setup reported them."""
    return {"images": {"retrieval": [setup.get("retrieval")], "ingest": [setup.get("ingest")],
                       "embed": deployed["images"]["embed"]},
            "rows": setup.get("rows", "").split()}


def candidate_expected(baseline_deployed, candidate, measured):
    """The deployment a candidate's run must match: the baseline's, except that a chunking
    candidate brings its own ingest image and chunk counts. The control brings neither. Returns it,
    and why the candidate is wrong, if it is."""
    expected = copy.deepcopy(baseline_deployed)
    if candidate == "deployed":
        return expected, None
    ingest = measured["images"]["ingest"][0] or ""
    if not ingest.startswith(CANDIDATE_INGEST):
        return expected, (f"the candidate {candidate} ran {ingest or 'no ingest image'}, "
                          f"not an image from {CANDIDATE_INGEST}")
    expected["images"]["ingest"], expected["rows"] = [ingest], measured["rows"]
    return expected, None


def set_up(candidate):
    """Sets up the candidate's index in the VM, and returns what it reported."""
    setup = {}
    for line in lab.in_vm(CANDIDATE_SCRIPTS, candidate_env(candidate)):
        kind, _, rest = line.partition(" ")
        if kind in ("ingest", "retrieval", "rows"):
            setup[kind] = rest
        print(f"  {line}", flush=True)
    return setup


def tear_down():
    for line in lab.in_vm(CANDIDATE_SCRIPTS, {"ACTION": "down"}):
        print(f"  {line}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--baseline", type=pathlib.Path, nargs="?", const=BASELINE,
                        help=f"compare with this run, by default {BASELINE.relative_to(lab.ROOT)}")
    parser.add_argument("--tolerance", type=int, default=0,
                        help=f"questions that may lose their hit@{CHAT_K}, set from repeated runs")
    parser.add_argument("--save", type=pathlib.Path, nargs="?", const=BASELINE,
                        help="save this run, by default as the baseline")
    parser.add_argument("--candidate", choices=candidates(),
                        help="ask an index built beside the deployed one: deployed, the "
                             "control, or a chunk size in test/eval/candidates")
    args = parser.parse_args()
    if args.save and args.baseline:
        parser.error("--save and --baseline together would compare the run with itself")
    if args.save and args.candidate:
        parser.error("a baseline is saved from the deployed index, not a candidate's")
    golden = json.loads(GOLDEN.read_text())

    wrong = None
    if args.candidate:
        try:
            setup = set_up(args.candidate)
            with lab.tunnel(CANDIDATE_PORT, "deployment/retrieval-candidate"):
                results = evaluate(golden, CANDIDATE_PORT)
            deployed = candidate_deployed(lab.deployed(), setup)
        finally:
            tear_down()
        print(f"candidate {args.candidate}: ingest {setup.get('ingest')}")
    else:
        with lab.tunnel():
            results = evaluate(golden)
        deployed = lab.deployed()
    run = {"when": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
           "deployed": deployed, "summary": summarise(results), "results": results}
    for name, value in run["summary"].items():
        print(f"{name}: {value}")
    if args.save:
        args.save.parent.mkdir(parents=True, exist_ok=True)
        args.save.write_text(json.dumps(run, indent=2) + "\n")
        print(f"saved {args.save}")
    if args.baseline:
        baseline = json.loads(args.baseline.read_text())
        if args.candidate:
            baseline["deployed"], wrong = candidate_expected(baseline.get("deployed"),
                                                             args.candidate, deployed)
        print(f"top {K} the same as the baseline's: {same_top(baseline, results)} of "
              f"{len(results)} questions")
        other, worse = gate(baseline, results, args.tolerance, run["deployed"])
        if wrong:
            other = [wrong] + other
        for line in other + worse:
            print(f"FAIL {line}")
        if other:
            sys.exit(2)
        if worse:
            sys.exit(1)
        print(f"ok   no question lost its hit@{CHAT_K} beyond tolerance {args.tolerance},"
              " and nothing leaked")


if __name__ == "__main__":
    try:
        main()
    except Exception:  # A crash is not a run whose results were worse.
        traceback.print_exc()
        sys.exit(2)
